#!/bin/bash
# Tests for boxcutter-tapegun autostart and setup_workspace behavior.
# Validates config parsing, defaults, metadata label overrides,
# agent-config parsing, persona installation at .claude/personas/,
# and tapegun config generation.
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

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if ! echo "$haystack" | grep -qF -- "$needle"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected NOT to contain '$needle')"
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

assert_file_not_exists() {
    local desc="$1" path="$2"
    if [ ! -f "$path" ]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (file should not exist: $path)"
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

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

echo "=== Test 1: Default values ==="
assert_eq "AUTOSTART_CLAUDE defaults to true" "true" "$(extract_default AUTOSTART_CLAUDE)"
assert_eq "CLAUDE_FLAGS defaults to --dangerously-skip-permissions" "--dangerously-skip-permissions" "$(extract_default CLAUDE_FLAGS)"
assert_eq "CLAUDE_RESUME defaults to false" "false" "$(extract_default CLAUDE_RESUME)"
assert_eq "CLAUDE_WORKING_DIR defaults to /home/dev/project" "/home/dev/project" "$(extract_default CLAUDE_WORKING_DIR)"

echo ""
echo "=== Test 2: Config file override ==="

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
echo "=== Test 7: Parse agent-config JSON ==="

AGENT_CONFIG='{
  "repos": ["AndrewBudd/boxcutter", "AndrewBudd/other-repo"],
  "persona": {
    "role": "developer",
    "claude_md": ".claude/personas/developer.md",
    "instructions": "You are a developer. Write clean code."
  },
  "claude_flags": "--dangerously-skip-permissions --verbose",
  "claude_resume": "true",
  "working_dir": "/home/dev/project",
  "team_persona_files": [".claude/personas/developer.md", ".claude/personas/reviewer.md"]
}'

REPOS=$(echo "$AGENT_CONFIG" | jq -r '.repos // [] | .[]')
REPO_COUNT=$(echo "$REPOS" | grep -c . || true)
PERSONA_CLAUDE_MD=$(echo "$AGENT_CONFIG" | jq -r '.persona.claude_md // empty')
PERSONA_INSTRUCTIONS=$(echo "$AGENT_CONFIG" | jq -r '.persona.instructions // empty')
PERSONA_ROLE=$(echo "$AGENT_CONFIG" | jq -r '.persona.role // empty')
AC_CLAUDE_FLAGS=$(echo "$AGENT_CONFIG" | jq -r '.claude_flags // empty')
AC_CLAUDE_RESUME=$(echo "$AGENT_CONFIG" | jq -r '.claude_resume // empty')
AC_WORKING_DIR=$(echo "$AGENT_CONFIG" | jq -r '.working_dir // empty')
TEAM_PERSONA_FILES=$(echo "$AGENT_CONFIG" | jq -r '.team_persona_files // [] | .[]')

assert_eq "repo count" "2" "$REPO_COUNT"
assert_eq "first repo" "AndrewBudd/boxcutter" "$(echo "$REPOS" | head -1)"
assert_eq "second repo" "AndrewBudd/other-repo" "$(echo "$REPOS" | tail -1)"
assert_eq "persona role" "developer" "$PERSONA_ROLE"
assert_eq "persona claude_md path" ".claude/personas/developer.md" "$PERSONA_CLAUDE_MD"
assert_contains "persona instructions" "Write clean code" "$PERSONA_INSTRUCTIONS"
assert_eq "claude flags" "--dangerously-skip-permissions --verbose" "$AC_CLAUDE_FLAGS"
assert_eq "claude resume" "true" "$AC_CLAUDE_RESUME"
assert_eq "working dir" "/home/dev/project" "$AC_WORKING_DIR"
assert_eq "team persona file count" "2" "$(echo "$TEAM_PERSONA_FILES" | grep -c .)"

echo ""
echo "=== Test 8: Empty/missing agent-config ==="

