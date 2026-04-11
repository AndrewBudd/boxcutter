#!/bin/bash
# Tests for tapegun credential provisioning (setup_claude_credentials function).
# Validates that tapegun correctly fetches credentials from the metadata service
# and writes them to ~/.claude/.credentials.json.
#
# These tests simulate the metadata service with a local Node.js HTTP server
# and exercise the tapegun setup functions in isolation.

set -euo pipefail

TESTS_PASSED=0
TESTS_FAILED=0
TESTS_TOTAL=0

pass() { TESTS_PASSED=$((TESTS_PASSED + 1)); TESTS_TOTAL=$((TESTS_TOTAL + 1)); echo "  PASS: $1"; }
fail() { TESTS_FAILED=$((TESTS_FAILED + 1)); TESTS_TOTAL=$((TESTS_TOTAL + 1)); echo "  FAIL: $1"; }

TEST_DIR=$(mktemp -d)
SERVE_DIR="$TEST_DIR/serve"
mkdir -p "$SERVE_DIR/metadata"
METADATA_PID=""
METADATA_PORT=""

cleanup() {
    [ -n "$METADATA_PID" ] && kill "$METADATA_PID" 2>/dev/null || true
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# Start metadata mock server with Node.js
PORT_FILE="$TEST_DIR/port"
node -e "
const http = require('http'), fs = require('fs'), path = require('path');
const sd = '${SERVE_DIR}';
const server = http.createServer((req, res) => {
    if (req.url === '/metadata/claude-credentials') {
        const f = path.join(sd, 'metadata', 'claude-credentials.json');
        try { const d = fs.readFileSync(f, 'utf8'); res.writeHead(200, {'Content-Type':'application/json'}); res.end(d); }
        catch(e) { res.writeHead(404); res.end('not configured'); }
    } else if (req.url === '/identity') {
        res.writeHead(200, {'Content-Type':'application/json'}); res.end('{\"vm_id\":\"test\",\"labels\":{}}');
    } else { res.writeHead(404); res.end(''); }
});
server.listen(0, '127.0.0.1', () => { fs.writeFileSync('${PORT_FILE}', String(server.address().port)); });
" &
METADATA_PID=$!

# Wait for port file
for _i in $(seq 1 30); do [ -f "$PORT_FILE" ] && break; sleep 0.1; done
METADATA_PORT=$(cat "$PORT_FILE")

# Wait for server ready
for _i in $(seq 1 20); do
    curl -sf "http://127.0.0.1:${METADATA_PORT}/identity" >/dev/null 2>&1 && break
    sleep 0.1
done

# Define log as no-op
log() { :; }

# Source setup_claude_credentials from the tapegun script
eval "$(sed -n '/^setup_claude_credentials()/,/^}/p' "$(dirname "$0")/boxcutter-tapegun")"

# Helper to initialize a clean test home directory
init_env() {
    HOME_DIR="$TEST_DIR/$1"
    CLAUDE_DIR="$HOME_DIR/.claude"
    METADATA="http://127.0.0.1:${METADATA_PORT}"
    mkdir -p "$HOME_DIR"
}

# =============================================================================
# Test 1: Golden path — fetch credentials from metadata
# =============================================================================
echo "Test 1: setup_claude_credentials fetches and writes credentials"

cat > "$SERVE_DIR/metadata/claude-credentials.json" <<'EOF'
{"oauth_refresh_token":"rt_golden_test","oauth_client_id":"client_123"}
EOF

init_env home1
setup_claude_credentials

CRED_FILE="$CLAUDE_DIR/.credentials.json"
if [ -f "$CRED_FILE" ]; then
    pass "credential file created"
else
    fail "credential file not created at $CRED_FILE"
fi

if [ -f "$CRED_FILE" ]; then
    CONTENT=$(cat "$CRED_FILE")
    if echo "$CONTENT" | jq -e '.oauth_refresh_token == "rt_golden_test"' >/dev/null 2>&1; then
        pass "oauth_refresh_token correct"
    else
        fail "oauth_refresh_token mismatch: $CONTENT"
    fi
    if echo "$CONTENT" | jq -e '.oauth_client_id == "client_123"' >/dev/null 2>&1; then
        pass "oauth_client_id correct"
    else
        fail "oauth_client_id mismatch: $CONTENT"
    fi
fi

# Check file permissions (should be 600)
if [ -f "$CRED_FILE" ]; then
    PERMS=$(stat -c '%a' "$CRED_FILE" 2>/dev/null || stat -f '%Lp' "$CRED_FILE" 2>/dev/null)
    if [ "$PERMS" = "600" ]; then
        pass "credential file permissions are 600"
    else
        fail "credential file permissions are $PERMS, want 600"
    fi
fi

# =============================================================================
# Test 2: Credentials already present — skip fetch
# =============================================================================
echo "Test 2: setup_claude_credentials skips when credentials already exist"

init_env home2
mkdir -p "$CLAUDE_DIR"
echo '{"oauth_refresh_token":"existing_token"}' > "$CLAUDE_DIR/.credentials.json"

setup_claude_credentials

CONTENT=$(cat "$CLAUDE_DIR/.credentials.json")
if echo "$CONTENT" | jq -e '.oauth_refresh_token == "existing_token"' >/dev/null 2>&1; then
    pass "existing credentials preserved (not overwritten)"
else
    fail "existing credentials were overwritten: $CONTENT"
fi

# =============================================================================
# Test 3: Metadata returns 404 — graceful degradation
# =============================================================================
echo "Test 3: setup_claude_credentials handles missing credentials gracefully"

rm -f "$SERVE_DIR/metadata/claude-credentials.json"

init_env home3
setup_claude_credentials

if [ ! -f "$CLAUDE_DIR/.credentials.json" ]; then
    pass "no credential file created when metadata returns 404"
else
    fail "credential file should not exist on 404"
fi

# =============================================================================
# Test 4: Credential file content is valid JSON
# =============================================================================
echo "Test 4: credential file is valid JSON that Claude Code can parse"

cat > "$SERVE_DIR/metadata/claude-credentials.json" <<'EOF'
{"oauth_refresh_token":"rt_json_test","oauth_client_id":"cid_json_test","expires_in":3600}
EOF

init_env home4
setup_claude_credentials

if [ -f "$CLAUDE_DIR/.credentials.json" ]; then
    if jq empty "$CLAUDE_DIR/.credentials.json" 2>/dev/null; then
        pass "credential file is valid JSON"
    else
        fail "credential file is not valid JSON"
    fi
    if jq -e '.oauth_refresh_token' "$CLAUDE_DIR/.credentials.json" >/dev/null 2>&1; then
        pass "oauth_refresh_token field present"
    else
        fail "missing oauth_refresh_token in credential file"
    fi
fi

# =============================================================================
# Test 5: Metadata server unreachable — graceful timeout
# =============================================================================
echo "Test 5: setup_claude_credentials handles unreachable metadata gracefully"

init_env home5
METADATA="http://127.0.0.1:19999"  # unreachable port

setup_claude_credentials

if [ ! -f "$CLAUDE_DIR/.credentials.json" ]; then
    pass "no credential file created when metadata unreachable"
else
    fail "credential file should not exist when metadata is unreachable"
fi

# =============================================================================
# Test 6: Creates .claude directory if missing
# =============================================================================
echo "Test 6: setup_claude_credentials creates .claude directory if missing"

cat > "$SERVE_DIR/metadata/claude-credentials.json" <<'EOF'
{"oauth_refresh_token":"rt_mkdir_test","oauth_client_id":"cid_test"}
EOF

init_env home6
METADATA="http://127.0.0.1:${METADATA_PORT}"
# Do NOT create CLAUDE_DIR — function should create it

setup_claude_credentials

if [ -d "$CLAUDE_DIR" ]; then
    pass ".claude directory created automatically"
else
    fail ".claude directory not created"
fi
if [ -f "$CLAUDE_DIR/.credentials.json" ]; then
    pass "credential file created in new .claude directory"
else
    fail "credential file not created in new .claude directory"
fi

# =============================================================================
# Summary
# =============================================================================
echo ""
echo "Results: $TESTS_PASSED passed, $TESTS_FAILED failed, $TESTS_TOTAL total"
[ "$TESTS_FAILED" -gt 0 ] && exit 1
exit 0
