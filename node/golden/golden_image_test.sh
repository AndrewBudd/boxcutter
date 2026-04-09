#!/bin/bash
# Tests for the golden image Dockerfile.
# Validates that required tools are declared in the Dockerfile and PATH is configured.
# Run: bash node/golden/golden_image_test.sh
set -e
PASS=0
FAIL=0

assert_contains() {
    local desc="$1" file="$2" pattern="$3"
    if grep -qE "$pattern" "$file"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (pattern '$pattern' not found)"
        FAIL=$((FAIL + 1))
    fi
}

assert_not_contains() {
    local desc="$1" file="$2" pattern="$3"
    if ! grep -qE "$pattern" "$file"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (pattern '$pattern' should not be present)"
        FAIL=$((FAIL + 1))
    fi
}

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCKERFILE="${SCRIPT_DIR}/Dockerfile"

echo "=== Test 1: Core packages installed ==="
# Packages are on continuation lines in the multi-line RUN command
assert_contains "curl installed" "$DOCKERFILE" "curl"
assert_contains "jq installed" "$DOCKERFILE" "jq"
assert_contains "git installed" "$DOCKERFILE" "^\s+.*git\b"
assert_contains "tmux installed" "$DOCKERFILE" "tmux"
assert_contains "openssh-server installed" "$DOCKERFILE" "openssh-server"

echo ""
echo "=== Test 2: GitHub CLI installed ==="
assert_contains "gh CLI keyring added" "$DOCKERFILE" "githubcli-archive-keyring"
assert_contains "gh CLI apt source configured" "$DOCKERFILE" "github-cli.list"
assert_contains "gh package installed" "$DOCKERFILE" "apt-get install.*gh"

echo ""
echo "=== Test 3: mise and language runtimes ==="
assert_contains "mise installed via curl" "$DOCKERFILE" "mise.run"
assert_contains "Node.js 22 via mise" "$DOCKERFILE" "mise use -g node@22"
assert_contains "Ruby 3.2 via mise" "$DOCKERFILE" "mise use -g ruby@3.2"

echo ""
echo "=== Test 4: Claude Code installed ==="
assert_contains "Claude Code native installer" "$DOCKERFILE" "claude.ai/install.sh"

echo ""
echo "=== Test 5: PATH configuration ==="
# PATH setup spans multiple lines — check key elements across the file
assert_contains ".local/bin in PATH setup" "$DOCKERFILE" '\.local/bin'
assert_contains "mise shims in PATH setup" "$DOCKERFILE" 'mise/shims'
assert_contains "mise activate in .bashrc" "$DOCKERFILE" 'mise activate bash'
assert_contains ".bashrc modified" "$DOCKERFILE" '/home/dev/\.bashrc'
assert_contains ".profile modified" "$DOCKERFILE" '/home/dev/\.profile'

echo ""
echo "=== Test 6: Tailscale installed ==="
assert_contains "Tailscale installer" "$DOCKERFILE" "tailscale.com/install.sh"
assert_contains "Tailscale enabled" "$DOCKERFILE" "systemctl enable tailscaled"

echo ""
echo "=== Test 7: dev user configured ==="
assert_contains "dev user created" "$DOCKERFILE" "useradd.*dev"
assert_contains "passwordless sudo" "$DOCKERFILE" "NOPASSWD.*ALL"
assert_contains "SSH directory" "$DOCKERFILE" "mkdir.*\.ssh"

echo ""
echo "=== Test 8: gcc removed after compilation ==="
assert_contains "gcc removed" "$DOCKERFILE" "apt-get remove.*gcc"

echo ""
echo "=== Test 9: Ruby build dependencies ==="
assert_contains "build-essential reinstalled for Ruby" "$DOCKERFILE" "build-essential.*libssl-dev.*libreadline-dev"
assert_contains "build deps cleaned after mise install" "$DOCKERFILE" "apt-get remove.*build-essential"
assert_contains "PATH inserted early in .bashrc (sed -i 2i)" "$DOCKERFILE" "sed -i.*2i.*PATH"

echo ""
echo "=== Test 10: Build order (gh and Claude after gcc removal) ==="
# gh CLI and Claude Code should be installed after gcc is removed (no compile needed)
GCC_LINE=$(grep -n "apt-get remove.*gcc" "$DOCKERFILE" | head -1 | cut -d: -f1)
GH_LINE=$(grep -n "GitHub CLI" "$DOCKERFILE" | head -1 | cut -d: -f1)
CLAUDE_LINE=$(grep -n "Claude Code" "$DOCKERFILE" | head -1 | cut -d: -f1)
if [ -n "$GCC_LINE" ] && [ -n "$GH_LINE" ] && [ "$GH_LINE" -gt "$GCC_LINE" ]; then
    echo "  PASS: gh CLI installed after gcc removal (line $GH_LINE > $GCC_LINE)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: gh CLI should be installed after gcc removal"
    FAIL=$((FAIL + 1))
fi
if [ -n "$GCC_LINE" ] && [ -n "$CLAUDE_LINE" ] && [ "$CLAUDE_LINE" -gt "$GCC_LINE" ]; then
    echo "  PASS: Claude Code installed after gcc removal (line $CLAUDE_LINE > $GCC_LINE)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: Claude Code should be installed after gcc removal"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
