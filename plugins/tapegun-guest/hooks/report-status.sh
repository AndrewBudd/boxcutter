#!/bin/bash
# Report Claude Code status to tapegun after each response.
# Runs as a Stop hook — fires every time Claude finishes a turn.
# Posts the last assistant message to the metadata service for
# host-side monitoring via tapegun activity.

METADATA="http://169.254.169.254"
STATUS_FILE="/home/dev/.tapegun/status.json"

# Read hook input from stdin
input=$(cat)

# Extract last_assistant_message
message=$(echo "$input" | jq -r '.last_assistant_message // empty' 2>/dev/null)
[ -z "$message" ] && exit 0

# Truncate to last 500 chars to keep payload small
if [ ${#message} -gt 500 ]; then
    message="...${message: -500}"
fi

# Write local status file
mkdir -p /home/dev/.tapegun
jq -n --arg msg "$message" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{last_response: $msg, timestamp: $ts}' > "$STATUS_FILE" 2>/dev/null

# Post to metadata service (best-effort, don't block Claude)
curl -sf -X POST "$METADATA/tapegun/status" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg msg "$message" '{status: $msg}')" \
    --max-time 2 >/dev/null 2>&1 &

exit 0
