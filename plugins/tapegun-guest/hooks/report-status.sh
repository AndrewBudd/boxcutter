#!/bin/bash
# Report Claude Code agent status to tapegun metadata service AND messaging topic.
# Called by Stop, SubagentStop, StopFailure, and UserPromptSubmit hooks.
#
# Usage: report-status.sh <idle|working|error>

METADATA="http://169.254.169.254"
ORCHESTRATOR="${BOXCUTTER_ORCHESTRATOR:-http://192.168.50.2:8801}"
STATUS_FILE="/home/dev/.tapegun/status.json"
STATUS="${1:-idle}"
QUEUE_NAME="${BOXCUTTER_QUEUE:-$(hostname).inbox}"
TOPIC_NAME="${BOXCUTTER_STATUS_TOPIC:-status}"

# Read hook input from stdin
input=$(cat)

# Guard against infinite loops
stop_active=$(echo "$input" | jq -r '.stop_hook_active // false' 2>/dev/null)
if [ "$stop_active" = "true" ]; then
    exit 0
fi

# Extract context from hook payload
session_id=$(echo "$input" | jq -r '.session_id // empty' 2>/dev/null)
message=$(echo "$input" | jq -r '.last_assistant_message // empty' 2>/dev/null)

# Truncate message to last 500 chars
if [ ${#message} -gt 500 ]; then
    message="...${message: -500}"
fi

TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Write local status file
mkdir -p /home/dev/.tapegun
jq -n --arg status "$STATUS" \
       --arg msg "$message" \
       --arg ts "$TIMESTAMP" \
       --arg sid "$session_id" \
    '{status: $status, last_response: $msg, timestamp: $ts, session_id: $sid}' \
    > "$STATUS_FILE" 2>/dev/null

# Post to metadata service (best-effort, non-blocking)
curl -sf -X POST "$METADATA/tapegun/activity" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg status "$STATUS" \
                --arg msg "$message" \
                --arg ts "$TIMESTAMP" \
        '{status: $status, summary: $msg, timestamp: $ts}')" \
    --max-time 2 >/dev/null 2>&1 &

# Publish to messaging topic (best-effort, non-blocking)
curl -sf -X POST "${ORCHESTRATOR}/api/topics/${TOPIC_NAME}/publish" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg body "$STATUS" \
                --arg from "$(hostname)" \
        '{body: $body, from: $from, priority: "normal"}')" \
    --max-time 2 >/dev/null 2>&1 &

exit 0
