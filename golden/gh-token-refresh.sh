#!/bin/bash
# Refresh GitHub CLI auth token from the vmid metadata service.
# Runs as a systemd timer inside each VM.
# The metadata service at 10.0.0.1 identifies the VM by fwmark
# and returns a scoped GitHub installation token.

set -euo pipefail

METADATA="http://10.0.0.1"

resp=$(curl -sf "$METADATA/token/github" 2>/dev/null) || {
    echo "vmid: GitHub token not available (service may not be configured)" >&2
    exit 0
}

token=$(echo "$resp" | jq -r '.token // empty')
if [ -z "$token" ]; then
    echo "vmid: no token in response" >&2
    exit 1
fi

# Write the token so gh CLI picks it up
export GH_TOKEN="$token"

# Also persist for non-interactive use
mkdir -p "$HOME/.config/gh"
cat > "$HOME/.config/gh/hosts.yml" <<EOF
github.com:
    oauth_token: $token
    user: x-access-token
    git_protocol: https
EOF

echo "vmid: GitHub token refreshed (expires: $(echo "$resp" | jq -r '.expires_at // "unknown"'))"
