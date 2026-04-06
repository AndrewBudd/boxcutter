#!/bin/bash
# End-to-end upgrade test for boxcutter dev cluster.
# Provisions a cluster, creates VMs, upgrades to new images, verifies.
# Must complete with ZERO manual intervention.
set -eo pipefail

REPO="/home/dev/project"
IMAGES_DIR="${REPO}/.images"
BUNDLE_DIR="/home/dev/.boxcutter"
CLUSTER_KEY="${BUNDLE_DIR}/secrets/cluster-ssh.key"
SSH_KEY="/home/dev/.ssh/id_rsa_prod"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=30 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

export BOXCUTTER_REPO="$REPO"
export BOXCUTTER_PREFIX="boxcutter-dev"
export NODE_RAM="2G"
export NODE_VCPU="2"
export ORCH_RAM="1G"
export ORCH_VCPU="1"
export MIN_FREE_MEMORY_MB="512"
export BOXCUTTER_BUNDLE="$BUNDLE_DIR"

log() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }

# --- Phase 1: Provision cluster ---
log "=== Phase 1: Provision cluster ==="

log "Provisioning orchestrator..."
sudo BOXCUTTER_REPO="$REPO" BOXCUTTER_BUNDLE="$BUNDLE_DIR" BOXCUTTER_PREFIX="$BOXCUTTER_PREFIX" \
  ORCH_RAM="$ORCH_RAM" ORCH_VCPU="$ORCH_VCPU" \
  bash host/provision.sh orchestrator --from-image || fail "orchestrator provision"

log "Provisioning node..."
sudo BOXCUTTER_REPO="$REPO" BOXCUTTER_BUNDLE="$BUNDLE_DIR" BOXCUTTER_PREFIX="$BOXCUTTER_PREFIX" \
  NODE_RAM="$NODE_RAM" NODE_VCPU="$NODE_VCPU" \
  bash host/provision.sh node boxcutter-dev-node-1 --from-image || fail "node provision"

# Write cluster.json
NODE_DIGEST=$(cat "${IMAGES_DIR}/.node-digest" 2>/dev/null || echo "")
ORCH_DIGEST=$(cat "${IMAGES_DIR}/.orch-digest" 2>/dev/null || echo "")
sudo python3 -c "
import json
state = {
    'orchestrator': {
        'id': 'orchestrator', 'type': 'orchestrator',
        'bridge_ip': '192.168.50.2',
        'disk': '${IMAGES_DIR}/orchestrator.qcow2',
        'iso': '${IMAGES_DIR}/orchestrator-cloud-init.iso',
        'vcpu': ${ORCH_VCPU}, 'ram': '${ORCH_RAM}',
        'tap': 'tap-orch', 'mac': '52:54:00:00:00:02'
    },
    'nodes': [{
        'id': 'boxcutter-dev-node-1', 'type': 'node', 'status': 'active',
        'bridge_ip': '192.168.50.3',
        'disk': '${IMAGES_DIR}/boxcutter-dev-node-1.qcow2',
        'iso': '${IMAGES_DIR}/boxcutter-dev-node-1-cloud-init.iso',
        'vcpu': ${NODE_VCPU}, 'ram': '${NODE_RAM}',
        'tap': 'tap-node1', 'mac': '52:54:00:00:00:03'
    }]
}
with open('/var/lib/boxcutter/cluster.json', 'w') as f:
    json.dump(state, f, indent=2)
"
log "Cluster state written"

# --- Phase 2: Boot cluster ---
log "=== Phase 2: Boot cluster ==="

sudo rm -f /run/boxcutter-host.sock
sudo bash -c "$(cat <<'DAEMONEOF'
export BOXCUTTER_REPO="/home/dev/project"
export BOXCUTTER_PREFIX="boxcutter-dev"
export NODE_RAM="2G" NODE_VCPU="2" ORCH_RAM="1G" ORCH_VCPU="1" MIN_FREE_MEMORY_MB="512"
nohup /usr/local/bin/boxcutter-host run > /tmp/bch-test.log 2>&1 &
DAEMONEOF
)"
sleep 10

# Verify daemon started
[ -S /run/boxcutter-host.sock ] || fail "daemon socket not found"
log "Daemon started"

# Wait for orchestrator healthy
log "Waiting for orchestrator..."
for i in $(seq 1 30); do
  healthy=$(curl -sf --unix-socket /run/boxcutter-host.sock http://localhost/status 2>/dev/null | \
    python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('orchestrator',{}).get('service_healthy',False))" 2>/dev/null)
  [ "$healthy" = "True" ] && break
  sleep 10
done
[ "$healthy" = "True" ] || fail "orchestrator not healthy after 5 min"
log "Orchestrator healthy"