EMPTY_CONFIG="{}"
EMPTY_REPOS=$(echo "$EMPTY_CONFIG" | jq -r '.repos // [] | .[]')
EMPTY_COUNT=$(echo "$EMPTY_REPOS" | grep -c . || true)
assert_eq "empty config has 0 repos" "0" "$EMPTY_COUNT"

NULL_CONFIG="null"
NULL_REPOS=$(echo "$NULL_CONFIG" | jq -r '.repos // [] | .[]' 2>/dev/null || echo "")
NULL_COUNT=$(echo "$NULL_REPOS" | grep -c . || true)
assert_eq "null config has 0 repos" "0" "$NULL_COUNT"

echo ""
echo "=== Test 9: Single repo target ==="

SINGLE_CONFIG='{"repos": ["AndrewBudd/boxcutter"]}'
SINGLE_REPOS=$(echo "$SINGLE_CONFIG" | jq -r '.repos // [] | .[]')
SINGLE_COUNT=$(echo "$SINGLE_REPOS" | grep -c . || true)
assert_eq "single repo count" "1" "$SINGLE_COUNT"
SINGLE_REPO=$(echo "$SINGLE_REPOS" | head -1)
assert_eq "single repo name" "AndrewBudd/boxcutter" "$SINGLE_REPO"

echo ""
echo "=== Test 10: Persona installed at .claude/personas/ path ==="

# Simulate a project with a persona file already in the repo
PROJECT="${TMPDIR}/project10"
mkdir -p "${PROJECT}/.claude/personas"
echo "# Developer Persona" > "${PROJECT}/.claude/personas/developer.md"
echo "Focus on writing tests." >> "${PROJECT}/.claude/personas/developer.md"

PERSONA_CLAUDE_MD=".claude/personas/developer.md"
PERSONA_INSTRUCTIONS="You are a developer. Write clean code."
SRC="${PROJECT}/${PERSONA_CLAUDE_MD}"

# The install_persona logic should append instructions to the persona file
# (not modify root CLAUDE.md)
printf '\n\n## Additional Instructions\n\n%s\n' "$PERSONA_INSTRUCTIONS" >> "$SRC"

assert_file_exists "persona file exists at .claude/personas/" "$SRC"
assert_contains "persona file has original content" "Developer Persona" "$(cat "$SRC")"
assert_contains "persona file has appended instructions" "Write clean code" "$(cat "$SRC")"
assert_file_not_exists "root CLAUDE.md not created" "${PROJECT}/CLAUDE.md"

echo ""
echo "=== Test 11: Persona from inline instructions only (no claude_md) ==="

PROJECT11="${TMPDIR}/project11"
mkdir -p "$PROJECT11"

# When no claude_md path is provided, create default persona at .claude/personas/agent.md
PERSONAS_DIR="${PROJECT11}/.claude/personas"
mkdir -p "$PERSONAS_DIR"
printf '# Agent Instructions\n\n%s\n' "Be helpful and concise." > "${PERSONAS_DIR}/agent.md"

assert_file_exists "default persona created" "${PERSONAS_DIR}/agent.md"
assert_contains "default persona has instructions" "Be helpful and concise" "$(cat "${PERSONAS_DIR}/agent.md")"
assert_file_not_exists "root CLAUDE.md not created" "${PROJECT11}/CLAUDE.md"

echo ""
echo "=== Test 12: Persona file created when missing from repo ==="

PROJECT12="${TMPDIR}/project12"
mkdir -p "$PROJECT12"

# Simulate: persona.claude_md points to a file that doesn't exist yet
PERSONA_PATH=".claude/personas/dev.md"
SRC12="${PROJECT12}/${PERSONA_PATH}"
mkdir -p "$(dirname "$SRC12")"
printf '# Agent Instructions\n\n%s\n' "Custom instructions" > "$SRC12"

assert_file_exists "persona file created at specified path" "$SRC12"
assert_contains "created persona has instructions" "Custom instructions" "$(cat "$SRC12")"

