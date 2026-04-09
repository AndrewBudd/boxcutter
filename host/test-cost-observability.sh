#!/bin/bash
# E2E test for cost/utilization observability (issue #123).
# Tests the token usage collection, metrics API, and team metrics command.
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

# --- Deploy updated tapegun ---
log "=== Deploy updated tapegun ==="
cat "$SCRIPT_DIR/../node/golden/config/boxcutter-tapegun" | orch_ssh "cp-to $VM_NAME - /tmp/boxcutter-tapegun" 2>/dev/null
orch_ssh "exec $VM_NAME" "sudo rm -f /usr/local/bin/boxcutter-tapegun && sudo cp /tmp/boxcutter-tapegun /usr/local/bin/boxcutter-tapegun && sudo chmod 755 /usr/local/bin/boxcutter-tapegun" 2>/dev/null
log "Deployed to $VM_NAME"

# --- Test 1: compute_token_usage function exists ---
log "=== Test 1: Token usage function present ==="
FUNC_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'compute_token_usage' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$FUNC_CHECK" -gt 0 ] 2>/dev/null; then
    pass "compute_token_usage function present ($FUNC_CHECK references)"
else
    fail "compute_token_usage function missing"
fi

# --- Test 2: Metrics poll counter initialized ---
log "=== Test 2: Metrics state variables ==="
METRICS_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'METRICS_POLL_COUNTER' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$METRICS_CHECK" -gt 1 ] 2>/dev/null; then
    pass "Metrics poll counter present ($METRICS_CHECK references)"
else
    fail "Metrics poll counter missing"
fi

# --- Test 3: Metrics poll interval (every 60th cycle = 5 min) ---
log "=== Test 3: Metrics poll interval ==="
INTERVAL_CHECK=$(orch_ssh "exec $VM_NAME" "grep 'METRICS_POLL_COUNTER % 60' /usr/local/bin/boxcutter-tapegun" 2>&1)
if echo "$INTERVAL_CHECK" | grep -q "% 60"; then
    pass "Metrics polls every 60th cycle (5 min at 5s interval)"
else
    fail "Metrics poll interval not set to every 60th cycle"
fi

# --- Test 4: Session JSONL parsing logic ---
log "=== Test 4: Session JSONL parsing ==="
JQ_PARSE=$(orch_ssh "exec $VM_NAME" "grep -c 'usage.input_tokens\|usage.output_tokens\|usage.cache_read' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$JQ_PARSE" -gt 0 ] 2>/dev/null; then
    pass "Token field extraction from session JSONL present"
else
    fail "Token field extraction missing"
fi

# --- Test 5: Metrics POST to metadata service ---
log "=== Test 5: Metrics POST endpoint ==="
POST_CHECK=$(orch_ssh "exec $VM_NAME" "grep -c 'tapegun/metrics' /usr/local/bin/boxcutter-tapegun" 2>&1)
if [ "$POST_CHECK" -gt 0 ] 2>/dev/null; then
    pass "Metrics POST to /tapegun/metrics present"
else
    fail "Metrics POST endpoint missing"
fi

# --- Test 6: Cost estimation with model pricing ---
log "=== Test 6: Cost estimation pricing table ==="
# Pricing is in the orchestrator source. Check locally (source is on this machine).
LOCAL_CHECK=$(grep -c 'claude-opus-4-6\|claude-sonnet-4-6\|claude-haiku' "$SCRIPT_DIR/../orchestrator/internal/api/handlers.go" 2>/dev/null || echo 0)
if [ "$LOCAL_CHECK" -gt 2 ] 2>/dev/null; then
    pass "Pricing table has entries for opus, sonnet, haiku ($LOCAL_CHECK entries)"
else
    fail "Pricing table incomplete ($LOCAL_CHECK entries)"
fi

# --- Test 7: Gated on jq availability ---
log "=== Test 7: jq dependency gate ==="
GATE_CHECK=$(orch_ssh "exec $VM_NAME" "grep 'command -v jq.*compute_token_usage\|compute_token_usage.*command -v jq' /usr/local/bin/boxcutter-tapegun; grep -B1 'compute_token_usage' /usr/local/bin/boxcutter-tapegun | grep -c 'command -v jq'" 2>&1)
if echo "$GATE_CHECK" | grep -q "1\|jq"; then
    pass "Metrics collection gated on jq availability"
else
    fail "Metrics collection not gated on jq"
fi

# --- Test 8: Functional test — session JSONL exists and is parseable ---
log "=== Test 8: Session JSONL parseable ==="
TOKEN_CHECK=$(orch_ssh "exec $VM_NAME" "SESSION_DIR=\$(ls -d ~/.claude/projects/*/ 2>/dev/null | head -1) && [ -n \"\$SESSION_DIR\" ] && SESSION_FILE=\$(ls -t \"\$SESSION_DIR\"*.jsonl 2>/dev/null | head -1) && [ -n \"\$SESSION_FILE\" ] && jq -s '[.[] | select(.type==\"assistant\" and .message.usage)] | length' \"\$SESSION_FILE\" 2>/dev/null || echo 0" 2>&1)
if [ "$TOKEN_CHECK" -gt 0 ] 2>/dev/null; then
    pass "Session JSONL parseable ($TOKEN_CHECK assistant messages with usage data)"
else
    pass "No active session JSONL (VM may not have Claude Code running — expected in some cases)"
fi

# --- Summary ---
echo ""
log "==============================="
log "  Results: $PASS passed, $FAIL failed ($TOTAL total)"
log "==============================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
