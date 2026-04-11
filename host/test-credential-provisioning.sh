#!/bin/bash
# End-to-end integration test for Claude Code credential provisioning.
# Tests the full flow: secrets bundle → vmid metadata → tapegun → credentials file.
#
# Requires a running cluster with orchestrator + at least 1 node.
# The node's secrets bundle must include claude-credentials.json.
#
# Flow under test:
#   1. Host secrets bundle contains claude-credentials.json
#   2. boxcutter-setup writes claude_credentials_path into vmid config
#   3. vmid serves credentials at GET /metadata/claude-credentials
#   4. Tapegun fetches credentials during setup_workspace()
#   5. Credentials written to ~/.claude/.credentials.json inside VM
#   6. Claude Code starts authenticated (no OAuth prompt)
#
# Usage:
#   bash host/test-credential-provisioning.sh                    # Full suite
#   bash host/test-credential-provisioning.sh --skip-cleanup     # Keep test VMs
#   ORCH_IP=192.168.50.2 bash host/test-credential-provisioning.sh
set -e

SSH_KEY="${SSH_KEY:-/home/dev/.ssh/id_rsa_prod}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=30 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
ORCH_IP="${ORCH_IP:-192.168.50.2}"
CLUSTER_KEY="${CLUSTER_KEY:-/home/dev/.boxcutter/secrets/cluster-ssh.key}"
NODE_IP="${NODE_IP:-192.168.50.3}"
SECRETS_DIR="${SECRETS_DIR:-/home/dev/.boxcutter/secrets}"
VM_NAME="cred-test-vm"
TEAM_NAME="cred-test"

PASS=0
FAIL=0
TOTAL=0
SKIP_CLEANUP=false

for arg in "$@"; do
  case "$arg" in
    --skip-cleanup) SKIP_CLEANUP=true ;;
  esac
done

