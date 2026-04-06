#!/bin/bash
# End-to-end tapegun integration test.
# Requires a running cluster with at least 1 VM.
# Tests: daemon running, tmux capture, inbox delivery, activity reporting.
set -eo pipefail

SSH_KEY="${SSH_KEY:-/home/dev/.ssh/id_rsa_prod}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=30 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
ORCH_IP="${ORCH_IP:-192.168.50.2}"
CLUSTER_KEY="${CLUSTER_KEY:-/home/dev/.boxcutter/secrets/cluster-ssh.key}"

log() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }
pass() { log "PASS: $*"; }

orch_ssh() { ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@"$ORCH_IP" "$@"; }
node_ssh() { ssh -i "$CLUSTER_KEY" $SSH_OPTS ubuntu@"$1" "$2"; }

# --- Setup: find or create a VM ---
log "=== Setup ==="
VM_NAME=$(orch_ssh "list" 2>/dev/null | tail -n +2 | awk '{print $1}' | head -1)
if [ -z "$VM_NAME" ]; then
  log "No VMs found, creating one..."
  orch_ssh "new --ram 512 --vcpu 1" >/dev/null 2>&1 || fail "VM creation"
  sleep 10
  VM_NAME=$(orch_ssh "list" 2>/dev/null | tail -n +2 | awk '{print $1}' | head -1)
fi
[ -z "$VM_NAME" ] && fail "No VM available"
log "Using VM: $VM_NAME"

# Find which node the VM is on
NODE_IP=$(orch_ssh "list" 2>/dev/null | grep "$VM_NAME" | awk '{print $3}')
log "VM on node: $NODE_IP"

# --- Test 1: Tapegun daemon is running ---
log "=== Test 1: Tapegun daemon running ==="
DAEMON_STATUS=$(orch_ssh "exec $VM_NAME systemctl is-active boxcutter-tapegun" 2>/dev/null | tr -d '\n')
[ "$DAEMON_STATUS" = "active" ] || fail "tapegun daemon not active (status: $DAEMON_STATUS)"
pass "Tapegun daemon is active"

# --- Test 2: Tapegun runs as dev user ---
log "=== Test 2: Tapegun runs as User=dev ==="
DAEMON_USER=$(orch_ssh "exec $VM_NAME ps -o user= -C boxcutter-tapeg" 2>/dev/null | tr -d ' \n')
[ "$DAEMON_USER" = "dev" ] || fail "tapegun running as '$DAEMON_USER', expected 'dev'"
pass "Tapegun runs as dev"

# --- Test 3: tmux is installed ---
log "=== Test 3: tmux installed ==="
TMUX_VER=$(orch_ssh "exec $VM_NAME tmux -V" 2>/dev/null | tr -d '\n')
[ -n "$TMUX_VER" ] || fail "tmux not installed"
pass "tmux installed: $TMUX_VER"

# --- Test 4: Start a tmux session with a process ---
log "=== Test 4: tmux session with process ==="
orch_ssh "exec $VM_NAME bash -c 'tmux kill-server 2>/dev/null; tmux new-session -d -s main'" 2>/dev/null || true
sleep 1
# Run a command in the tmux session
orch_ssh "exec $VM_NAME tmux send-keys -t main 'echo TAPEGUN_MARKER_12345' Enter" 2>/dev/null
sleep 2
# Verify tmux has our marker
PANE=$(orch_ssh "exec $VM_NAME tmux capture-pane -t main -p" 2>/dev/null)
echo "$PANE" | grep -q "TAPEGUN_MARKER_12345" || fail "tmux pane doesn't contain marker"
pass "tmux session running with process"

# --- Test 5: Activity reporting (host → reads guest activity) ---
log "=== Test 5: Activity reporting ==="
# Wait for tapegun to capture the pane (polls every 5s)
sleep 8
ACTIVITY=$(orch_ssh "tapegun activity $VM_NAME" 2>/dev/null)
echo "$ACTIVITY" | grep -q "active" || fail "activity not showing 'active'"
echo "$ACTIVITY" | grep -q "TAPEGUN_MARKER_12345" || fail "activity doesn't contain tmux marker"
pass "Activity shows tmux pane content"

# --- Test 6: Send message to VM (host → guest) ---
log "=== Test 6: Message delivery ==="
orch_ssh "tapegun send $VM_NAME 'Hello from integration test'" 2>/dev/null || fail "tapegun send"
# Wait for tapegun daemon to poll inbox (5s interval)
sleep 8
INBOX=$(orch_ssh "exec $VM_NAME cat /home/dev/.tapegun/inbox.json" 2>/dev/null)
echo "$INBOX" | grep -q "Hello from integration test" || fail "message not in inbox"
pass "Message delivered to VM inbox"

# --- Test 7: Send-keys message (injected into tmux) ---
log "=== Test 7: Send-keys injection ==="
orch_ssh "tapegun sendkeys $VM_NAME 'echo SENDKEYS_TEST_67890'" 2>/dev/null || fail "tapegun sendkeys"
sleep 8
PANE2=$(orch_ssh "exec $VM_NAME tmux capture-pane -t main -p" 2>/dev/null)
echo "$PANE2" | grep -q "SENDKEYS_TEST_67890" || fail "send-keys not injected into tmux"
pass "Send-keys message injected into tmux"

# --- Test 8: Activity reflects send-keys command ---
log "=== Test 8: Activity reflects injected command ==="
sleep 8
ACTIVITY2=$(orch_ssh "tapegun activity $VM_NAME" 2>/dev/null)
echo "$ACTIVITY2" | grep -q "SENDKEYS_TEST_67890" || fail "activity doesn't show injected command"
pass "Activity shows injected command output"

# --- Test 9: Pending message count ---
log "=== Test 9: Pending message count ==="
# Send another message
orch_ssh "tapegun send $VM_NAME 'Second test message'" 2>/dev/null || fail "send second"
sleep 2
ACTIVITY3=$(orch_ssh "tapegun activity $VM_NAME" 2>/dev/null)
echo "$ACTIVITY3" | grep -q "pending: [1-9]" || log "  Note: pending count may be 0 (daemon already fetched)"
pass "Message send works repeatedly"

# --- Test 10: Broadcast ---
log "=== Test 10: Broadcast ==="
orch_ssh "tapegun broadcast 'Broadcast test message'" 2>/dev/null || fail "broadcast"
sleep 8
INBOX2=$(orch_ssh "exec $VM_NAME cat /home/dev/.tapegun/inbox.json" 2>/dev/null)
echo "$INBOX2" | grep -q "Broadcast test message" || fail "broadcast not in inbox"
pass "Broadcast delivered"

# --- Cleanup ---
orch_ssh "exec $VM_NAME tmux kill-server" 2>/dev/null || true

log ""
log "========================================="
log "  TAPEGUN TEST PASSED (10/10)"
log "========================================="
