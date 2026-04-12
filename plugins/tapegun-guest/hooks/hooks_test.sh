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
echo "=== Test 2: PostToolUse checks queue after Bash ==="
MATCHER=$(jq -r '.hooks.PostToolUse[0].matcher' "$HOOKS_FILE")
assert_eq "PostToolUse matcher is Bash" "Bash" "$MATCHER"
CMD=$(jq -r '.hooks.PostToolUse[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "PostToolUse runs check-queue" 'true' "$(echo "$CMD" | grep -q 'check-queue' && echo true || echo false)"

echo ""
echo "=== Test 3: UserPromptSubmit reports status ==="
STATUS_CMD=$(jq -r '.hooks.UserPromptSubmit[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "UserPromptSubmit reports working" 'true' "$(echo "$STATUS_CMD" | grep -q 'report-status.sh working' && echo true || echo false)"

echo ""
echo "=== Test 4: UserPromptSubmit also checks queue ==="
QUEUE_CMD=$(jq -r '.hooks.UserPromptSubmit[0].hooks[1].command' "$HOOKS_FILE")
assert_eq "UserPromptSubmit checks queue" 'true' "$(echo "$QUEUE_CMD" | grep -q 'check-queue.sh' && echo true || echo false)"
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
echo "=== Test 6: All 6 hook events are configured ==="
EVENTS=$(jq -r '.hooks | keys[]' "$HOOKS_FILE" | sort | tr '\n' ',')
assert_eq "All events present" "Notification,PostToolUse,Stop,StopFailure,SubagentStop,UserPromptSubmit," "$EVENTS"

echo ""
echo "=== Test 7: Notification hook checks queue on idle ==="
NOTIF_MATCHER=$(jq -r '.hooks.Notification[0].matcher' "$HOOKS_FILE")
assert_eq "Notification matcher is idle_prompt" "idle_prompt" "$NOTIF_MATCHER"
NOTIF_CMD=$(jq -r '.hooks.Notification[0].hooks[0].command' "$HOOKS_FILE")
assert_eq "Notification runs check-queue" 'true' "$(echo "$NOTIF_CMD" | grep -q 'check-queue' && echo true || echo false)"

echo ""
echo "=== Test 8: All referenced scripts exist ==="
for script in check-queue.sh report-status.sh; do
    if [ -f "${SCRIPT_DIR}/${script}" ]; then
        echo "  PASS: ${script} exists"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: ${script} not found"
        FAIL=$((FAIL + 1))
    fi
done

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