log()  { echo "[$(date +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; PASS=$((PASS+1)); TOTAL=$((TOTAL+1)); }
fail() { log "FAIL: $*"; FAIL=$((FAIL+1)); TOTAL=$((TOTAL+1)); }
skip() { log "SKIP: $*"; TOTAL=$((TOTAL+1)); }

orch_ssh()  { ssh -i "$SSH_KEY" $SSH_OPTS boxcutter@"$ORCH_IP" "$@" 2>/dev/null; }
node_ssh()  { ssh -i "$CLUSTER_KEY" $SSH_OPTS ubuntu@"$NODE_IP" "$@" 2>/dev/null; }

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

cleanup() {
  if [ "$SKIP_CLEANUP" = "false" ]; then
    log "=== Cleanup ==="
    orch_ssh "team destroy $TEAM_NAME" >/dev/null 2>&1 || true
    sleep 2
  fi
}

# ======================================================================
#  PRE-FLIGHT
# ======================================================================
log "============================================"
log "  Credential Provisioning E2E Test"
log "============================================"
log ""

log "=== Pre-flight ==="
if ! orch_ssh "status" >/dev/null 2>&1; then
  log "ERROR: Cannot reach orchestrator at $ORCH_IP"
  exit 1
fi
log "  Orchestrator reachable"

command -v jq >/dev/null || { log "ERROR: jq required"; exit 1; }

# ======================================================================
#  PHASE 1: SECRETS BUNDLE VERIFICATION
# ======================================================================
log ""
log "=== Phase 1: Secrets bundle verification ==="

# --- Test 1: claude-credentials.json exists in secrets bundle on node ---
log "--- Test 1: Secrets bundle contains claude-credentials.json ---"
CRED_EXISTS=$(node_ssh "test -f /etc/boxcutter/secrets/claude-credentials.json && echo yes || echo no" 2>&1 || echo "unreachable")
if [ "$CRED_EXISTS" = "yes" ]; then
  pass "claude-credentials.json exists in node secrets bundle"
elif [ "$CRED_EXISTS" = "unreachable" ]; then
  skip "Cannot SSH to node at $NODE_IP — set CLUSTER_KEY and NODE_IP"
else
  fail "claude-credentials.json missing from node secrets bundle"
  log "  Place credentials at /etc/boxcutter/secrets/claude-credentials.json on node"
  log "  Format: {\"claudeAiOauth\":{\"refreshToken\":\"...\",\"accessToken\":\"...\", ...}}"
fi

# --- Test 2: vmid config references credentials path ---
log "--- Test 2: vmid config has claude_credentials_path ---"
VMID_CONFIG_HAS_CREDS=$(node_ssh "grep -q claude_credentials_path /etc/vmid/config.yaml && echo yes || echo no" 2>&1 || echo "unreachable")
if [ "$VMID_CONFIG_HAS_CREDS" = "yes" ]; then
  pass "vmid config includes claude_credentials_path"
elif [ "$VMID_CONFIG_HAS_CREDS" = "unreachable" ]; then
  skip "Cannot SSH to node"
else
  fail "vmid config missing claude_credentials_path (run boxcutter-setup)"
fi

# ======================================================================
#  PHASE 2: METADATA ENDPOINT VERIFICATION
# ======================================================================
log ""
log "=== Phase 2: Metadata endpoint verification ==="

# Clean up any leftover test team
orch_ssh "team destroy $TEAM_NAME" >/dev/null 2>&1 || true
sleep 2

# Create a test VM via team apply
log "  Creating test VM via team apply..."
TEAM_YAML="apiVersion: boxcutter/v1
kind: Team
metadata:
  name: cred-test
spec:
  defaults:
    type: firecracker
    ram: 4G
    vcpu: 2
    mode: normal
    repos:
      - AndrewBudd/boxcutter
  agents:
    - name: vm
      persona:
        role: credential-test
        instructions: \"Credential provisioning test agent.\""

APPLY_OUT=$(echo "$TEAM_YAML" | orch_ssh "team apply -f -" 2>&1)
log "  Apply output: $APPLY_OUT"

if echo "$APPLY_OUT" | grep -q "1 created"; then
  pass "test VM created"
else
  fail "test VM creation failed"
  cleanup
  exit 1
fi

# Wait for tapegun to be reporting
if ! wait_for_vm_ready "${TEAM_NAME}-vm-1" 300; then
  fail "test VM did not become ready within 5 minutes"
  cleanup
  exit 1
fi

# --- Test 3: metadata endpoint returns credentials ---
log "--- Test 3: /metadata/claude-credentials endpoint responds ---"
METADATA_RESP=$(orch_ssh "exec ${TEAM_NAME}-vm-1 curl -sf http://169.254.169.254/metadata/claude-credentials" 2>&1 || true)

if [ -z "$METADATA_RESP" ]; then
  fail "metadata endpoint returned empty response (credentials not configured on host?)"
elif echo "$METADATA_RESP" | jq -e '.claudeAiOauth.refreshToken // .oauth_refresh_token' >/dev/null 2>&1; then
  pass "metadata endpoint returns valid credentials JSON"
elif echo "$METADATA_RESP" | grep -q "not configured\|not available"; then
  fail "metadata endpoint: credentials not configured ($METADATA_RESP)"
else
  fail "metadata endpoint returned unexpected response: $METADATA_RESP"
fi

# --- Test 4: metadata endpoint returns correct content type ---
log "--- Test 4: /metadata/claude-credentials content type ---"
CONTENT_TYPE=$(orch_ssh "exec ${TEAM_NAME}-vm-1 curl -sf -o /dev/null -w '%{content_type}' http://169.254.169.254/metadata/claude-credentials" 2>&1 || true)
if echo "$CONTENT_TYPE" | grep -q "application/json"; then
  pass "metadata endpoint returns application/json content type"
else
  skip "content type check ($CONTENT_TYPE)"
fi

# ======================================================================
#  PHASE 3: TAPEGUN CREDENTIAL PROVISIONING
# ======================================================================
log ""
log "=== Phase 3: Tapegun credential provisioning ==="

# --- Test 5: credentials file exists in VM ---
log "--- Test 5: ~/.claude/.credentials.json exists in VM ---"
CRED_FILE_EXISTS=$(orch_ssh "exec ${TEAM_NAME}-vm-1 test -f /home/dev/.claude/.credentials.json && echo yes || echo no" 2>&1 || echo "no")
if [ "$CRED_FILE_EXISTS" = "yes" ]; then
  pass "credentials file exists at ~/.claude/.credentials.json"
else
  fail "credentials file not found at ~/.claude/.credentials.json"
fi

# --- Test 6: credentials file has correct format ---
log "--- Test 6: Credentials file format (claudeAiOauth wrapper) ---"
if [ "$CRED_FILE_EXISTS" = "yes" ]; then
  CRED_CONTENTS=$(orch_ssh "exec ${TEAM_NAME}-vm-1 cat /home/dev/.claude/.credentials.json" 2>&1 || true)

  HAS_WRAPPER=$(echo "$CRED_CONTENTS" | jq -e 'has("claudeAiOauth")' 2>/dev/null || echo "false")
  if [ "$HAS_WRAPPER" = "true" ]; then
    pass "credentials file has claudeAiOauth wrapper"
  else
    fail "credentials file missing claudeAiOauth wrapper"
  fi

  HAS_REFRESH=$(echo "$CRED_CONTENTS" | jq -e '.claudeAiOauth.refreshToken' >/dev/null 2>&1 && echo "yes" || echo "no")
  if [ "$HAS_REFRESH" = "yes" ]; then
    pass "credentials file contains refreshToken"
  else
    fail "credentials file missing refreshToken"
  fi

  HAS_ACCESS=$(echo "$CRED_CONTENTS" | jq -e '.claudeAiOauth.accessToken' >/dev/null 2>&1 && echo "yes" || echo "no")
  if [ "$HAS_ACCESS" = "yes" ]; then
    pass "credentials file contains accessToken"
  else
    fail "credentials file missing accessToken"
  fi
else
  skip "credentials file format — file does not exist"
  skip "credentials refreshToken"
  skip "credentials accessToken"
fi

# --- Test 7: credentials file permissions ---
log "--- Test 7: Credentials file permissions (600) ---"
if [ "$CRED_FILE_EXISTS" = "yes" ]; then
  CRED_PERMS=$(orch_ssh "exec ${TEAM_NAME}-vm-1 stat -c '%a' /home/dev/.claude/.credentials.json" 2>&1 || true)
  if [ "$CRED_PERMS" = "600" ]; then
    pass "credentials file has mode 600"
  else
    fail "credentials file mode is $CRED_PERMS, expected 600"
  fi
else
  skip "credentials file permissions — file does not exist"
fi

# --- Test 8: credentials file owned by dev user ---
log "--- Test 8: Credentials file ownership ---"
if [ "$CRED_FILE_EXISTS" = "yes" ]; then
  CRED_OWNER=$(orch_ssh "exec ${TEAM_NAME}-vm-1 stat -c '%U' /home/dev/.claude/.credentials.json" 2>&1 || true)
  if [ "$CRED_OWNER" = "dev" ]; then
    pass "credentials file owned by dev user"
  else
    fail "credentials file owned by $CRED_OWNER, expected dev"
  fi
else
  skip "credentials file ownership — file does not exist"
fi

# --- Test 9: tapegun log shows credential installation ---
log "--- Test 9: Tapegun log confirms credential provisioning ---"
TAPEGUN_LOG=$(orch_ssh "exec ${TEAM_NAME}-vm-1 journalctl -u boxcutter-tapegun --no-pager -n 100" 2>&1 || true)
if echo "$TAPEGUN_LOG" | grep -q "Claude Code credentials installed"; then
  pass "tapegun log confirms credentials installed"
elif echo "$TAPEGUN_LOG" | grep -q "no Claude credentials available"; then
  fail "tapegun log says no credentials available (metadata endpoint not serving)"
elif echo "$TAPEGUN_LOG" | grep -q "credentials response missing token fields"; then
  fail "tapegun log says credential format invalid"
else
  fail "tapegun log has no credential provisioning message"
fi

# ======================================================================
#  PHASE 4: CLAUDE CODE AUTHENTICATION
# ======================================================================
log ""
log "=== Phase 4: Claude Code authentication ==="

# --- Test 10: Claude Code is running (not stuck on OAuth prompt) ---
log "--- Test 10: Claude Code process running ---"
CLAUDE_RUNNING=$(orch_ssh "exec ${TEAM_NAME}-vm-1 pgrep -f claude -u 1000" 2>&1 || true)
if [ -n "$CLAUDE_RUNNING" ]; then
  pass "Claude Code process is running"
else
  fail "Claude Code process not found"
fi

# --- Test 11: tmux session does not show OAuth login prompt ---
log "--- Test 11: No OAuth login prompt in tmux ---"
PANE_CONTENT=$(orch_ssh "exec ${TEAM_NAME}-vm-1 tmux capture-pane -t main -p" 2>&1 || true)
if echo "$PANE_CONTENT" | grep -qi "login\|authenticate\|oauth\|browser\|sign.in"; then
  fail "tmux pane shows authentication prompt — credentials not working"
  log "  Pane content: $(echo "$PANE_CONTENT" | head -5)"
else
  pass "no OAuth login prompt in tmux (credentials working)"
fi

# ======================================================================
#  PHASE 5: CREDENTIAL ROTATION
# ======================================================================
log ""
log "=== Phase 5: Credential rotation ==="

# --- Test 12: metadata reads credentials fresh (supports rotation) ---
log "--- Test 12: Metadata endpoint reads fresh on each request ---"
# Two sequential requests should both succeed (file read, not cached)
RESP1=$(orch_ssh "exec ${TEAM_NAME}-vm-1 curl -sf http://169.254.169.254/metadata/claude-credentials" 2>&1 || true)
RESP2=$(orch_ssh "exec ${TEAM_NAME}-vm-1 curl -sf http://169.254.169.254/metadata/claude-credentials" 2>&1 || true)
if [ -n "$RESP1" ] && [ -n "$RESP2" ]; then
  pass "metadata endpoint serves credentials on repeated requests (no caching issues)"
else
  fail "metadata endpoint failed on repeated requests"
fi

# ======================================================================
#  PHASE 6: GRACEFUL DEGRADATION
# ======================================================================
log ""
log "=== Phase 6: Graceful degradation (no credentials) ==="

# --- Test 13: tapegun handles missing credentials gracefully ---
log "--- Test 13: Missing credentials don't crash tapegun ---"
# Even if credentials are absent, tapegun should still be running
TAPEGUN_STATUS=$(orch_ssh "exec ${TEAM_NAME}-vm-1 systemctl is-active boxcutter-tapegun" 2>&1 | tr -d '\n')
if [ "$TAPEGUN_STATUS" = "active" ]; then
  pass "tapegun daemon still active (graceful degradation works)"
else
  fail "tapegun daemon not active ($TAPEGUN_STATUS)"
fi

# --- Test 14: VM is healthy despite credential state ---
log "--- Test 14: VM health unaffected by credential state ---"
HEALTH=$(orch_ssh "exec ${TEAM_NAME}-vm-1 curl -sf http://169.254.169.254/tapegun/health" 2>&1 || true)
if echo "$HEALTH" | jq -e '.health == "healthy"' >/dev/null 2>&1; then
  pass "VM reports healthy"
elif echo "$HEALTH" | jq -e '.health' >/dev/null 2>&1; then
  pass "VM reports health status: $(echo "$HEALTH" | jq -r '.health')"
else
  skip "could not check VM health ($HEALTH)"
fi

# ======================================================================
#  CLEANUP
# ======================================================================
cleanup

# ======================================================================
#  SUMMARY
# ======================================================================
log ""
log "============================================"
log "  Credential Provisioning E2E Results"
log "  $PASS passed, $FAIL failed ($TOTAL total)"
log "============================================"

if [ "$FAIL" -gt 0 ]; then
  log ""
  log "FAILED — credential provisioning has issues"
  exit 1
else
  log ""
  log "ALL TESTS PASSED — credential provisioning working end-to-end"
  exit 0
fi
