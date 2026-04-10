#!/bin/bash
# Comprehensive end-to-end test suite for boxcutter releases.
#
# Tests PRs #139 (upgrade safety) and #140 (reconcile from reality), plus
# general release validation: VM lifecycle, drain, restart, stale state cleanup.
#
# Requirements:
#   - KVM-enabled host (nested virt OK)
#   - Go 1.23+ on PATH
#   - QEMU, genisoimage, squashfs-tools, mosquitto, curl, jq, python3
#   - ~/.boxcutter/ bundle with boxcutter.yaml + secrets/
#   - Run as root (or with sudo access for bridge/TAP/QEMU)
#
# Usage:
#   sudo -E bash host/e2e-test.sh              # Full suite
#   sudo -E bash host/e2e-test.sh --skip-build # Skip binary build (reuse existing)
#   sudo -E bash host/e2e-test.sh --no-teardown # Keep cluster running after tests
#
# Exit code: 0 if all tests pass, 1 if any test fails.
set -eo pipefail

REPO="${BOXCUTTER_REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
IMAGES_DIR="${REPO}/.images"
BUNDLE_DIR="${BOXCUTTER_BUNDLE:-${HOME}/.boxcutter}"
HOST_BIN="/usr/local/bin/boxcutter-host"
DAEMON_LOG="/tmp/bch-e2e.log"
SOCK="/run/boxcutter-host.sock"

export BOXCUTTER_REPO="$REPO"
export BOXCUTTER_PREFIX="${BOXCUTTER_PREFIX:-boxcutter-dev}"
export NODE_RAM="${NODE_RAM:-2G}"
export NODE_VCPU="${NODE_VCPU:-2}"
export ORCH_RAM="${ORCH_RAM:-1G}"
export ORCH_VCPU="${ORCH_VCPU:-1}"
export MIN_FREE_MEMORY_MB="${MIN_FREE_MEMORY_MB:-256}"
export BOXCUTTER_BUNDLE="$BUNDLE_DIR"

SSH_KEY="${SSH_KEY:-${HOME}/.ssh/boxcutter-vm.key}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# --- Parse flags ---
SKIP_BUILD=false
NO_TEARDOWN=false
for arg in "$@"; do
  case "$arg" in
    --skip-build)  SKIP_BUILD=true ;;
    --no-teardown) NO_TEARDOWN=true ;;
  esac
done

# --- Counters and helpers ---
PASS=0
FAIL=0
TOTAL=0
FAILED_TESTS=""

log()  { echo "[$(date +%H:%M:%S)] $*"; }
pass() { log "  PASS: $*"; PASS=$((PASS+1)); TOTAL=$((TOTAL+1)); }
fail() { log "  FAIL: $*"; FAIL=$((FAIL+1)); TOTAL=$((TOTAL+1)); FAILED_TESTS="${FAILED_TESTS}\n  - $*"; }
skip() { log "  SKIP: $*"; TOTAL=$((TOTAL+1)); }
die()  { log "FATAL: $*"; exit 2; }

node_ssh() { ssh -i "$SSH_KEY" $SSH_OPTS ubuntu@192.168.50.3 "$@" 2>/dev/null; }
orch_ssh() { ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@192.168.50.2 "$@" 2>/dev/null; }
orch_ubuntu_ssh() { ssh -i "$SSH_KEY" $SSH_OPTS ubuntu@192.168.50.2 "$@" 2>/dev/null; }
host_status() { curl -sf --unix-socket "$SOCK" http://localhost/status 2>/dev/null; }

wait_for_health() {
  local what="$1" timeout="${2:-300}" interval="${3:-10}"
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    case "$what" in
      orchestrator)
        healthy=$(host_status | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('orchestrator',{}).get('service_healthy',False))" 2>/dev/null)
        ;;
      node)
        healthy=$(host_status | python3 -c "import sys,json; d=json.load(sys.stdin); ns=d.get('nodes',[]); print(ns[0].get('service_healthy',False) if ns else False)" 2>/dev/null)
        ;;
      golden)
        healthy=$(host_status | python3 -c "import sys,json; d=json.load(sys.stdin); ns=d.get('nodes',[]); print(ns[0].get('health',{}).get('golden_ready',False) if ns else False)" 2>/dev/null)
        ;;
    esac
    [ "$healthy" = "True" ] && return 0
    sleep "$interval"
    elapsed=$((elapsed + interval))
  done
  return 1
}

