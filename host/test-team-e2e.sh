#!/bin/bash
# End-to-end integration test for team YAML apply/scale/destroy.
# Requires a running cluster with orchestrator + at least 1 node.
#
# Tests:
#   1.  team apply creates VMs with correct names
#   2.  team list shows the team
#   3.  team status shows VM status
#   4.  metadata endpoint serves correct agent-config per VM
#   5.  persona files installed correctly on VMs
#   6.  Claude Code running (tapegun active with tmux session)
#   7.  tapegun inbox messaging works (plugin installed)
#   8.  re-apply is idempotent (no-op)
#   9.  scale-up creates additional VMs
#   10. scale-down destroys excess VMs
#   11. crash recovery: kill Claude Code -> tapegun restarts it
#   12. team destroy removes all VMs
#   13. invalid YAML produces clear error
#   14. existing VMs unaffected
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SSH_KEY="${SSH_KEY:-/home/dev/.ssh/id_rsa_prod}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=30 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
ORCH_IP="${ORCH_IP:-192.168.50.2}"
TIMEOUT="${TIMEOUT:-300}"

PASS=0
FAIL=0
TOTAL=0

log()  { echo "[$(date +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; PASS=$((PASS+1)); TOTAL=$((TOTAL+1)); }
fail() { log "FAIL: $*"; FAIL=$((FAIL+1)); TOTAL=$((TOTAL+1)); }
skip() { log "SKIP: $*"; TOTAL=$((TOTAL+1)); }

orch_ssh() { ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@"$ORCH_IP" "$@" 2>/dev/null; }

# wait_for_vm_ready polls until a VM's tapegun is reporting activity.
# Usage: wait_for_vm_ready <vm_name> <timeout_seconds>
wait_for_vm_ready() {
  local vm="$1" timeout="${2:-180}"
  log "  Waiting for $vm to be ready (up to ${timeout}s)..."
  for i in $(seq 1 "$((timeout / 5))"); do
    local activity
    activity=$(orch_ssh "tapegun activity $vm" 2>&1 || true)
    if echo "$activity" | grep -qi "active\|idle\|pane_content"; then
      log "  $vm is ready (${i}x5s)"
      return 0
    fi
    [ $((i % 6)) -eq 0 ] && log "  Still waiting for $vm... ($((i*5))s)"
    sleep 5
  done
  log "  WARNING: $vm did not become ready within ${timeout}s"
  return 1
}

# --- Pre-flight: verify orchestrator is reachable ---
log "=== Pre-flight ==="
if ! orch_ssh "status" >/dev/null 2>&1; then
  log "ERROR: Cannot reach orchestrator at $ORCH_IP"
  log "Set ORCH_IP and SSH_KEY environment variables"
  exit 1
fi
log "Orchestrator reachable"

# Count existing VMs to verify we don't break them
EXISTING_VMS=$(orch_ssh "list" 2>/dev/null | tail -n +2 | wc -l)
log "Existing VMs: $EXISTING_VMS"

# --- Cleanup: destroy any leftover test-team VMs ---
log "=== Cleanup (pre-test) ==="
orch_ssh "team destroy test-team" >/dev/null 2>&1 || true
sleep 2

# --- Test 1: team apply creates VMs ---
log "=== Test 1: team apply ==="
APPLY_OUT=$(cat "$SCRIPT_DIR/test-team-e2e.yaml" | orch_ssh "team apply -f -" 2>&1)
echo "$APPLY_OUT"

if echo "$APPLY_OUT" | grep -q "3 created"; then
  pass "team apply created 3 VMs"
else
  fail "team apply did not create 3 VMs"
fi

# Wait for VMs to appear in list
sleep 5

# --- Test 2: team list shows the team ---
log "=== Test 2: team list ==="
LIST_OUT=$(orch_ssh "team list" 2>&1)
echo "$LIST_OUT"

if echo "$LIST_OUT" | grep -q "test-team"; then
  pass "team list shows test-team"
else
  fail "team list missing test-team"
fi

# --- Test 3: team status shows VMs ---
log "=== Test 3: team status ==="
STATUS_OUT=$(orch_ssh "team status test-team" 2>&1)
echo "$STATUS_OUT"

for VM_NAME in test-team-lead-1 test-team-worker-1 test-team-worker-2; do
  if echo "$STATUS_OUT" | grep -q "$VM_NAME"; then
    pass "team status shows $VM_NAME"
  else
    fail "team status missing $VM_NAME"
  fi
done

# --- Wait for VMs to boot and tapegun to set up ---
log "=== Waiting for VMs to be ready ==="
LEAD_READY=false
WORKER_READY=false
if wait_for_vm_ready "test-team-lead-1" 180; then
  LEAD_READY=true
fi
if wait_for_vm_ready "test-team-worker-1" 180; then
  WORKER_READY=true
fi

# --- Test 4: metadata endpoint serves correct agent-config ---
log "=== Test 4: metadata agent-config ==="
if [ "$LEAD_READY" = "true" ]; then
  LEAD_CONFIG=$(orch_ssh "exec test-team-lead-1 curl -sf http://169.254.169.254/metadata/agent-config" 2>&1 || true)
  echo "  lead agent-config: $LEAD_CONFIG"

  if echo "$LEAD_CONFIG" | grep -q '"team".*:.*"test-team"'; then
    pass "lead agent-config has team=test-team"
  else
    fail "lead agent-config missing team field"
  fi

  if echo "$LEAD_CONFIG" | grep -q '"agent".*:.*"lead"'; then
    pass "lead agent-config has agent=lead"
  else
    fail "lead agent-config missing agent field"
  fi

  if echo "$LEAD_CONFIG" | grep -q '"role".*:.*"test-lead"'; then
    pass "lead agent-config has persona role=test-lead"
  else
    fail "lead agent-config missing persona role"
  fi
else
  skip "lead VM not ready — cannot test metadata endpoint"
  skip "lead agent-config team field"
  skip "lead agent-config persona role"
fi

if [ "$WORKER_READY" = "true" ]; then
  WORKER_CONFIG=$(orch_ssh "exec test-team-worker-1 curl -sf http://169.254.169.254/metadata/agent-config" 2>&1 || true)
  if echo "$WORKER_CONFIG" | grep -q '"agent".*:.*"worker"'; then
    pass "worker agent-config has agent=worker"
  else
    fail "worker agent-config missing agent field"
  fi
else
  skip "worker VM not ready — cannot test metadata endpoint"
fi

# --- Test 5: persona files installed correctly ---
log "=== Test 5: persona files ==="
if [ "$LEAD_READY" = "true" ]; then
  # Lead should have eng-manager.md persona
  LEAD_PERSONAS=$(orch_ssh "exec test-team-lead-1 ls .claude/personas/ 2>/dev/null" 2>&1 || true)
  echo "  lead personas: $LEAD_PERSONAS"

  if echo "$LEAD_PERSONAS" | grep -q "eng-manager.md"; then
    pass "lead has eng-manager.md persona"
  else
    fail "lead missing eng-manager.md persona"
  fi

  # Lead should NOT have developer.md (other agent's persona)
  if echo "$LEAD_PERSONAS" | grep -q "developer.md"; then
    fail "lead has developer.md (should be removed)"
  else
    pass "lead does not have developer.md (correctly removed)"
  fi

  # Check persona content includes inline instructions
  LEAD_PERSONA_CONTENT=$(orch_ssh "exec test-team-lead-1 cat .claude/personas/eng-manager.md 2>/dev/null" 2>&1 || true)
  if echo "$LEAD_PERSONA_CONTENT" | grep -q "LEAD READY"; then
    pass "lead persona contains inline instructions"
  else
    fail "lead persona missing inline instructions"
  fi
else
  skip "lead VM not ready — cannot test persona files"
  skip "lead developer.md removal"
  skip "lead persona instructions"
fi

if [ "$WORKER_READY" = "true" ]; then
  WORKER_PERSONAS=$(orch_ssh "exec test-team-worker-1 ls .claude/personas/ 2>/dev/null" 2>&1 || true)
  echo "  worker personas: $WORKER_PERSONAS"

  if echo "$WORKER_PERSONAS" | grep -q "developer.md"; then
    pass "worker has developer.md persona"
  else
    fail "worker missing developer.md persona"
  fi

  if echo "$WORKER_PERSONAS" | grep -q "eng-manager.md"; then
    fail "worker has eng-manager.md (should be removed)"
  else
    pass "worker does not have eng-manager.md (correctly removed)"
  fi
else
  skip "worker VM not ready — cannot test persona files"
  skip "worker eng-manager.md removal"
fi

# --- Test 6: Claude Code running (tapegun active) ---
log "=== Test 6: Claude Code running ==="
if [ "$LEAD_READY" = "true" ]; then
  # Check tapegun daemon is active
  TAPEGUN_STATUS=$(orch_ssh "exec test-team-lead-1 systemctl is-active boxcutter-tapegun" 2>&1 | tr -d '\n')
  if [ "$TAPEGUN_STATUS" = "active" ]; then
    pass "lead tapegun daemon is active"
  else
    fail "lead tapegun daemon not active (status: $TAPEGUN_STATUS)"
  fi

  # Check tmux session exists
  TMUX_CHECK=$(orch_ssh "exec test-team-lead-1 tmux has-session -t main 2>&1; echo \$?" 2>&1 | tail -1)
  if [ "$TMUX_CHECK" = "0" ]; then
    pass "lead has tmux session 'main'"
  else
    fail "lead missing tmux session 'main'"
  fi

  # Check Claude Code process running
  CLAUDE_PID=$(orch_ssh "exec test-team-lead-1 pgrep -f claude -u 1000" 2>&1 || true)
  if [ -n "$CLAUDE_PID" ]; then
    pass "lead has Claude Code process running"
  else
    fail "lead has no Claude Code process"
  fi
else
  skip "lead VM not ready — cannot test Claude Code"
  skip "lead tmux session"
  skip "lead Claude Code process"
fi

# --- Test 7: tapegun inbox messaging (plugin installed) ---
log "=== Test 7: tapegun inbox messaging ==="
if [ "$LEAD_READY" = "true" ]; then
  # Check plugin is installed
  PLUGIN_CHECK=$(orch_ssh "exec test-team-lead-1 test -f /home/dev/.claude/plugins/tapegun-guest/.claude-plugin/plugin.json && echo present" 2>&1 || true)
  if echo "$PLUGIN_CHECK" | grep -q "present"; then
    pass "lead has tapegun-guest plugin installed"
  else
    fail "lead missing tapegun-guest plugin"
  fi

  # Send a message and verify delivery
  orch_ssh "tapegun send test-team-lead-1 'E2E_TEST_MESSAGE_$(date +%s)'" >/dev/null 2>&1 || true
  sleep 8  # Wait for tapegun polling cycle (5s interval)

  INBOX=$(orch_ssh "exec test-team-lead-1 cat /home/dev/.tapegun/inbox.json 2>/dev/null" 2>&1 || true)
  if echo "$INBOX" | grep -q "E2E_TEST_MESSAGE"; then
    pass "tapegun inbox message delivered to lead"
  else
    fail "tapegun inbox message not delivered to lead"
  fi
else
  skip "lead VM not ready — cannot test plugin"
  skip "lead inbox messaging"
fi

# --- Test 8: re-apply is idempotent ---
log "=== Test 8: idempotent re-apply ==="
REAPPLY_OUT=$(cat "$SCRIPT_DIR/test-team-e2e.yaml" | orch_ssh "team apply -f -" 2>&1)
echo "$REAPPLY_OUT"

if echo "$REAPPLY_OUT" | grep -q "0 created.*3 existing.*0 destroyed"; then
  pass "re-apply is idempotent"
elif echo "$REAPPLY_OUT" | grep -q "0 created"; then
  pass "re-apply created 0 VMs (idempotent)"
else
  fail "re-apply was not idempotent"
fi

# --- Test 9: scale-up ---
log "=== Test 9: scale-up (workers 2→3) ==="
SCALE_UP_OUT=$(cat "$SCRIPT_DIR/test-team-e2e-scale.yaml" | orch_ssh "team apply -f -" 2>&1)
echo "$SCALE_UP_OUT"

if echo "$SCALE_UP_OUT" | grep -q "1 created"; then
  pass "scale-up created 1 new VM"
else
  fail "scale-up did not create expected VMs"
fi

sleep 5

# Verify test-team-worker-3 exists
STATUS_AFTER_SCALE=$(orch_ssh "team status test-team" 2>&1)
if echo "$STATUS_AFTER_SCALE" | grep -q "test-team-worker-3"; then
  pass "test-team-worker-3 exists after scale-up"
else
  fail "test-team-worker-3 missing after scale-up"
fi

# --- Test 10: scale-down ---
log "=== Test 10: scale-down (workers 3→1) ==="
SCALE_DOWN_OUT=$(cat "$SCRIPT_DIR/test-team-e2e-scaledown.yaml" | orch_ssh "team apply -f -" 2>&1)
echo "$SCALE_DOWN_OUT"

if echo "$SCALE_DOWN_OUT" | grep -q "2 destroyed"; then
  pass "scale-down destroyed 2 VMs"
else
  fail "scale-down did not destroy expected VMs"
fi

sleep 5

# Verify worker-2 and worker-3 are gone
STATUS_AFTER_DOWN=$(orch_ssh "team status test-team" 2>&1)
if echo "$STATUS_AFTER_DOWN" | grep -q "test-team-worker-2"; then
  fail "test-team-worker-2 still exists after scale-down"
else
  pass "test-team-worker-2 removed after scale-down"
fi
if echo "$STATUS_AFTER_DOWN" | grep -q "test-team-worker-3"; then
  fail "test-team-worker-3 still exists after scale-down"
else
  pass "test-team-worker-3 removed after scale-down"
fi

# --- Test 11: crash recovery ---
log "=== Test 11: crash recovery ==="
if [ "$LEAD_READY" = "true" ]; then
  # Kill Claude Code process and verify tapegun restarts it
  orch_ssh "exec test-team-lead-1 pkill -f claude -u 1000" >/dev/null 2>&1 || true
  log "  Killed Claude Code on lead, waiting for tapegun restart..."

  RECOVERED=false
  for i in $(seq 1 12); do
    sleep 5
    CLAUDE_PID=$(orch_ssh "exec test-team-lead-1 pgrep -f claude -u 1000" 2>&1 || true)
    if [ -n "$CLAUDE_PID" ]; then
      RECOVERED=true
      log "  Claude Code recovered after $((i*5))s"
      break
    fi
  done

  if [ "$RECOVERED" = "true" ]; then
    pass "crash recovery: Claude Code restarted by tapegun"
  else
    fail "crash recovery: Claude Code not restarted within 60s"
  fi
else
  skip "lead VM not ready — cannot test crash recovery"
fi

# --- Test 12: team destroy ---
log "=== Test 12: team destroy ==="
DESTROY_OUT=$(orch_ssh "team destroy test-team" 2>&1)
echo "$DESTROY_OUT"

sleep 5

REMAINING=$(orch_ssh "team status test-team" 2>&1)
if echo "$REMAINING" | grep -q "No VMs found"; then
  pass "team destroy removed all VMs"
else
  fail "team destroy did not remove all VMs"
fi

# --- Test 13: invalid YAML ---
log "=== Test 13: invalid YAML ==="
INVALID_OUT=$(echo "not: valid: team: yaml" | orch_ssh "team apply -f -" 2>&1)
if echo "$INVALID_OUT" | grep -qi "error"; then
  pass "invalid YAML produces error"
else
  fail "invalid YAML did not produce error"
fi

# --- Test 14: verify existing VMs unaffected ---
log "=== Test 14: existing VMs unaffected ==="
FINAL_VMS=$(orch_ssh "list" 2>/dev/null | tail -n +2 | wc -l)
if [ "$FINAL_VMS" -eq "$EXISTING_VMS" ]; then
  pass "existing VMs unaffected ($EXISTING_VMS VMs)"
else
  fail "VM count changed: was $EXISTING_VMS, now $FINAL_VMS"
fi

# --- Summary ---
echo ""
log "==============================="
log "  Results: $PASS passed, $FAIL failed ($TOTAL total)"
log "==============================="

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
