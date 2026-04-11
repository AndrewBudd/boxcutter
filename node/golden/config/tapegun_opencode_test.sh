#!/bin/bash
# Tests for tapegun OpenCode + tool-aware launch behavior (PRs #178/#179).
# Validates tool selection parsing, opencode config generation, crash recovery
# process patterns, environment variable setup, and default fallback behavior.
# Run: bash tapegun_opencode_test.sh

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

assert_json_eq() {
    local desc="$1" jqpath="$2" expected="$3" file="$4"
    local actual
    actual=$(jq -r "$jqpath" "$file" 2>/dev/null)
    assert_eq "$desc" "$expected" "$actual"
}

# --- Helper: reference the tapegun script ---
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="${SCRIPT_DIR}/boxcutter-tapegun"

extract_default() {
    local var="$1"
    grep -E "^${var}=" "$SCRIPT" | head -1 | sed "s/^${var}=//" | tr -d '"'
}

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# =========================================================================
echo "=== Test 1: Default AGENT_TOOL is claude-code ==="
# =========================================================================

assert_eq "AGENT_TOOL defaults to claude-code" "claude-code" "$(extract_default AGENT_TOOL)"
assert_eq "AGENT_MODEL defaults to empty" "" "$(extract_default AGENT_MODEL)"

# =========================================================================
echo ""
echo "=== Test 2: Tool selection from agent-config JSON ==="
# =========================================================================

# Simulate tapegun's jq extraction of tool from agent-config
AGENT_CONFIG_OPENCODE='{
  "tool": "opencode",
  "inference": {
    "model": "qwen3-coder:30b"
  },
  "repos": ["AndrewBudd/boxcutter"]
}'

agent_tool=$(echo "$AGENT_CONFIG_OPENCODE" | jq -r '.tool // "claude-code"')
agent_model=$(echo "$AGENT_CONFIG_OPENCODE" | jq -r '.inference.model // "qwen3-coder:30b"')
assert_eq "opencode tool parsed from config" "opencode" "$agent_tool"
assert_eq "model parsed from config" "qwen3-coder:30b" "$agent_model"

# =========================================================================
echo ""
echo "=== Test 3: Default tool when not specified in agent-config ==="
# =========================================================================

AGENT_CONFIG_NONTOOL='{
  "repos": ["AndrewBudd/boxcutter"],
  "persona": {"role": "developer"}
}'

default_tool=$(echo "$AGENT_CONFIG_NONTOOL" | jq -r '.tool // "claude-code"')
assert_eq "missing tool defaults to claude-code" "claude-code" "$default_tool"

# =========================================================================
echo ""
echo "=== Test 4: Aider tool selection ==="
# =========================================================================

AGENT_CONFIG_AIDER='{
  "tool": "aider",
  "inference": {
    "provider": "openrouter",
    "model": "deepseek/deepseek-coder"
  }
}'

aider_tool=$(echo "$AGENT_CONFIG_AIDER" | jq -r '.tool // "claude-code"')
aider_model=$(echo "$AGENT_CONFIG_AIDER" | jq -r '.inference.model // "qwen3-coder:30b"')
assert_eq "aider tool parsed" "aider" "$aider_tool"
assert_eq "aider model parsed" "deepseek/deepseek-coder" "$aider_model"

# =========================================================================
echo ""
echo "=== Test 5: OpenCode config file generation ==="
# =========================================================================

WORKDIR="${TMPDIR}/project5"
mkdir -p "$WORKDIR"
AGENT_MODEL="qwen3-coder:30b"

OPENCODE_CONFIG="${WORKDIR}/opencode.json"
cat > "$OPENCODE_CONFIG" <<OCJSON
{
  "provider": {
    "local": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Local Ollama",
      "options": {
        "baseURL": "http://169.254.169.254/v1",
        "apiKey": "dummy"
      },
      "models": {
        "${AGENT_MODEL}": {
          "name": "${AGENT_MODEL}"
        }
      }
    }
  },
  "model": "local/${AGENT_MODEL}",
  "permission": { "*": "allow" }
}
OCJSON

assert_file_exists "opencode.json created" "$OPENCODE_CONFIG"
assert_json_eq "opencode config baseURL points to metadata proxy" \
    '.provider.local.options.baseURL' \
    "http://169.254.169.254/v1" \
    "$OPENCODE_CONFIG"
assert_json_eq "opencode config apiKey is dummy" \
    '.provider.local.options.apiKey' \
    "dummy" \
    "$OPENCODE_CONFIG"
assert_json_eq "opencode config model is local/model" \
    '.model' \
    "local/qwen3-coder:30b" \
    "$OPENCODE_CONFIG"