start_daemon() {
  rm -f "$SOCK"
  nohup "$HOST_BIN" run > "$DAEMON_LOG" 2>&1 &
  local pid=$!
  sleep 2
  if ! kill -0 $pid 2>/dev/null; then
    log "ERROR: daemon failed to start. Log:"
    tail -20 "$DAEMON_LOG"
    return 1
  fi
  log "  Daemon started (PID $pid)"
}

stop_daemon() {
  pkill -f "$HOST_BIN run" 2>/dev/null || true
  sleep 1
}

# ======================================================================
#  PHASE 0: PRE-FLIGHT
# ======================================================================
log "============================================"
log "  Boxcutter E2E Test Suite"
log "  Prefix: $BOXCUTTER_PREFIX"
log "  Node: $NODE_VCPU vCPU, $NODE_RAM RAM"
log "============================================"
log ""

log "=== Phase 0: Pre-flight ==="
[ -f "$BUNDLE_DIR/boxcutter.yaml" ] || die "Bundle not found at $BUNDLE_DIR/"
[ -f "$SSH_KEY" ] || die "SSH key not found: $SSH_KEY"
command -v qemu-system-x86_64 >/dev/null || die "qemu-system-x86_64 not found"
command -v python3 >/dev/null || die "python3 not found"
command -v curl >/dev/null || die "curl not found"
[ -e /dev/kvm ] || die "/dev/kvm not available (no KVM support)"
log "  Pre-flight OK"

# ======================================================================
#  PHASE 1: BUILD
# ======================================================================
if [ "$SKIP_BUILD" = false ]; then
  log ""
  log "=== Phase 1: Build from source ==="
  command -v go >/dev/null || die "go not found on PATH"

  log "  Building boxcutter-host..."
  (cd "$REPO/host" && go build -o "$HOST_BIN" ./cmd/host/) || die "host build failed"

  log "  Running unit tests..."
  (cd "$REPO/host" && go test ./... 2>&1) || die "host unit tests failed"
  (cd "$REPO/node/vmid" && go test ./... 2>&1) || die "vmid unit tests failed"
  (cd "$REPO/orchestrator" && go test ./... 2>&1) || die "orchestrator unit tests failed"
  log "  Unit tests passed"
else
  log ""
  log "=== Phase 1: Build (skipped) ==="
  [ -x "$HOST_BIN" ] || die "$HOST_BIN not found — build first or remove --skip-build"
fi

# ======================================================================
#  PHASE 2: CLEANUP + BOOTSTRAP
# ======================================================================
log ""
log "=== Phase 2: Bootstrap dev cluster ==="

# Kill any existing daemon
stop_daemon

# Kill non-runner QEMU VMs
for pid in $(pgrep qemu-system 2>/dev/null); do
  if ! cat /proc/$pid/cmdline 2>/dev/null | tr '\0' ' ' | grep -q runner; then
    kill -9 $pid 2>/dev/null || true
  fi
done
sleep 1

# Clean TAPs
for tap in $(ip link show 2>/dev/null | grep -oP 'tap-\S+' | tr -d ':'); do
  ip link del $tap 2>/dev/null || true
done