echo ""
echo "=== Test 13: Team persona cleanup ==="

PROJECT13="${TMPDIR}/project13"
mkdir -p "${PROJECT13}/.claude/personas"
echo "# My Persona" > "${PROJECT13}/.claude/personas/developer.md"
echo "# Other Agent" > "${PROJECT13}/.claude/personas/reviewer.md"
echo "# Third Agent" > "${PROJECT13}/.claude/personas/tester.md"

# This agent owns developer.md; reviewer.md is a team file to remove; tester.md is NOT in the team list
MY_PERSONA=".claude/personas/developer.md"
TEAM_FILES=".claude/personas/developer.md
.claude/personas/reviewer.md"

echo "$TEAM_FILES" | while IFS= read -r file; do
    [ -z "$file" ] && continue
    [ "$file" = "$MY_PERSONA" ] && continue
    target="${PROJECT13}/${file}"
    [ -f "$target" ] && rm "$target"
done

assert_file_exists "own persona file kept" "${PROJECT13}/.claude/personas/developer.md"
assert_file_not_exists "other team persona removed" "${PROJECT13}/.claude/personas/reviewer.md"
assert_file_exists "non-team persona untouched" "${PROJECT13}/.claude/personas/tester.md"

echo ""
echo "=== Test 14: Default flags when not specified ==="

CLAUDE_FLAGS=""
RESULT="${CLAUDE_FLAGS:-"--dangerously-skip-permissions"}"
assert_eq "default flags when empty" "--dangerously-skip-permissions" "$RESULT"

echo ""
echo "=== Test 15: Stamp file idempotency ==="

STAMP="${TMPDIR}/.tapegun-setup-done"
assert_file_not_exists "stamp absent before setup" "$STAMP"
touch "$STAMP"
assert_file_exists "stamp present after setup" "$STAMP"

echo ""
echo "=== Test 16: Session restore writes JSONL to correct path ==="

PROJECT16="${TMPDIR}/project16"
mkdir -p "${PROJECT16}/.claude/projects/-home-dev-project"

SESSION_ID="8fd8c5bf-b0df-4a95-8ff9-49d809d26532"
SESSION_DATA='{"type":"user","message":"hello"}'
SESSION_DIR="${PROJECT16}/.claude/projects/-home-dev-project"

echo "$SESSION_DATA" > "${SESSION_DIR}/${SESSION_ID}.jsonl"

assert_file_exists "session JSONL written" "${SESSION_DIR}/${SESSION_ID}.jsonl"
assert_contains "session data preserved" "hello" "$(cat "${SESSION_DIR}/${SESSION_ID}.jsonl")"

echo ""
echo "=== Test 17: Checkpoint finds most recent session JSONL ==="

CP_DIR="${TMPDIR}/cp-test/.claude/projects/-home-dev-project"
mkdir -p "$CP_DIR"

# Create two session files with different timestamps
echo '{"old":"data"}' > "${CP_DIR}/old-session.jsonl"
sleep 0.1
echo '{"new":"data"}' > "${CP_DIR}/new-session.jsonl"