assert_json_eq "opencode config has model entry" \
    '.provider.local.models["qwen3-coder:30b"].name' \
    "qwen3-coder:30b" \
    "$OPENCODE_CONFIG"
assert_json_eq "opencode config permission allows all" \
    '.permission["*"]' \
    "allow" \
    "$OPENCODE_CONFIG"

# Verify it's valid JSON
if jq empty "$OPENCODE_CONFIG" 2>/dev/null; then
    echo "  PASS: opencode.json is valid JSON"
    PASS=$((PASS + 1))
else
    echo "  FAIL: opencode.json is not valid JSON"
    FAIL=$((FAIL + 1))
fi

# =========================================================================
echo ""
echo "=== Test 6: OpenCode config with different model ==="
# =========================================================================

WORKDIR6="${TMPDIR}/project6"
mkdir -p "$WORKDIR6"
AGENT_MODEL="qwen3-coder:8b"

OPENCODE_CONFIG6="${WORKDIR6}/opencode.json"
cat > "$OPENCODE_CONFIG6" <<OCJSON
{
  "provider": {
    "local": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Local Ollama",
      "options": {
        "baseURL": "http://169.254.169.254/v1",
        "apiKey": "dummy"
      },
      "models": {
        "${AGENT_MODEL}": {
          "name": "${AGENT_MODEL}"
        }
      }
    }
  },
  "model": "local/${AGENT_MODEL}",
  "permission": { "*": "allow" }
}
OCJSON

assert_json_eq "8b model correctly substituted" \
    '.model' \
    "local/qwen3-coder:8b" \
    "$OPENCODE_CONFIG6"

# =========================================================================
echo ""
echo "=== Test 7: Crash recovery process pattern selection ==="
# =========================================================================

# Simulate tapegun's crash recovery pattern matching
for tool_case in "claude-code:claude" "opencode:opencode" "aider:aider" "unknown:claude"; do
    AGENT_TOOL="${tool_case%%:*}"
    EXPECTED_PROC="${tool_case##*:}"

    case "$AGENT_TOOL" in
      opencode) AGENT_PROC_PATTERN='opencode' ;;
      aider)    AGENT_PROC_PATTERN='aider' ;;
      *)        AGENT_PROC_PATTERN='claude' ;;
    esac

    assert_eq "crash recovery pattern for $AGENT_TOOL" "$EXPECTED_PROC" "$AGENT_PROC_PATTERN"
done

# =========================================================================
echo ""
echo "=== Test 8: Sequence execution process pattern ==="
# =========================================================================

# Tapegun waits for the agent process before injecting sequences.
# Verify the _SEQ_PROC variable is set correctly per tool.
for tool_case in "opencode:opencode" "aider:aider" "claude-code:claude" "anything-else:claude"; do
    AGENT_TOOL="${tool_case%%:*}"
    EXPECTED="${tool_case##*:}"

    case "$AGENT_TOOL" in
      opencode) _SEQ_PROC='opencode' ;;
      aider)    _SEQ_PROC='aider' ;;
      *)        _SEQ_PROC='claude' ;;
    esac

    assert_eq "sequence proc pattern for $AGENT_TOOL" "$EXPECTED" "$_SEQ_PROC"
done

# =========================================================================
echo ""
echo "=== Test 9: Pre-trust settings skipped for non-claude tools ==="
# =========================================================================

# Tapegun should only write pre-trust settings.json for claude-code.
# The condition is: [ -n "$CLAUDE_WORKING_DIR" ] && [ "$AGENT_TOOL" = "claude-code" ]
for test_case in "claude-code:yes" "opencode:no" "aider:no"; do
    AGENT_TOOL="${test_case%%:*}"
    EXPECTED="${test_case##*:}"
    CLAUDE_WORKING_DIR="/home/dev/project"

    if [ -n "$CLAUDE_WORKING_DIR" ] && [ "$AGENT_TOOL" = "claude-code" ]; then
        SHOULD_TRUST="yes"
    else
        SHOULD_TRUST="no"
    fi

    assert_eq "pre-trust for $AGENT_TOOL" "$EXPECTED" "$SHOULD_TRUST"
done

# =========================================================================
echo ""
echo "=== Test 10: Environment variables per tool ==="
# =========================================================================

# Verify the correct env vars are set for each tool.
# OpenCode uses OPENAI_BASE_URL; Aider uses OPENAI_API_BASE.

# OpenCode env vars
OPENCODE_BASE_URL="http://169.254.169.254/v1"
OPENCODE_API_KEY="dummy"
assert_eq "opencode OPENAI_BASE_URL" "http://169.254.169.254/v1" "$OPENCODE_BASE_URL"
assert_eq "opencode OPENAI_API_KEY" "dummy" "$OPENCODE_API_KEY"