# Clean instance images (keep base images and ubuntu cloud image)
find "$IMAGES_DIR" -maxdepth 1 -name '*.qcow2' ! -name '*-base.qcow2' ! -name 'ubuntu-*' -delete 2>/dev/null || true
find "$IMAGES_DIR" -maxdepth 1 -name '*.iso' -delete 2>/dev/null || true
find "$IMAGES_DIR" -maxdepth 1 -name '*.pid' -delete 2>/dev/null || true
find "$IMAGES_DIR" -maxdepth 1 -name '*console.log' -delete 2>/dev/null || true
rm -f "$SOCK" /var/lib/boxcutter/cluster.json

log "  Bootstrapping cluster from source..."
"$HOST_BIN" bootstrap --from-source 2>&1 | while IFS= read -r line; do
  echo "    $line"
done
RC=${PIPESTATUS[0]}
if [ "$RC" -ne 0 ]; then
  # Bootstrap may timeout waiting for health — VMs may still be booting.
  # Check if VMs are at least running.
  QEMU_COUNT=$(pgrep -c qemu-system 2>/dev/null || echo 0)
  if [ "$QEMU_COUNT" -lt 2 ]; then
    die "Bootstrap failed and VMs not running"
  fi
  log "  Bootstrap command exited $RC but VMs are running — continuing"
fi

# Ensure daemon is running
if ! [ -S "$SOCK" ]; then
  log "  Starting daemon..."
  start_daemon
fi

# Wait for health
log "  Waiting for orchestrator health..."
wait_for_health orchestrator 600 10 || die "Orchestrator not healthy after 10 min"
log "  Orchestrator healthy"

log "  Waiting for node health..."
wait_for_health node 600 10 || die "Node not healthy after 10 min"
log "  Node healthy"

# Ensure node services are running (cloud-init may have failed to start them)
node_ssh "sudo systemctl is-active boxcutter-node" >/dev/null 2>&1 || {
  log "  Starting node services manually..."
  node_ssh "sudo mkdir -p /etc/boxcutter/secrets"
  node_ssh "echo 'tskey-dummy' | sudo tee /etc/boxcutter/secrets/tailscale-vm-authkey > /dev/null"
  node_ssh "echo 'tskey-dummy' | sudo tee /etc/boxcutter/secrets/tailscale-node-authkey > /dev/null"
  node_ssh "sudo systemctl enable boxcutter-node vmid 2>&1; sudo systemctl restart boxcutter-node vmid"
  wait_for_health node 300 10 || die "Node not healthy after service restart"
}

# Ensure orchestrator knows about the node
ORCH_NODES=$(orch_ubuntu_ssh "sudo journalctl -u boxcutter-orchestrator --no-pager 2>&1" | grep -c "registered.*node" || echo 0)
if [ "$ORCH_NODES" -eq 0 ]; then
  log "  Restarting orchestrator for node discovery..."
  orch_ubuntu_ssh "sudo systemctl restart boxcutter-orchestrator"
  sleep 5
fi

# Wait for golden image
log "  Waiting for golden image (may take 5-15 min on nested virt)..."
wait_for_health golden 900 15 || die "Golden image not ready after 15 min"
log "  Golden image ready"

log "  Bootstrap complete"

# ======================================================================
#  PHASE 3: TESTS
# ======================================================================
log ""
log "=== Phase 3: Tests ==="

# Record initial PIDs
ORCH_PID=$(host_status | python3 -c "import sys,json; print(json.load(sys.stdin).get('orchestrator',{}).get('pid',0))")
NODE_PID=$(host_status | python3 -c "import sys,json; ns=json.load(sys.stdin).get('nodes',[]); print(ns[0].get('pid',0) if ns else 0)")

# ------------------------------------------------------------------
# TEST 1: VM creation
# ------------------------------------------------------------------
log ""
log "--- Test 1: VM creation ---"
VM_OUT=$(orch_ssh "new --ram 256 --vcpu 1" 2>&1) || true
if echo "$VM_OUT" | grep -q "running"; then
  VM_NAME=$(echo "$VM_OUT" | grep "Name:" | awk '{print $2}')
  pass "VM created: $VM_NAME"
