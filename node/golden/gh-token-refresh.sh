#!/bin/bash
# Refresh GitHub CLI auth token from the vmid metadata service.
# Runs as a systemd timer inside each VM.
# The metadata service identifies the VM by fwmark and returns
# a scoped GitHub installation token.

set -euo pipefail

METADATA="http://169.254.169.254"

resp=$(curl -sf "$METADATA/token/github" 2>/dev/null) || {
    echo "vmid: GitHub token not available (service may not be configured)" >&2
    exit 0
}

token=$(echo "$resp" | jq -r '.token // empty')
if [ -z "$token" ]; then
    echo "vmid: no token in response" >&2
    exit 1
fi

# Authenticate the gh CLI so `gh auth status` works and all gh
# commands (pr create, issue list, etc.) pick up the token.
echo "$token" | gh auth login --with-token

# Update git credential helper so HTTPS git operations use the fresh token
git config --global credential.helper '!f() { echo "username=x-access-token"; echo "password='"$token"'"; }; f'

echo "vmid: GitHub token refreshed (expires: $(echo "$resp" | jq -r '.expires_at // "unknown"'))"