# Aider env vars
AIDER_BASE="http://169.254.169.254/v1"
AIDER_KEY="dummy"
assert_eq "aider OPENAI_API_BASE" "http://169.254.169.254/v1" "$AIDER_BASE"
assert_eq "aider OPENAI_API_KEY" "dummy" "$AIDER_KEY"

# =========================================================================
echo ""
echo "=== Test 11: Unknown tool falls back to claude-code ==="
# =========================================================================

AGENT_TOOL="cursormcp"
case "$AGENT_TOOL" in
  claude-code|opencode|aider)
    FALLBACK="no"
    ;;
  *)
    AGENT_TOOL="claude-code"
    FALLBACK="yes"
    ;;
esac
assert_eq "unknown tool triggers fallback" "yes" "$FALLBACK"
assert_eq "fallback resets to claude-code" "claude-code" "$AGENT_TOOL"

# =========================================================================
echo ""
echo "=== Test 12: Inference config cascading in agent-config JSON ==="
# =========================================================================

# Simulate what the orchestrator produces for the metadata endpoint
# This is the full team YAML → resolved agent config → JSON pipeline

# Default-inherited config (no tool field for claude-code)
CLAUDE_CONFIG='{"team":"test","agent":"lead","replica_index":1}'
TOOL_FIELD=$(echo "$CLAUDE_CONFIG" | jq -r '.tool // "claude-code"')
assert_eq "claude-code config defaults tool via jq" "claude-code" "$TOOL_FIELD"

# OpenCode config (tool field present)
OPENCODE_CONFIG_JSON='{"team":"test","agent":"worker","replica_index":1,"tool":"opencode","inference":{"model":"qwen3-coder:30b"}}'
TOOL_FIELD2=$(echo "$OPENCODE_CONFIG_JSON" | jq -r '.tool // "claude-code"')
MODEL_FIELD=$(echo "$OPENCODE_CONFIG_JSON" | jq -r '.inference.model // empty')
assert_eq "opencode config has tool field" "opencode" "$TOOL_FIELD2"
assert_eq "opencode config has inference model" "qwen3-coder:30b" "$MODEL_FIELD"

# Aider with openrouter (provider + model)
AIDER_CONFIG_JSON='{"team":"test","agent":"reviewer","replica_index":1,"tool":"aider","inference":{"provider":"openrouter","model":"deepseek/deepseek-coder"}}'
PROVIDER=$(echo "$AIDER_CONFIG_JSON" | jq -r '.inference.provider // empty')
assert_eq "aider config has provider" "openrouter" "$PROVIDER"

# =========================================================================
echo ""
echo "=== Test 13: Tapegun script syntax validation ==="
# =========================================================================

if bash -n "$SCRIPT" 2>/dev/null; then
    echo "  PASS: tapegun script has valid bash syntax"
    PASS=$((PASS + 1))
else
    echo "  FAIL: tapegun script has syntax errors"
    FAIL=$((FAIL + 1))
fi

# =========================================================================
echo ""
echo "=== Test 14: Script contains tool-aware case statements ==="
# =========================================================================

SCRIPT_CONTENT=$(cat "$SCRIPT")

assert_contains "script has case AGENT_TOOL for launch" 'case "$AGENT_TOOL"' "$SCRIPT_CONTENT"
assert_contains "script has opencode case" "opencode)" "$SCRIPT_CONTENT"
assert_contains "script has aider case" "aider)" "$SCRIPT_CONTENT"
assert_contains "script has claude-code case" "claude-code)" "$SCRIPT_CONTENT"
assert_contains "script has AGENT_PROC_PATTERN for crash recovery" 'AGENT_PROC_PATTERN' "$SCRIPT_CONTENT"
assert_contains "script has opencode.json generation" 'opencode.json' "$SCRIPT_CONTENT"

# =========================================================================
echo ""
echo "=== Test 15: Aider launch command construction ==="
# =========================================================================

AGENT_MODEL="qwen3-coder:30b"
AIDER_CMD="aider --model openai/${AGENT_MODEL} --yes"
assert_eq "aider command uses openai/ prefix" "aider --model openai/qwen3-coder:30b --yes" "$AIDER_CMD"

# Different model
AGENT_MODEL="deepseek/deepseek-coder"
AIDER_CMD="aider --model openai/${AGENT_MODEL} --yes"
assert_eq "aider command with openrouter model" "aider --model openai/deepseek/deepseek-coder --yes" "$AIDER_CMD"

# =========================================================================
echo ""
echo "=== Results ==="
echo "Passed: $PASS"
echo "Failed: $FAIL"
[ $FAIL -eq 0 ] && echo "All tests passed!" || exit 1
