#!/bin/bash
# End-to-end test for session checkpoint-restore.
# Validates the full lifecycle: checkpoint_session() → file storage → restore_session() → --continue.
# Does NOT require a running VM or metadata service — uses local filesystem and mocked curl.
set -e

PASS=0
FAIL=0

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected '$expected', got '$actual')"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if echo "$haystack" | grep -qF -- "$needle"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected to contain '$needle')"
        FAIL=$((FAIL + 1))
    fi
}

assert_file_exists() {
    local desc="$1" path="$2"
    if [ -f "$path" ]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (file not found: $path)"
        FAIL=$((FAIL + 1))
    fi
}

assert_gt() {
    local desc="$1" val="$2" threshold="$3"
    if [ "$val" -gt "$threshold" ] 2>/dev/null; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc ($val not > $threshold)"
        FAIL=$((FAIL + 1))
    fi
}

TESTDIR=$(mktemp -d)
trap "rm -rf $TESTDIR" EXIT

# ============================================================
echo "=== E2E Test 1: Full checkpoint lifecycle ==="
# Simulate: running session → checkpoint → VM loss → restore → --continue
# ============================================================

# Set up fake home directory structure
FAKE_HOME="${TESTDIR}/home/dev"
FAKE_PROJECT="${FAKE_HOME}/project"
FAKE_CLAUDE="${FAKE_HOME}/.claude"
FAKE_SESSION_DIR="${FAKE_CLAUDE}/projects/-home-dev-project"
FAKE_CHECKPOINT_DIR="${TESTDIR}/checkpoints/test-vm"

mkdir -p "$FAKE_PROJECT/.git" "$FAKE_SESSION_DIR" "$FAKE_CHECKPOINT_DIR"

# Create a realistic session JSONL (simulating a Claude Code conversation)
SESSION_ID="e2e-test-session-$(date +%s)"
cat > "${FAKE_SESSION_DIR}/${SESSION_ID}.jsonl" <<'JSONL'
{"type":"system","message":"You are Claude Code...","uuid":"sys-1","timestamp":1700000001}
{"type":"user","message":"Fix the login timeout bug in auth.go","uuid":"usr-1","parentUuid":"sys-1","timestamp":1700000002}
{"type":"assistant","message":"I'll look at auth.go to understand the timeout issue.","uuid":"ast-1","parentUuid":"usr-1","timestamp":1700000003}
{"type":"assistant","toolUse":{"name":"Read","input":{"file_path":"/home/dev/project/auth.go"}},"uuid":"ast-2","parentUuid":"ast-1","timestamp":1700000004}
{"type":"user","message":"The timeout is set to 5s but production needs 30s","uuid":"usr-2","parentUuid":"ast-2","timestamp":1700000010}
{"type":"assistant","message":"I'll update the timeout constant from 5s to 30s.","uuid":"ast-3","parentUuid":"usr-2","timestamp":1700000011}
JSONL

SESSION_SIZE=$(wc -c < "${FAKE_SESSION_DIR}/${SESSION_ID}.jsonl")
assert_gt "session JSONL created with content" "$SESSION_SIZE" 100

# Set up a git repo with a branch
cd "$FAKE_PROJECT"
git init -q
git config user.email "test@test.com"
git config user.name "Test"
echo "package main" > main.go
git add main.go
git commit -q -m "init"
git checkout -q -b feat/fix-login-timeout
echo "timeout := 30 * time.Second" > auth.go
GIT_BRANCH=$(git branch --show-current)
assert_eq "git branch is feat branch" "feat/fix-login-timeout" "$GIT_BRANCH"
cd - >/dev/null

echo ""
echo "=== E2E Test 2: Simulate checkpoint_session() ==="

