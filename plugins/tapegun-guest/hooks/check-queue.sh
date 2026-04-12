#!/bin/bash
# Check the messaging queue for new messages and notify Claude.
# Replaces check-inbox.sh with queue-based reads from the orchestrator.
# Runs as a PostToolUse/UserPromptSubmit hook in Claude Code.

QUEUE_NAME="${BOXCUTTER_QUEUE:-$(hostname).inbox}"
ORCHESTRATOR="${BOXCUTTER_ORCHESTRATOR:-http://192.168.50.2:8801}"
MARKER="/home/dev/.tapegun/.last-queue-check"

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

# Count and display messages
if command -v jq >/dev/null 2>&1; then
    count=$(echo "$response" | jq 'length' 2>/dev/null)
    [ "$count" = "0" ] && exit 0

    echo "--- INCOMING MESSAGES ($count) ---"
    echo "$response" | jq -r '.[] | "[\(.priority)] from \(.from_agent // "unknown"): \(.body)"' 2>/dev/null

    # Ack the messages
    ids=$(echo "$response" | jq -r '[.[].id] | join(",")' 2>/dev/null)
    if [ -n "$ids" ]; then
        curl -sf -X POST "${ORCHESTRATOR}/api/queues/${QUEUE_NAME}/ack" \
            -H "Content-Type: application/json" \
            -d "{\"message_ids\":[$(echo "$response" | jq -r '[.[].id | "\"" + . + "\""] | join(",")')]}" \
            --max-time 2 2>/dev/null || true
    fi
    echo "---"
else
    echo "--- INCOMING MESSAGES ---"
    echo "New messages in queue ${QUEUE_NAME} — check via: ssh boxcutter msg read ${QUEUE_NAME}"
    echo "---"
fi