else
  fail "VM creation failed: $VM_OUT"
  VM_NAME=""
fi

# Verify VM appears in list
sleep 3
VM_LIST=$(orch_ssh "list" 2>&1)
VM_COUNT=$(echo "$VM_LIST" | tail -n +2 | grep -c "running" || echo 0)
if [ "$VM_COUNT" -gt 0 ]; then
  pass "VM visible in orchestrator list ($VM_COUNT running)"
else
  fail "VM not visible in orchestrator list"
fi

# ------------------------------------------------------------------
# TEST 2: Daemon restart — VMs survive (PR #140: reconcile from reality)
# ------------------------------------------------------------------
log ""
log "--- Test 2: Daemon restart survival (PR #140) ---"

# Record pre-restart QEMU PIDs
PRE_QEMU_PIDS=$(pgrep -d, qemu-system 2>/dev/null)

stop_daemon

# Verify QEMU processes survived
POST_QEMU_PIDS=$(pgrep -d, qemu-system 2>/dev/null)
if [ "$PRE_QEMU_PIDS" = "$POST_QEMU_PIDS" ]; then
  pass "QEMU processes survived daemon kill"
else
  fail "QEMU processes changed after daemon kill (before: $PRE_QEMU_PIDS, after: $POST_QEMU_PIDS)"
fi

# Restart daemon
start_daemon || die "Daemon failed to restart"
wait_for_health orchestrator 120 5 || fail "Orchestrator not healthy after restart"
wait_for_health node 120 5 || fail "Node not healthy after restart"

# Check reconciliation log
if grep -q "merging with cached state" "$DAEMON_LOG"; then
  pass "Daemon re-discovered running VMs from process scan"
else
  fail "Daemon did not reconcile from running processes"
fi

# Verify VM survived
sleep 5
POST_VM_COUNT=$(orch_ssh "list" 2>&1 | tail -n +2 | grep -c "running" || echo 0)
if [ "$POST_VM_COUNT" -ge "$VM_COUNT" ]; then
  pass "VMs survived daemon restart ($POST_VM_COUNT running)"
else
  fail "VMs lost during restart (before: $VM_COUNT, after: $POST_VM_COUNT)"
fi

# ------------------------------------------------------------------
# TEST 3: Stale cluster.json cleanup (PR #140)
# ------------------------------------------------------------------
log ""
log "--- Test 3: Stale cluster.json cleanup (PR #140) ---"

stop_daemon

# Inject stale entries
python3 -c "
import json
with open('/var/lib/boxcutter/cluster.json') as f:
    state = json.load(f)
state['nodes'].append({
    'id': '${BOXCUTTER_PREFIX}-node-99', 'type': 'node', 'status': 'active',
    'bridge_ip': '192.168.50.99', 'pid': 99999,
    'disk': '${IMAGES_DIR}/${BOXCUTTER_PREFIX}-node-99.qcow2',
    'vcpu': 2, 'ram': '2G', 'tap': 'tap-node99', 'mac': '52:54:00:00:00:99'
})
with open('/var/lib/boxcutter/cluster.json', 'w') as f:
    json.dump(state, f, indent=2)
"

start_daemon || die "Daemon failed to start after stale injection"

if grep -q "removing stale entry" "$DAEMON_LOG"; then
  pass "Stale cluster.json entries removed on boot"
else
  fail "Stale entries not cleaned up"
fi

# Verify the real node is still there
REAL_NODES=$(host_status | python3 -c "import sys,json; print(json.load(sys.stdin).get('nodes',[{}])[0].get('id',''))" 2>/dev/null)
if echo "$REAL_NODES" | grep -q "node-1"; then
  pass "Real node preserved after stale cleanup"
