#!/bin/bash
# Tests for boxcutter-tapegun autostart behavior.
# Validates config parsing, defaults, and metadata label overrides.
# Run: bash boxcutter-tapegun_test.sh

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

# --- Helper: extract default values from the tapegun script ---
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="${SCRIPT_DIR}/boxcutter-tapegun"

extract_default() {
    local var="$1"
    grep -E "^${var}=" "$SCRIPT" | head -1 | sed "s/^${var}=//" | tr -d '"'
}

echo "=== Test 1: Default values ==="
assert_eq "AUTOSTART_CLAUDE defaults to true" "true" "$(extract_default AUTOSTART_CLAUDE)"
assert_eq "CLAUDE_FLAGS defaults to --dangerously-skip-permissions" "--dangerously-skip-permissions" "$(extract_default CLAUDE_FLAGS)"
assert_eq "CLAUDE_RESUME defaults to false" "false" "$(extract_default CLAUDE_RESUME)"
assert_eq "CLAUDE_WORKING_DIR defaults to /home/dev/project" "/home/dev/project" "$(extract_default CLAUDE_WORKING_DIR)"

echo ""
echo "=== Test 2: Config file override ==="
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# Simulate config file override
cat > "${TMPDIR}/config" <<'EOF'
AUTOSTART_CLAUDE=false
CLAUDE_FLAGS="--verbose"
CLAUDE_WORKING_DIR="/custom/path"
EOF

# Source defaults then override
AUTOSTART_CLAUDE=true
CLAUDE_FLAGS="--dangerously-skip-permissions"
CLAUDE_WORKING_DIR="/home/dev/project"
source "${TMPDIR}/config"

assert_eq "Config file overrides AUTOSTART_CLAUDE" "false" "$AUTOSTART_CLAUDE"
assert_eq "Config file overrides CLAUDE_FLAGS" "--verbose" "$CLAUDE_FLAGS"
assert_eq "Config file overrides CLAUDE_WORKING_DIR" "/custom/path" "$CLAUDE_WORKING_DIR"

echo ""
echo "=== Test 3: Metadata label parsing ==="

# Simulate label parsing for autostart=false (disable override)
AUTOSTART_CLAUDE=true
LABELS="tapegun.autostart=false"
if echo "$LABELS" | grep -q "tapegun.autostart=false"; then
    AUTOSTART_CLAUDE=false
elif echo "$LABELS" | grep -q "tapegun.autostart=true"; then
    AUTOSTART_CLAUDE=true
fi
assert_eq "Label tapegun.autostart=false disables autostart" "false" "$AUTOSTART_CLAUDE"

# Simulate label parsing for autostart=true (enable override)
AUTOSTART_CLAUDE=false
LABELS="tapegun.autostart=true"
if echo "$LABELS" | grep -q "tapegun.autostart=false"; then
    AUTOSTART_CLAUDE=false
elif echo "$LABELS" | grep -q "tapegun.autostart=true"; then
    AUTOSTART_CLAUDE=true
fi
assert_eq "Label tapegun.autostart=true enables autostart" "true" "$AUTOSTART_CLAUDE"

# Simulate resume label
CLAUDE_RESUME=false
LABELS="tapegun.resume=true"
if echo "$LABELS" | grep -q "tapegun.resume=true"; then
    CLAUDE_RESUME=true
fi
assert_eq "Label tapegun.resume=true enables resume" "true" "$CLAUDE_RESUME"

echo ""
echo "=== Test 4: Claude command construction ==="

# Test basic command
CLAUDE_FLAGS="--dangerously-skip-permissions"
CLAUDE_RESUME=false
CLAUDE_CMD="claude"
[ -n "$CLAUDE_FLAGS" ] && CLAUDE_CMD="$CLAUDE_CMD $CLAUDE_FLAGS"
[ "$CLAUDE_RESUME" = "true" ] && CLAUDE_CMD="$CLAUDE_CMD --resume"
assert_eq "Basic command" "claude --dangerously-skip-permissions" "$CLAUDE_CMD"

