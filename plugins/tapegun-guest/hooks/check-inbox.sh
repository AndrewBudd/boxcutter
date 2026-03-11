#!/bin/bash
# Check the tapegun inbox for new messages and notify Claude if any are present.
# Runs as a PostToolUse hook after Bash commands.

INBOX="/home/dev/.tapegun/inbox.json"
MARKER="/home/dev/.tapegun/.last-notified"

# Exit silently if no inbox file
[ -f "$INBOX" ] || exit 0

# Exit silently if inbox is empty or just "[]"
content=$(cat "$INBOX" 2>/dev/null)
[ -z "$content" ] && exit 0
[ "$content" = "[]" ] && exit 0
[ "$content" = "null" ] && exit 0

# Check if inbox has changed since last notification
if [ -f "$MARKER" ]; then
    inbox_mtime=$(stat -c %Y "$INBOX" 2>/dev/null || echo 0)
    marker_mtime=$(stat -c %Y "$MARKER" 2>/dev/null || echo 0)
    [ "$inbox_mtime" -le "$marker_mtime" ] && exit 0
fi

# Count messages
if command -v jq >/dev/null 2>&1; then
    count=$(echo "$content" | jq 'length' 2>/dev/null)
    [ "$count" = "0" ] && exit 0

    echo "--- TAPEGUN INBOX ---"
    echo "You have $count new message(s) in /home/dev/.tapegun/inbox.json"
    echo ""
    echo "$content" | jq -r '.[] | "[\(.priority)] from \(.from // "unknown"): \(.body)"' 2>/dev/null
    echo "---"
else
    echo "--- TAPEGUN INBOX ---"
    echo "New messages in /home/dev/.tapegun/inbox.json — read the file for details."
    echo "---"
fi

# Update marker
touch "$MARKER"
