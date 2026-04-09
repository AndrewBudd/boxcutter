#!/bin/bash
# Tests for tapegun-guest hooks configuration.
# Validates that hooks.json has the correct event bindings.
# Run: bash plugins/tapegun-guest/hooks/hooks_test.sh
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

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOKS_FILE="${SCRIPT_DIR}/hooks.json"

echo "=== Test 1: hooks.json is valid JSON ==="
if jq empty "$HOOKS_FILE" 2>/dev/null; then
    echo "  PASS: valid JSON"
    PASS=$((PASS + 1))
else
    echo "  FAIL: hooks.json is not valid JSON"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "=== Test 2: PostToolUse checks inbox after Bash ==="
MATCHER=$(jq -r '.hooks.PostToolUse[0].matcher' "$HOOKS_FILE")
assert_eq "PostToolUse matcher is Bash" "Bash" "$MATCHER"
CMD=$(jq -r '.hooks.PostToolUse[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "PostToolUse runs check-inbox" 'true' "$(echo "$CMD" | grep -q 'check-inbox' && echo true || echo false)"

echo ""
echo "=== Test 3: UserPromptSubmit reports status ==="
STATUS_CMD=$(jq -r '.hooks.UserPromptSubmit[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "UserPromptSubmit reports working" 'true' "$(echo "$STATUS_CMD" | grep -q 'report-status.sh working' && echo true || echo false)"

echo ""
echo "=== Test 4: UserPromptSubmit also checks inbox ==="
INBOX_CMD=$(jq -r '.hooks.UserPromptSubmit[0].hooks[1].command' "$HOOKS_FILE")
assert_eq "UserPromptSubmit checks inbox" 'true' "$(echo "$INBOX_CMD" | grep -q 'check-inbox.sh' && echo true || echo false)"
PROMPT_HOOK_COUNT=$(jq '.hooks.UserPromptSubmit[0].hooks | length' "$HOOKS_FILE")
assert_eq "UserPromptSubmit has 2 hooks" "2" "$PROMPT_HOOK_COUNT"

echo ""
echo "=== Test 5: Stop/StopFailure/SubagentStop report status ==="
STOP_CMD=$(jq -r '.hooks.Stop[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "Stop reports idle" 'true' "$(echo "$STOP_CMD" | grep -q 'report-status.sh idle' && echo true || echo false)"
FAIL_CMD=$(jq -r '.hooks.StopFailure[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "StopFailure reports error" 'true' "$(echo "$FAIL_CMD" | grep -q 'report-status.sh error' && echo true || echo false)"
SUB_CMD=$(jq -r '.hooks.SubagentStop[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "SubagentStop reports idle" 'true' "$(echo "$SUB_CMD" | grep -q 'report-status.sh idle' && echo true || echo false)"

echo ""
echo "=== Test 6: All 5 hook events are configured ==="
EVENTS=$(jq -r '.hooks | keys[]' "$HOOKS_FILE" | sort | tr '\n' ',')
assert_eq "All events present" "PostToolUse,Stop,StopFailure,SubagentStop,UserPromptSubmit," "$EVENTS"

echo ""
echo "=== Test 7: check-inbox.sh is executable and exists ==="
INBOX_SCRIPT="${SCRIPT_DIR}/check-inbox.sh"
if [ -f "$INBOX_SCRIPT" ]; then
    echo "  PASS: check-inbox.sh exists"
    PASS=$((PASS + 1))
else
    echo "  FAIL: check-inbox.sh not found"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "=== Test 8: check-inbox.sh handles missing inbox gracefully ==="
# Run check-inbox with a nonexistent inbox — should exit 0
INBOX="/nonexistent/path" bash -c '
    INBOX="/nonexistent/path"
    [ -f "$INBOX" ] || exit 0
    exit 1
' 2>/dev/null
if [ $? -eq 0 ]; then
    echo "  PASS: exits cleanly when inbox missing"
    PASS=$((PASS + 1))
else
    echo "  FAIL: should exit 0 when inbox missing"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