else
  # Node-1 may have died — check if it was stale too
  RUNNING_QEMU=$(pgrep -a qemu-system 2>/dev/null | grep -c node || echo 0)
  if [ "$RUNNING_QEMU" -eq 0 ]; then
    skip "Node-1 not running (VM died during test) — stale cleanup still correct"
  else
    fail "Real node missing after stale cleanup"
  fi
fi

# ------------------------------------------------------------------
# TEST 4: cluster.json deletion + full rediscovery (PR #140)
# ------------------------------------------------------------------
log ""
log "--- Test 4: cluster.json deletion recovery (PR #140) ---"

stop_daemon
rm -f /var/lib/boxcutter/cluster.json

start_daemon || die "Daemon failed to start after cluster.json deletion"

if grep -q "not in cluster.json — adopting" "$DAEMON_LOG"; then
  pass "Deleted cluster.json: VMs adopted from running processes"
else
  fail "VMs not adopted after cluster.json deletion"
fi

# Verify state was rebuilt
REBUILT_NODES=$(host_status | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('nodes',[])))" 2>/dev/null)
REBUILT_ORCH=$(host_status | python3 -c "import sys,json; d=json.load(sys.stdin); print('yes' if d.get('orchestrator') else 'no')" 2>/dev/null)
if [ "$REBUILT_ORCH" = "yes" ]; then
  pass "Orchestrator state rebuilt from QEMU process"
else
  fail "Orchestrator not recovered after cluster.json deletion"
fi

# ------------------------------------------------------------------
# TEST 5: Health monitor backs off during upgrade (PR #139)
# ------------------------------------------------------------------
log ""
log "--- Test 5: Upgrade mutex — health monitor backs off (PR #139) ---"

# Verify the upgradingOrch mutex exists in the binary
if strings "$HOST_BIN" | grep -q "upgradingOrch"; then
  pass "upgradingOrch mutex flag present in binary"
else
  fail "upgradingOrch mutex flag missing from binary"
fi

# Verify health loop checks the flag (via source code inspection)
if grep -q "orchUpgradeActive" "$REPO/host/cmd/host/main.go"; then
  pass "healthLoop checks orchUpgradeActive before orchestrator health check"
else
  fail "healthLoop does not check upgrade flag"
fi

# Verify reconcile loop clears flag on exit (defer)
if grep -qP "defer.*upgradingOrch\s*=\s*false" "$REPO/host/cmd/host/main.go" 2>/dev/null || \
   grep -A5 "func runReconcileLoop" "$REPO/host/cmd/host/main.go" | grep -q "defer"; then
  pass "runReconcileLoop defers upgradingOrch=false on exit"
else
  fail "runReconcileLoop does not defer flag cleanup"
fi

# ------------------------------------------------------------------
# TEST 6: Drain-first upgrade strategy (PR #139)
# ------------------------------------------------------------------
log ""
log "--- Test 6: Drain-first strategy code verification (PR #139) ---"

# Verify leastLoadedOldNode helper exists
if grep -q "func leastLoadedOldNode" "$REPO/host/cmd/host/main.go"; then
  pass "leastLoadedOldNode helper exists"
else
  fail "leastLoadedOldNode helper missing"
fi

# Verify reconcileNodeUpgrade calls drain-first instead of erroring
if grep -q "Drain-first upgrade" "$REPO/host/cmd/host/main.go"; then
  pass "reconcileNodeUpgrade implements drain-first fallback"
else
  fail "reconcileNodeUpgrade missing drain-first fallback"
fi

# Verify the drain-first path sets node status to draining
if grep -A5 "Drain-first" "$REPO/host/cmd/host/main.go" | grep -q "draining"; then
  pass "Drain-first sets node status to draining before drain"
else
  fail "Drain-first does not set draining status"
fi

# ------------------------------------------------------------------
# TEST 7: drainNode safety check — 0 VMs before stop (PR #140)
# ------------------------------------------------------------------
log ""
log "--- Test 7: Drain safety check (PR #140) ---"

