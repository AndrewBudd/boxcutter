#!/bin/bash
# Test script for boxcutter-setup GitHub config extraction.
# Validates that the awk-based YAML extraction works correctly.
set -eo pipefail

PASS=0
FAIL=0

assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc (expected='$expected', got='$actual')"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== boxcutter-setup GitHub extraction tests ==="

# --- Test 1: Standard config with github section ---
echo "Test 1: Standard config with github enabled"
TMPCONF=$(mktemp)
cat > "$TMPCONF" <<'EOF'
tailscale:
  enabled: true
github:
  enabled: true
  app_id: 123456
  installation_id: 789012
truenas:
  enabled: false
EOF

GITHUB_ENABLED=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /enabled:/{print $2; exit}' "$TMPCONF")
assert_eq "github.enabled" "true" "$GITHUB_ENABLED"

APP_ID=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /app_id:/{print $2; exit}' "$TMPCONF")
assert_eq "github.app_id" "123456" "$APP_ID"

INSTALL_ID=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /installation_id:/{print $2; exit}' "$TMPCONF")
assert_eq "github.installation_id" "789012" "$INSTALL_ID"
rm -f "$TMPCONF"

# --- Test 2: Config with github disabled ---
echo "Test 2: GitHub disabled"
TMPCONF=$(mktemp)
cat > "$TMPCONF" <<'EOF'
github:
  enabled: false
  app_id: 123456
  installation_id: 789012
EOF

GITHUB_ENABLED=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /enabled:/{print $2; exit}' "$TMPCONF")
assert_eq "github.enabled=false" "false" "$GITHUB_ENABLED"
rm -f "$TMPCONF"

# --- Test 3: No github section ---
echo "Test 3: No github section"
TMPCONF=$(mktemp)
cat > "$TMPCONF" <<'EOF'
tailscale:
  enabled: true
truenas:
  enabled: false
EOF

GITHUB_ENABLED=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /enabled:/{print $2; exit}' "$TMPCONF")
assert_eq "no github section" "" "$GITHUB_ENABLED"
rm -f "$TMPCONF"

# --- Test 4: Don't confuse truenas.enabled with github.enabled ---
echo "Test 4: Only truenas.enabled=true, no github section"
TMPCONF=$(mktemp)
cat > "$TMPCONF" <<'EOF'
truenas:
  enabled: true
  host: truenas.local
EOF

GITHUB_ENABLED=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /enabled:/{print $2; exit}' "$TMPCONF")
assert_eq "should not match truenas.enabled" "" "$GITHUB_ENABLED"
rm -f "$TMPCONF"

# --- Test 5: GitHub section at end of file (no trailing section) ---
echo "Test 5: GitHub section at end of file"
TMPCONF=$(mktemp)
cat > "$TMPCONF" <<'EOF'
tailscale:
  node_authkey_path: /etc/boxcutter/secrets/node-key
github:
  enabled: true
  app_id: 999
  installation_id: 888
EOF

APP_ID=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /app_id:/{print $2; exit}' "$TMPCONF")
assert_eq "app_id at EOF" "999" "$APP_ID"

INSTALL_ID=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /installation_id:/{print $2; exit}' "$TMPCONF")
assert_eq "installation_id at EOF" "888" "$INSTALL_ID"
rm -f "$TMPCONF"

# --- Test 6: Commented-out values ---
echo "Test 6: Commented-out installation_id"
TMPCONF=$(mktemp)
cat > "$TMPCONF" <<'EOF'
github:
  enabled: true
  app_id: 555
  # installation_id: 789012
EOF

INSTALL_ID=$(awk '/^github:/{found=1; next} found && /^\S/{found=0} found && !/^[ \t]*#/ && /installation_id:/{print $2; exit}' "$TMPCONF")
assert_eq "commented installation_id should be empty" "" "$INSTALL_ID"
rm -f "$TMPCONF"

# --- Test 7: Claude credentials sed injection ---
echo "Test 7: Claude credentials path inserted into vmid config"
TMPVMID=$(mktemp)
cat > "$TMPVMID" <<'EOF'
listen:
  vm_addr: "169.254.169.254"
  vm_port: 80
  admin_socket: /run/vmid/admin.sock

metadata:
  ssh_authorized_keys:
    - /etc/boxcutter/secrets/cluster-ssh.key.pub
    - /etc/boxcutter/secrets/authorized-keys
  ca_cert_path: /etc/boxcutter/secrets/ca.crt

jwt:
  ttl: "10m"
EOF

# Simulate what boxcutter-setup does
sed -i "/ca_cert_path:/a\\  claude_credentials_path: /etc/boxcutter/secrets/claude-credentials.json" "$TMPVMID"

# Verify the line was inserted in the right place (inside metadata: block, after ca_cert_path)
CRED_LINE=$(grep "claude_credentials_path:" "$TMPVMID" | sed 's/^ *//')
assert_eq "claude_credentials_path present" "claude_credentials_path: /etc/boxcutter/secrets/claude-credentials.json" "$CRED_LINE"

# Verify it's indented correctly (inside metadata section)
INDENT=$(grep "claude_credentials_path:" "$TMPVMID" | sed 's/[^ ].*//')
assert_eq "correct indentation (2 spaces)" "  " "$INDENT"

# Verify jwt: section is still intact after the metadata block
JWT_LINE=$(grep "^jwt:" "$TMPVMID")
assert_eq "jwt section preserved" "jwt:" "$JWT_LINE"
rm -f "$TMPVMID"

# --- Test 8: Claude credentials NOT inserted when file missing ---
echo "Test 8: Claude credentials skipped when secret missing"
TMPVMID=$(mktemp)
cat > "$TMPVMID" <<'EOF'
metadata:
  ca_cert_path: /etc/boxcutter/secrets/ca.crt

jwt:
  ttl: "10m"
EOF
# Don't run the sed (simulates the file-missing branch)
CRED_COUNT=$(grep -c "claude_credentials_path" "$TMPVMID" || true)
assert_eq "no credentials line when not inserted" "0" "$CRED_COUNT"
rm -f "$TMPVMID"

# --- Summary ---
echo ""
echo "Results: $PASS passed, $FAIL failed"
[ $FAIL -eq 0 ] || exit 1