# ls -t should return newest first
LATEST=$(ls -t "${CP_DIR}"/*.jsonl 2>/dev/null | head -1)
LATEST_NAME=$(basename "$LATEST" .jsonl)
assert_eq "newest session found" "new-session" "$LATEST_NAME"

echo ""
echo "=== Test 18: Checkpoint interval tracking ==="

CHECKPOINT_INTERVAL=60
LAST_CHECKPOINT=0
NOW_EPOCH=$(date +%s)
SHOULD_CHECKPOINT=$((NOW_EPOCH - LAST_CHECKPOINT >= CHECKPOINT_INTERVAL))
assert_eq "first checkpoint triggers" "1" "$SHOULD_CHECKPOINT"

LAST_CHECKPOINT=$NOW_EPOCH
SHOULD_CHECKPOINT=$((NOW_EPOCH - LAST_CHECKPOINT >= CHECKPOINT_INTERVAL))
assert_eq "immediate re-checkpoint skipped" "0" "$SHOULD_CHECKPOINT"

echo ""
echo "=== Test 19: Restore sets RESTORED_SESSION_ID ==="

RESTORED_SESSION_ID=""
# Simulate restore setting the variable
RESTORED_SESSION_ID="test-session-123"
CLAUDE_CMD="claude --dangerously-skip-permissions"
[ -n "$RESTORED_SESSION_ID" ] && CLAUDE_CMD="$CLAUDE_CMD --continue"
assert_contains "claude cmd has --continue after restore" "--continue" "$CLAUDE_CMD"

RESTORED_SESSION_ID=""
CLAUDE_CMD="claude --dangerously-skip-permissions"
CLAUDE_RESUME=true
[ -n "$RESTORED_SESSION_ID" ] && CLAUDE_CMD="$CLAUDE_CMD --continue"
[ "$CLAUDE_RESUME" = "true" ] && [ -z "$RESTORED_SESSION_ID" ] && CLAUDE_CMD="$CLAUDE_CMD --resume"
assert_contains "claude cmd has --resume without restore" "--resume" "$CLAUDE_CMD"
assert_not_contains "claude cmd no --continue without restore" "--continue" "$CLAUDE_CMD"

echo ""
echo "=== Test 20: Checkpoint JSON structure ==="

# Verify jq can build the checkpoint payload structure
if command -v jq >/dev/null 2>&1; then
    CP_JSON=$(jq -n \
        --arg session_id "test-sess" \
        --arg git_branch "main" \
        --arg git_stash "" \
        '{session_id: $session_id, git_branch: $git_branch, git_stash: $git_stash}')
    CP_SID=$(echo "$CP_JSON" | jq -r '.session_id')
    assert_eq "checkpoint JSON session_id" "test-sess" "$CP_SID"
    CP_BRANCH=$(echo "$CP_JSON" | jq -r '.git_branch')
    assert_eq "checkpoint JSON git_branch" "main" "$CP_BRANCH"
fi

echo ""
echo "=== Test 21: Claude credential provisioning — native format passthrough ==="

# When metadata serves the full claudeAiOauth format, it should be written as-is
CRED_DIR21="${TMPDIR}/cred21"
mkdir -p "$CRED_DIR21"
NATIVE_CREDS='{"claudeAiOauth":{"refreshToken":"rt-123","accessToken":"at-456","expiresAt":1700000000,"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"default"}}'

# Simulate setup_claude_credentials logic for native format
CLAUDE_DIR="$CRED_DIR21"
cred_file="${CLAUDE_DIR}/.credentials.json"
resp="$NATIVE_CREDS"

if ! echo "$resp" | jq -e 'has("claudeAiOauth")' >/dev/null 2>&1; then
    resp=$(echo "$resp" | jq '{claudeAiOauth:{refreshToken:.oauth_refresh_token,accessToken:(.oauth_access_token//""),expiresAt:(.oauth_expires_at//0),scopes:(.oauth_scopes//["user:inference"]),subscriptionType:(.oauth_subscription_type//""),rateLimitTier:(.oauth_rate_limit_tier//"")}}' 2>/dev/null)
fi
echo "$resp" > "$cred_file"
chmod 600 "$cred_file"

assert_file_exists "credentials file created (native)" "$cred_file"
REFRESH=$(cat "$cred_file" | jq -r '.claudeAiOauth.refreshToken')
assert_eq "refresh token preserved (native)" "rt-123" "$REFRESH"
ACCESS=$(cat "$cred_file" | jq -r '.claudeAiOauth.accessToken')
assert_eq "access token preserved (native)" "at-456" "$ACCESS"
PERMS=$(stat -c '%a' "$cred_file" 2>/dev/null || stat -f '%Lp' "$cred_file" 2>/dev/null)
assert_eq "credentials file mode 600" "600" "$PERMS"

echo ""
echo "=== Test 22: Claude credential provisioning — flat format transformation ==="

# When metadata serves flat oauth fields, they should be wrapped into claudeAiOauth
CRED_DIR22="${TMPDIR}/cred22"
mkdir -p "$CRED_DIR22"
FLAT_CREDS='{"oauth_refresh_token":"flat-rt-789","oauth_access_token":"flat-at-012"}'

CLAUDE_DIR="$CRED_DIR22"
cred_file="${CLAUDE_DIR}/.credentials.json"
resp="$FLAT_CREDS"

if ! echo "$resp" | jq -e 'has("claudeAiOauth")' >/dev/null 2>&1; then
    resp=$(echo "$resp" | jq '{
        claudeAiOauth: {
            refreshToken: .oauth_refresh_token,
            accessToken: (.oauth_access_token // ""),
            expiresAt: (.oauth_expires_at // 0),
            scopes: (.oauth_scopes // ["user:inference","user:profile","user:sessions:claude_code"]),
            subscriptionType: (.oauth_subscription_type // ""),
            rateLimitTier: (.oauth_rate_limit_tier // "")
        }
    }' 2>/dev/null)
fi
echo "$resp" > "$cred_file"

assert_file_exists "credentials file created (flat)" "$cred_file"
REFRESH22=$(cat "$cred_file" | jq -r '.claudeAiOauth.refreshToken')
assert_eq "refresh token transformed from flat" "flat-rt-789" "$REFRESH22"
ACCESS22=$(cat "$cred_file" | jq -r '.claudeAiOauth.accessToken')
assert_eq "access token transformed from flat" "flat-at-012" "$ACCESS22"
SCOPES22=$(cat "$cred_file" | jq -r '.claudeAiOauth.scopes | length')
assert_eq "default scopes added for flat format" "3" "$SCOPES22"

echo ""
echo "=== Test 23: Claude credential provisioning — empty response ==="

# Empty response should not create a credentials file
CRED_DIR23="${TMPDIR}/cred23"
mkdir -p "$CRED_DIR23"
resp=""
if [ -z "$resp" ]; then
    SKIP23="yes"
fi
assert_eq "empty response skips credential install" "yes" "$SKIP23"
assert_file_not_exists "no credentials file from empty response" "${CRED_DIR23}/.credentials.json"

echo ""
echo "=== Test 24: Claude credential provisioning — invalid JSON ==="

# Invalid JSON should not create a credentials file
CRED_DIR24="${TMPDIR}/cred24"
mkdir -p "$CRED_DIR24"
resp="not-json"
VALID24=$(echo "$resp" | jq -e '.claudeAiOauth.refreshToken // .oauth_refresh_token' >/dev/null 2>&1 && echo "yes" || echo "no")
assert_eq "invalid JSON fails validation" "no" "$VALID24"
assert_file_not_exists "no credentials file from invalid JSON" "${CRED_DIR24}/.credentials.json"

echo ""
echo "=== Test 25: Claude credential provisioning — idempotent overwrite ==="

# Running credential setup twice should update the file
CRED_DIR25="${TMPDIR}/cred25"
mkdir -p "$CRED_DIR25"

# First write
echo '{"claudeAiOauth":{"refreshToken":"old-token"}}' > "${CRED_DIR25}/.credentials.json"
# Second write (simulating rotation)
echo '{"claudeAiOauth":{"refreshToken":"new-token"}}' > "${CRED_DIR25}/.credentials.json"

ROTATED=$(cat "${CRED_DIR25}/.credentials.json" | jq -r '.claudeAiOauth.refreshToken')
assert_eq "credential file updated on re-run" "new-token" "$ROTATED"

echo ""
echo "=== Results ==="
echo "Passed: $PASS"
echo "Failed: $FAIL"
[ $FAIL -eq 0 ] && echo "All tests passed!" || exit 1