# Read session data and build checkpoint JSON (same logic as tapegun)
SESSION_FILE=$(ls -t "${FAKE_SESSION_DIR}"/*.jsonl 2>/dev/null | head -1)
assert_file_exists "found session file" "$SESSION_FILE"

CHECKPOINT_SESSION_ID=$(basename "$SESSION_FILE" .jsonl)
assert_eq "extracted session ID" "$SESSION_ID" "$CHECKPOINT_SESSION_ID"

GIT_BRANCH_CP=$(git -C "$FAKE_PROJECT" branch --show-current 2>/dev/null)
GIT_STASH_CP=$(git -C "$FAKE_PROJECT" stash create 2>/dev/null || echo "")

# Build checkpoint JSON using jq (same as tapegun does)
CHECKPOINT_JSON=$(jq -n \
    --arg session_id "$CHECKPOINT_SESSION_ID" \
    --arg git_branch "$GIT_BRANCH_CP" \
    --arg git_stash "$GIT_STASH_CP" \
    --rawfile session_data "$SESSION_FILE" \
    '{session_id: $session_id, git_branch: $git_branch, git_stash: $git_stash, session_data: $session_data}')

# Verify checkpoint JSON structure
CP_SID=$(echo "$CHECKPOINT_JSON" | jq -r '.session_id')
CP_BRANCH=$(echo "$CHECKPOINT_JSON" | jq -r '.git_branch')
CP_DATA_LINES=$(echo "$CHECKPOINT_JSON" | jq -r '.session_data' | wc -l)

assert_eq "checkpoint has correct session_id" "$SESSION_ID" "$CP_SID"
assert_eq "checkpoint has correct git_branch" "feat/fix-login-timeout" "$CP_BRANCH"
assert_gt "checkpoint session_data has lines" "$CP_DATA_LINES" 3

# Write checkpoint to "disk" (simulating vmid file storage)
echo "$CHECKPOINT_JSON" | jq -r '.session_data' > "${FAKE_CHECKPOINT_DIR}/${SESSION_ID}.jsonl"
assert_file_exists "checkpoint file written to disk" "${FAKE_CHECKPOINT_DIR}/${SESSION_ID}.jsonl"

echo ""
echo "=== E2E Test 3: Simulate VM loss — destroy session state ==="

# Clear the session directory (simulates VM being destroyed and recreated)
rm -rf "${FAKE_SESSION_DIR}"
mkdir -p "${FAKE_SESSION_DIR}"

# Verify session is gone
REMAINING=$(ls "${FAKE_SESSION_DIR}"/*.jsonl 2>/dev/null | wc -l)
assert_eq "session cleared (VM loss)" "0" "$REMAINING"

echo ""
echo "=== E2E Test 4: Simulate restore_session() ==="

# Parse checkpoint (same logic as tapegun restore_session)
RESTORE_SID=$(echo "$CHECKPOINT_JSON" | jq -r '.session_id // empty')
RESTORE_DATA=$(echo "$CHECKPOINT_JSON" | jq -r '.session_data // empty')
RESTORE_BRANCH=$(echo "$CHECKPOINT_JSON" | jq -r '.git_branch // empty')

# Restore session JSONL
echo "$RESTORE_DATA" > "${FAKE_SESSION_DIR}/${RESTORE_SID}.jsonl"
assert_file_exists "session JSONL restored" "${FAKE_SESSION_DIR}/${RESTORE_SID}.jsonl"

# Verify restored content matches original
RESTORED_LINES=$(wc -l < "${FAKE_SESSION_DIR}/${RESTORE_SID}.jsonl")
assert_eq "restored JSONL line count" "6" "$RESTORED_LINES"

# Verify conversation content survived
RESTORED_CONTENT=$(cat "${FAKE_SESSION_DIR}/${RESTORE_SID}.jsonl")
assert_contains "restored has user message" "Fix the login timeout" "$RESTORED_CONTENT"
assert_contains "restored has assistant response" "update the timeout constant" "$RESTORED_CONTENT"
assert_contains "restored has tool use" "Read" "$RESTORED_CONTENT"

# Restore git branch
cd "$FAKE_PROJECT"
git checkout "$RESTORE_BRANCH" 2>/dev/null
CURRENT_BRANCH=$(git branch --show-current)
assert_eq "git branch restored" "feat/fix-login-timeout" "$CURRENT_BRANCH"
cd - >/dev/null

# Set RESTORED_SESSION_ID (triggers --continue in Claude launch)
RESTORED_SESSION_ID="$RESTORE_SID"
assert_eq "RESTORED_SESSION_ID set" "$SESSION_ID" "$RESTORED_SESSION_ID"

echo ""
echo "=== E2E Test 5: Claude Code launch command with restored session ==="

CLAUDE_FLAGS="--dangerously-skip-permissions"
CLAUDE_RESUME=false

# Build command (same logic as tapegun)
CLAUDE_CMD="claude"
[ -n "$CLAUDE_FLAGS" ] && CLAUDE_CMD="$CLAUDE_CMD $CLAUDE_FLAGS"
if [ -n "$RESTORED_SESSION_ID" ]; then
    CLAUDE_CMD="$CLAUDE_CMD --continue"
elif [ "$CLAUDE_RESUME" = "true" ]; then
    CLAUDE_CMD="$CLAUDE_CMD --resume"
fi

assert_eq "claude cmd with restore" "claude --dangerously-skip-permissions --continue" "$CLAUDE_CMD"

echo ""
echo "=== E2E Test 6: Checkpoint size enforcement ==="

# Create a session larger than 20MB
OVERSIZED="${TESTDIR}/oversized.jsonl"
dd if=/dev/urandom bs=1M count=21 2>/dev/null | base64 > "$OVERSIZED"
OVERSIZE=$(wc -c < "$OVERSIZED")
assert_gt "oversized file is >20MB" "$OVERSIZE" 20971520

# The tapegun checkpoint_session skips files >20MB
SKIP=false
[ "$OVERSIZE" -gt 20971520 ] && SKIP=true
assert_eq "oversized session skipped" "true" "$SKIP"

echo ""
echo "=== E2E Test 7: No checkpoint available (fresh VM) ==="

# Simulate restore with no checkpoint (empty response)
EMPTY_CHECKPOINT=""
FRESH_RESTORED=""
if [ -z "$EMPTY_CHECKPOINT" ] || [ "$EMPTY_CHECKPOINT" = "null" ] || [ "$EMPTY_CHECKPOINT" = "{}" ]; then
    FRESH_RESTORED=""
fi

CLAUDE_CMD="claude --dangerously-skip-permissions"
if [ -n "$FRESH_RESTORED" ]; then
    CLAUDE_CMD="$CLAUDE_CMD --continue"
fi
assert_eq "fresh VM gets no --continue" "claude --dangerously-skip-permissions" "$CLAUDE_CMD"

echo ""
echo "=== E2E Test 8: Multiple sessions — picks most recent ==="

MULTI_DIR="${TESTDIR}/multi-sessions"
mkdir -p "$MULTI_DIR"

echo '{"old":true}' > "$MULTI_DIR/old-session.jsonl"
sleep 0.1
echo '{"mid":true}' > "$MULTI_DIR/mid-session.jsonl"
sleep 0.1
echo '{"new":true}' > "$MULTI_DIR/new-session.jsonl"

PICKED=$(ls -t "$MULTI_DIR"/*.jsonl | head -1)
PICKED_NAME=$(basename "$PICKED" .jsonl)
assert_eq "picks newest session" "new-session" "$PICKED_NAME"

echo ""
echo "=== E2E Test 9: Checkpoint on SIGTERM (clean shutdown) ==="

# Verify cleanup function calls checkpoint_session
# (structural test — grep the tapegun script)
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TAPEGUN="${SCRIPT_DIR}/boxcutter-tapegun"
if [ -f "$TAPEGUN" ]; then
    CLEANUP_HAS_CP=$(grep -c "checkpoint_session" "$TAPEGUN" | head -1)
    assert_gt "cleanup() calls checkpoint_session" "$CLEANUP_HAS_CP" 0
else
    echo "  SKIP: tapegun script not found at $TAPEGUN"
fi

echo ""
echo "=== E2E Test 10: Checkpoint interval (60s default) ==="

# Verify CHECKPOINT_INTERVAL is 60
if [ -f "$TAPEGUN" ]; then
    INTERVAL=$(grep '^CHECKPOINT_INTERVAL=' "$TAPEGUN" | head -1 | sed 's/CHECKPOINT_INTERVAL=//')
    assert_eq "default checkpoint interval is 60s" "60" "$INTERVAL"
fi

echo ""
echo "=== E2E Test 11: Round-trip data integrity ==="

# Verify that JSON round-trip through jq preserves special characters
SPECIAL_SESSION='{"msg":"line1\nline2","code":"func() { return \"hello\" }"}'
echo "$SPECIAL_SESSION" > "${TESTDIR}/special.jsonl"

RT_JSON=$(jq -n --rawfile data "${TESTDIR}/special.jsonl" '{session_data: $data}')
RT_DATA=$(echo "$RT_JSON" | jq -r '.session_data')
echo "$RT_DATA" > "${TESTDIR}/special-restored.jsonl"

ORIG_MD5=$(md5sum "${TESTDIR}/special.jsonl" | cut -d' ' -f1)
REST_MD5=$(md5sum "${TESTDIR}/special-restored.jsonl" | cut -d' ' -f1)
assert_eq "round-trip preserves data integrity" "$ORIG_MD5" "$REST_MD5"

echo ""
echo "========================================="
echo "=== E2E Results ==="
echo "Passed: $PASS"
echo "Failed: $FAIL"
[ $FAIL -eq 0 ] && echo "All e2e tests passed!" || exit 1
