#!/bin/bash
# End-to-end integration test for team YAML apply/scale/destroy.
# Requires a running cluster with orchestrator + at least 1 node.
#
# Tests:
#   1. team apply creates VMs with correct names
#   2. team list shows the team
#   3. team status shows VM status
#   4. re-apply is idempotent (no-op)
#   5. scale-up creates additional VMs
#   6. scale-down destroys excess VMs
#   7. team destroy removes all VMs
#   8. invalid YAML produces clear error
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

# --- Test 4: re-apply is idempotent ---
log "=== Test 4: idempotent re-apply ==="
REAPPLY_OUT=$(cat "$SCRIPT_DIR/test-team-e2e.yaml" | orch_ssh "team apply -f -" 2>&1)
echo "$REAPPLY_OUT"

if echo "$REAPPLY_OUT" | grep -q "0 created.*3 existing.*0 destroyed"; then
  pass "re-apply is idempotent"
elif echo "$REAPPLY_OUT" | grep -q "0 created"; then
  pass "re-apply created 0 VMs (idempotent)"
else
  fail "re-apply was not idempotent"
fi

# --- Test 5: scale-up ---
log "=== Test 5: scale-up (workers 2→3) ==="
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

# --- Test 6: scale-down ---
log "=== Test 6: scale-down (workers 3→1) ==="
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

# --- Test 7: team destroy ---
log "=== Test 7: team destroy ==="
DESTROY_OUT=$(orch_ssh "team destroy test-team" 2>&1)
echo "$DESTROY_OUT"

sleep 5

REMAINING=$(orch_ssh "team status test-team" 2>&1)
if echo "$REMAINING" | grep -q "No VMs found"; then
  pass "team destroy removed all VMs"
else
  fail "team destroy did not remove all VMs"
fi

# --- Test 8: invalid YAML ---
log "=== Test 8: invalid YAML ==="
INVALID_OUT=$(echo "not: valid: team: yaml" | orch_ssh "team apply -f -" 2>&1)
if echo "$INVALID_OUT" | grep -qi "error"; then
  pass "invalid YAML produces error"
else
  fail "invalid YAML did not produce error"
fi

# --- Test 9: verify existing VMs unaffected ---
log "=== Test 9: existing VMs unaffected ==="
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