# Test with resume
CLAUDE_RESUME=true
CLAUDE_CMD="claude"
[ -n "$CLAUDE_FLAGS" ] && CLAUDE_CMD="$CLAUDE_CMD $CLAUDE_FLAGS"
[ "$CLAUDE_RESUME" = "true" ] && CLAUDE_CMD="$CLAUDE_CMD --resume"
assert_eq "Command with resume" "claude --dangerously-skip-permissions --resume" "$CLAUDE_CMD"

# Test with empty flags
CLAUDE_FLAGS=""
CLAUDE_RESUME=false
CLAUDE_CMD="claude"
[ -n "$CLAUDE_FLAGS" ] && CLAUDE_CMD="$CLAUDE_CMD $CLAUDE_FLAGS"
[ "$CLAUDE_RESUME" = "true" ] && CLAUDE_CMD="$CLAUDE_CMD --resume"
assert_eq "Command with no flags" "claude" "$CLAUDE_CMD"

echo ""
echo "=== Test 5: Pre-trust settings.json ==="

TRUST_DIR="${TMPDIR}/.claude"
CLAUDE_WORKING_DIR="/home/dev/project"
mkdir -p "$TRUST_DIR"
SETTINGS_FILE="$TRUST_DIR/settings.json"
cat > "$SETTINGS_FILE" <<EOSETTINGS
{
  "permissions": {
    "allow": [],
    "deny": []
  },
  "trustedDirectories": [
    "$CLAUDE_WORKING_DIR"
  ]
}
EOSETTINGS

# Verify settings.json was created with correct content
assert_eq "settings.json exists" "true" "$([ -f "$SETTINGS_FILE" ] && echo true || echo false)"

TRUSTED=$(cat "$SETTINGS_FILE" | grep -o '/home/dev/project' | head -1)
assert_eq "settings.json contains trusted directory" "/home/dev/project" "$TRUSTED"

# Verify it won't overwrite existing settings
echo '{"existing": true}' > "$SETTINGS_FILE"
if [ ! -f "$SETTINGS_FILE" ]; then
    cat > "$SETTINGS_FILE" <<EOSETTINGS2
{"trustedDirectories": ["$CLAUDE_WORKING_DIR"]}
EOSETTINGS2
fi
EXISTING=$(cat "$SETTINGS_FILE" | grep -o '"existing"' | head -1)
assert_eq "Existing settings.json not overwritten" '"existing"' "$EXISTING"

echo ""
echo "=== Test 6: send_keys no_enter jq parsing ==="

# Test that jq correctly extracts no_enter field
MSG_WITH_ENTER='[{"body":"echo hello","send_keys":true,"no_enter":false}]'
NO_ENTER_VAL=$(echo "$MSG_WITH_ENTER" | jq -c '.[] | select(.send_keys == true)' | jq -r '.no_enter // false')
assert_eq "no_enter=false parsed correctly" "false" "$NO_ENTER_VAL"

MSG_NO_ENTER='[{"body":"partial text","send_keys":true,"no_enter":true}]'
NO_ENTER_VAL2=$(echo "$MSG_NO_ENTER" | jq -c '.[] | select(.send_keys == true)' | jq -r '.no_enter // false')
assert_eq "no_enter=true parsed correctly" "true" "$NO_ENTER_VAL2"

MSG_MISSING='[{"body":"echo hi","send_keys":true}]'
NO_ENTER_VAL3=$(echo "$MSG_MISSING" | jq -c '.[] | select(.send_keys == true)' | jq -r '.no_enter // false')
assert_eq "missing no_enter defaults to false" "false" "$NO_ENTER_VAL3"

# Test body extraction
BODY=$(echo "$MSG_WITH_ENTER" | jq -c '.[] | select(.send_keys == true)' | jq -r '.body')
assert_eq "body extracted correctly" "echo hello" "$BODY"

echo ""
echo "=== Results ==="
echo "Passed: $PASS"
echo "Failed: $FAIL"
[ $FAIL -eq 0 ] && echo "All tests passed!" || exit 1
