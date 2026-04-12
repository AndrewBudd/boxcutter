#!/bin/bash
# Check the messaging queue for new messages and forward to the active agent.
# For OpenCode: POST to the server API to inject messages into the session.
# For Claude Code: print to stdout so the hook output reaches Claude.
# Runs as a PostToolUse/UserPromptSubmit hook in Claude Code.

QUEUE_NAME="${BOXCUTTER_QUEUE:-$(hostname).inbox}"
ORCHESTRATOR="${BOXCUTTER_ORCHESTRATOR:-http://192.168.50.2:8801}"
MARKER="/home/dev/.tapegun/.last-queue-check"
OPENCODE_PORT="${OPENCODE_SERVER_PORT:-4096}"
OPENCODE_PASS="${OPENCODE_SERVER_PASSWORD:-tapegun}"

# Rate limit: don't check more than once every 10 seconds
if [ -f "$MARKER" ]; then
    marker_age=$(( $(date +%s) - $(stat -c %Y "$MARKER" 2>/dev/null || echo 0) ))
    [ "$marker_age" -lt 10 ] && exit 0
fi
touch "$MARKER"

# Read from queue
response=$(curl -sf "${ORCHESTRATOR}/api/queues/${QUEUE_NAME}/messages" --max-time 3 2>/dev/null)
[ -z "$response" ] && exit 0
[ "$response" = "[]" ] && exit 0
[ "$response" = "null" ] && exit 0

# Count messages
if ! command -v jq >/dev/null 2>&1; then
    echo "--- INCOMING MESSAGES ---"
    echo "New messages in queue ${QUEUE_NAME} — install jq for full display"
    echo "---"
    exit 0
fi

count=$(echo "$response" | jq 'length' 2>/dev/null)
[ "$count" = "0" ] && exit 0

# Ack messages immediately so they don't re-deliver
ids=$(echo "$response" | jq -r '[.[].id] | join(",")' 2>/dev/null)
if [ -n "$ids" ]; then
    curl -sf -X POST "${ORCHESTRATOR}/api/queues/${QUEUE_NAME}/ack" \
        -H "Content-Type: application/json" \
        -d "{\"message_ids\":[$(echo "$response" | jq -r '[.[].id | "\"" + . + "\""] | join(",")')]}" \
        --max-time 2 2>/dev/null || true
fi

# Combine all messages into one text block
combined=$(echo "$response" | jq -r '.[] | "[from \(.from_agent // "unknown")] \(.body)"' 2>/dev/null)
[ -z "$combined" ] && exit 0

# Try to deliver via OpenCode API if the server is running
if curl -sf -u "opencode:${OPENCODE_PASS}" "http://localhost:${OPENCODE_PORT}/session" --max-time 1 >/dev/null 2>&1; then
    # Get or create a session
    session_id=$(curl -sf -u "opencode:${OPENCODE_PASS}" "http://localhost:${OPENCODE_PORT}/session" --max-time 2 2>/dev/null | jq -r '.[0].id // empty')
    if [ -z "$session_id" ]; then
        session_id=$(curl -sf -u "opencode:${OPENCODE_PASS}" -X POST "http://localhost:${OPENCODE_PORT}/session" \
            -H "Content-Type: application/json" -d '{}' --max-time 3 2>/dev/null | jq -r '.id // empty')
    fi

    if [ -n "$session_id" ]; then
        # POST message to OpenCode session
        payload=$(jq -n --arg text "$combined" '{parts: [{type: "text", text: $text}]}')
        curl -sf -u "opencode:${OPENCODE_PASS}" \
            -X POST "http://localhost:${OPENCODE_PORT}/session/${session_id}/message" \
            -H "Content-Type: application/json" \
            -d "$payload" \
            --max-time 60 >/dev/null 2>&1 &
        exit 0
    fi
fi

# Fallback: print to stdout (works for Claude Code hooks)
echo "--- INCOMING MESSAGES ($count) ---"
echo "$response" | jq -r '.[] | "[\(.priority)] from \(.from_agent // "unknown"): \(.body)"' 2>/dev/null
echo "---"
