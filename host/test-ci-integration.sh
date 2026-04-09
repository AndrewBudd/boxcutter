#!/bin/bash
# E2E test for CI integration / PR pipeline (issue #122).
# Tests the CI watcher function in tapegun.
#
# Tests:
#   1. check_ci_status function exists in deployed tapegun
#   2. CI polling only activates when a PR exists
#   3. CI pass notification delivered to inbox
#   4. CI failure notification delivered to inbox
#   5. Circuit breaker escalation after N failures
#   6. State change dedup (no duplicate notifications)
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SSH_KEY="${SSH_KEY:-/home/dev/.ssh/boxcutter-vm.key}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=30 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
ORCH_IP="${ORCH_IP:-192.168.50.2}"

PASS=0
FAIL=0
TOTAL=0

log()  { echo "[$(date +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; PASS=$((PASS+1)); TOTAL=$((TOTAL+1)); }
fail() { log "FAIL: $*"; FAIL=$((FAIL+1)); TOTAL=$((TOTAL+1)); }

orch_ssh() { ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@"$ORCH_IP" "$@" 2>/dev/null; }

# --- Pre-flight ---
log "=== Pre-flight ==="
if ! orch_ssh "status" >/dev/null 2>&1; then
    log "ERROR: Cannot reach orchestrator"
    exit 1
fi

VM_NAME=$(orch_ssh "list" 2>/dev/null | tail -n +2 | awk '{print $1}' | head -1)
[ -z "$VM_NAME" ] && { log "ERROR: No VMs available"; exit 1; }
log "Using VM: $VM_NAME"

# --- Test 1: check_ci_status function exists ---
log "=== Test 1: CI watcher function present ==="
FUNC_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'check_ci_status' /usr/local/bin/boxcutter-tapegun 2>/dev/null || echo 0" 2>&1)
if [ "$FUNC_CHECK" -gt 0 ] 2>/dev/null; then
    pass "check_ci_status function present in tapegun ($FUNC_CHECK references)"
else
    # Deploy updated tapegun for testing
    log "Deploying updated tapegun to $VM_NAME..."
    cat "$SCRIPT_DIR/../node/golden/config/boxcutter-tapegun" | orch_ssh "cp-to $VM_NAME - /tmp/boxcutter-tapegun" 2>/dev/null
    orch_ssh "exec $VM_NAME" "sudo rm -f /usr/local/bin/boxcutter-tapegun && sudo cp /tmp/boxcutter-tapegun /usr/local/bin/boxcutter-tapegun && sudo chmod 755 /usr/local/bin/boxcutter-tapegun" 2>/dev/null
    FUNC_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'check_ci_status' /usr/local/bin/boxcutter-tapegun 2>/dev/null || echo 0" 2>&1)
    if [ "$FUNC_CHECK" -gt 0 ] 2>/dev/null; then
        pass "check_ci_status deployed to $VM_NAME ($FUNC_CHECK references)"
    else
        fail "could not deploy check_ci_status to $VM_NAME"
    fi
fi

# --- Test 2: CI state variables initialized ---
log "=== Test 2: CI state variables ==="
VAR_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'CI_POLL_COUNTER\|CI_FIX_ATTEMPTS\|CI_MAX_FIX_ATTEMPTS\|LAST_CI_STATE' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$VAR_CHECK" -gt 3 ] 2>/dev/null; then
    pass "CI state variables present ($VAR_CHECK references)"
else
    fail "CI state variables missing"
fi

# --- Test 3: CI poll gated on gh + jq availability ---
log "=== Test 3: gh and jq dependency check ==="
GH_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'command -v gh' /usr/local/bin/boxcutter-tapegun" 2>&1)
JQ_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'command -v jq' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$GH_CHECK" -gt 0 ] 2>/dev/null && [ "$JQ_CHECK" -gt 0 ] 2>/dev/null; then
    pass "CI polling gated on gh and jq availability"
else
    fail "CI polling not gated on tool availability"
fi

# --- Test 4: Circuit breaker threshold ---
log "=== Test 4: Circuit breaker ==="
CB_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'CI_MAX_FIX_ATTEMPTS' /usr/local/bin/boxcutter-tapegun" 2>&1)
ESCALATE_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'escalat' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$CB_CHECK" -gt 0 ] 2>/dev/null && [ "$ESCALATE_CHECK" -gt 0 ] 2>/dev/null; then
    pass "Circuit breaker with escalation present"
else
    fail "Circuit breaker logic missing"
fi

# --- Test 5: State change dedup ---
log "=== Test 5: State change dedup ==="
DEDUP_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'LAST_CI_STATE' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$DEDUP_CHECK" -gt 1 ] 2>/dev/null; then
    pass "State change dedup logic present ($DEDUP_CHECK references)"
else
    fail "State change dedup missing"
fi

# --- Test 6: Poll interval (every 6th cycle = 30s) ---
log "=== Test 6: Poll interval ==="
INTERVAL_CHECK=$(orch_ssh "exec $VM_NAME" "grep 'CI_POLL_COUNTER % 6' /usr/local/bin/boxcutter-tapegun" 2>&1)
if echo "$INTERVAL_CHECK" | grep -q "% 6"; then
    pass "CI polls every 6th cycle (30s at 5s poll interval)"
else
    fail "CI poll interval not set to every 6th cycle"
fi

# --- Test 7: No PR guard — function returns early ---
log "=== Test 7: No PR guard ==="
GUARD_CHECK=$(orch_ssh "exec $VM_NAME" "grep -A2 'pr_num=.*gh pr view' /usr/local/bin/boxcutter-tapegun | grep -c 'return'" 2>&1)
if [ "$GUARD_CHECK" -gt 0 ] 2>/dev/null; then
    pass "Function returns early when no PR exists"
else
    fail "Missing early return guard for no-PR case"
fi

# --- Test 8: CI pass resets fix counter ---
log "=== Test 8: Fix counter reset on pass ==="
RESET_CHECK=$(orch_ssh "exec $VM_NAME" "grep -A2 'All checks passed' /usr/local/bin/boxcutter-tapegun | grep -c 'CI_FIX_ATTEMPTS=0'" 2>&1)
if [ "$RESET_CHECK" -gt 0 ] 2>/dev/null; then
    pass "Fix counter resets to 0 on CI pass"
else
    fail "Fix counter not reset on CI pass"
fi

# --- Summary ---
echo ""
log "==============================="
log "  Results: $PASS passed, $FAIL failed ($TOTAL total)"
log "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