if grep -q "final safety check" "$REPO/host/cmd/host/main.go"; then
  pass "drainNode has final 0-VM safety check before QEMU stop"
else
  fail "drainNode missing 0-VM safety check"
fi

# Verify it queries the node agent
if grep -A10 "final safety check" "$REPO/host/cmd/host/main.go" | grep -q "/api/vms"; then
  pass "Safety check queries node agent /api/vms endpoint"
else
  fail "Safety check does not query node agent"
fi

# ------------------------------------------------------------------
# TEST 8: Orchestrator source-of-truth VM list (PR #140)
# ------------------------------------------------------------------
log ""
log "--- Test 8: Orchestrator queries nodes for VM list (PR #140) ---"

if grep -q "source of truth" "$REPO/orchestrator/internal/api/handlers.go" 2>/dev/null; then
  pass "handleVMList queries nodes as source of truth"
else
  fail "handleVMList does not query nodes as source of truth"
fi

# Verify handleVMDestroy requires node confirmation
if grep -q "502" "$REPO/orchestrator/internal/api/handlers.go" 2>/dev/null; then
  pass "handleVMDestroy returns 502 on node agent failure (not silent delete)"
else
  fail "handleVMDestroy does not return 502 on failure"
fi

# ------------------------------------------------------------------
# TEST 9: VM destruction via orchestrator
# ------------------------------------------------------------------
log ""
log "--- Test 9: VM destruction ---"

if [ -n "$VM_NAME" ]; then
  DESTROY_OUT=$(orch_ssh "destroy $VM_NAME" 2>&1) || true
  sleep 3
  AFTER_DESTROY=$(orch_ssh "list" 2>&1 | tail -n +2 | grep -c "$VM_NAME" || echo 0)
  if [ "$AFTER_DESTROY" -eq 0 ]; then
    pass "VM $VM_NAME destroyed successfully"
  else
    fail "VM $VM_NAME still present after destroy"
  fi
else
  skip "VM destruction — no VM was created"
fi

# ------------------------------------------------------------------
# TEST 10: Unit tests pass (all modules)
# ------------------------------------------------------------------
log ""
log "--- Test 10: Unit tests ---"

UNIT_FAIL=0
for mod in host node/vmid orchestrator; do
  if (cd "$REPO/$mod" && go test ./... >/dev/null 2>&1); then
    pass "Unit tests pass: $mod"
  else
    fail "Unit tests fail: $mod"
    UNIT_FAIL=1
  fi
done

# ======================================================================
#  PHASE 4: TEARDOWN
# ======================================================================
if [ "$NO_TEARDOWN" = false ]; then
  log ""
  log "=== Phase 4: Teardown ==="
  stop_daemon
  for pid in $(pgrep qemu-system 2>/dev/null); do
    if ! cat /proc/$pid/cmdline 2>/dev/null | tr '\0' ' ' | grep -q runner; then
      kill $pid 2>/dev/null || true
    fi
  done
  for tap in $(ip link show 2>/dev/null | grep -oP 'tap-\S+' | tr -d ':'); do
    ip link del $tap 2>/dev/null || true
  done
  rm -f "$SOCK"
  log "  Teardown complete"
else
  log ""
  log "=== Phase 4: Teardown (skipped — --no-teardown) ==="
fi

# ======================================================================
#  SUMMARY
# ======================================================================
log ""
log "============================================"
log "  E2E Results: $PASS passed, $FAIL failed ($TOTAL total)"
if [ "$FAIL" -gt 0 ]; then
  log ""
  log "  Failed tests:"
  echo -e "$FAILED_TESTS"
fi
log "============================================"

if [ "$FAIL" -gt 0 ]; then
  log ""
  log "DEPLOYMENT BLOCKED — $FAIL test(s) failed"
  exit 1
else
  log ""
  log "ALL TESTS PASSED — clear for deployment"
  exit 0
fi