# Wait for node healthy
log "Waiting for node..."
for i in $(seq 1 30); do
  healthy=$(curl -sf --unix-socket /run/boxcutter-host.sock http://localhost/status 2>/dev/null | \
    python3 -c "import sys,json; d=json.load(sys.stdin); ns=d.get('nodes',[]); print(ns[0].get('service_healthy',False) if ns else False)" 2>/dev/null)
  [ "$healthy" = "True" ] && break
  sleep 10
done
[ "$healthy" = "True" ] || fail "node not healthy after 5 min"
log "Node healthy"

# --- Phase 3: Build golden image and create VM ---
log "=== Phase 3: Create VM ==="

# Wait for SSH to be ready on orchestrator (cloud-init may still be running)
log "Waiting for orchestrator SSH..."
for i in $(seq 1 30); do
  ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "help" >/dev/null 2>&1 && break
  sleep 10
done

# Trigger golden build
log "Triggering golden image build..."
ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "golden set-head build" || fail "golden set-head"

# Wait for golden image
log "Waiting for golden image (this takes ~5 min on nested virt)..."
for i in $(seq 1 60); do
  ready=$(curl -sf --unix-socket /run/boxcutter-host.sock http://localhost/status 2>/dev/null | \
    python3 -c "import sys,json; d=json.load(sys.stdin); ns=d.get('nodes',[]); print(ns[0].get('health',{}).get('golden_ready',False) if ns else False)" 2>/dev/null)
  [ "$ready" = "True" ] && break
  [ $((i % 6)) -eq 0 ] && log "  Still building golden... ($((i*10))s)"
  sleep 10
done
[ "$ready" = "True" ] || fail "golden image not ready after 10 min"
log "Golden image ready"

# Create a VM
log "Creating VM..."
ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "new --ram 512 --vcpu 1" || fail "VM creation"
sleep 5

# Verify VM exists
VM_COUNT=$(ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "list" 2>/dev/null | grep -c "running" || echo 0)
[ "$VM_COUNT" -gt 0 ] || fail "no running VMs found"
log "VM created and running (${VM_COUNT} VM(s))"

# --- Phase 4: Upgrade ---
log "=== Phase 4: Rolling upgrade ==="

log "Starting upgrade..."
sudo bash -c "$(cat <<'UPGRADEEOF'
export BOXCUTTER_REPO="/home/dev/project"
export BOXCUTTER_PREFIX="boxcutter-dev"
export NODE_RAM="2G" NODE_VCPU="2" ORCH_RAM="1G" ORCH_VCPU="1" MIN_FREE_MEMORY_MB="512"
/usr/local/bin/boxcutter-host upgrade all 2>&1
UPGRADEEOF
)" &
UPGRADE_PID=$!

# Monitor upgrade progress
while kill -0 $UPGRADE_PID 2>/dev/null; do
  status=$(tail -1 /tmp/bch-test.log 2>/dev/null | grep -oP '\[reconcile\].*' || echo "waiting...")
  [ -n "$status" ] && log "  $status"
  sleep 30
done

wait $UPGRADE_PID
UPGRADE_EXIT=$?
[ $UPGRADE_EXIT -eq 0 ] || fail "upgrade exited with code $UPGRADE_EXIT"
log "Upgrade command completed successfully"

# --- Phase 5: Verify ---
log "=== Phase 5: Verify ==="

# Wait for everything to settle after upgrade
log "Waiting for cluster to stabilize..."
sleep 30

# Wait for orchestrator to be healthy
for i in $(seq 1 30); do
  healthy=$(curl -sf --unix-socket /run/boxcutter-host.sock http://localhost/status 2>/dev/null | \
    python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('orchestrator',{}).get('service_healthy',False))" 2>/dev/null)
  [ "$healthy" = "True" ] && break
  sleep 10
done

# Check cluster health
log "Checking cluster health..."
curl -sf --unix-socket /run/boxcutter-host.sock http://localhost/status | \
  python3 -c "
import sys,json
d = json.load(sys.stdin)
orch = d.get('orchestrator',{})
print(f'Orchestrator: healthy={orch.get(\"service_healthy\")}')
for n in d.get('nodes',[]):
    h = n.get('health',{})
    print(f'Node {n[\"id\"]}: healthy={n.get(\"service_healthy\")} vms={h.get(\"vms_running\",0)} golden={h.get(\"golden_ready\",False)}')
"

# Verify VM survived migration
VM_COUNT=$(ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "list" 2>/dev/null | grep -c "running" || echo 0)
[ "$VM_COUNT" -gt 0 ] || fail "VMs lost during upgrade!"
log "VMs survived upgrade: ${VM_COUNT} running"

ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "list"

log ""
log "========================================="
log "  UPGRADE TEST PASSED"
log "========================================="
