# Receiving Messages via Tapegun

## Context

You are running inside a Boxcutter Firecracker VM. A background daemon called `boxcutter-tapegun` runs as a systemd service and does two things every 5 seconds:

1. **Reports activity**: Captures the last 50 lines of the tmux pane and posts it to the metadata service. External agents use this to observe what you're doing.
2. **Fetches inbox**: Checks for new messages from external agents and writes them to your inbox file.

## Trigger

Use this skill when:
- You want to check for incoming messages or tasks
- A hook notification tells you there are new messages in your inbox
- You want to understand how the tapegun messaging system works

## Inbox Location

Messages are written to:

```
/home/dev/.tapegun/inbox.json
```

This is a JSON array of message objects:

```json
[
  {
    "id": "tg-1705312200000000000",
    "from": "host-agent",
    "body": "Please run the test suite and report results",
    "priority": "normal",
    "send_keys": false,
    "created_at": "2025-01-15T10:30:00Z"
  }
]
```

## Message Fields

- **`id`**: Unique message identifier.
- **`from`**: Who sent the message (usually "host-agent" or similar).
- **`body`**: The message content — typically a task description or instruction.
- **`priority`**: `"normal"` or `"urgent"`. Urgent messages are also injected into tmux.
- **`send_keys`**: If `true`, the message was also injected into tmux via send-keys.
- **`created_at`**: When the message was sent.

## How Messages Arrive

- The `boxcutter-tapegun` daemon polls the metadata service every 5 seconds.
- Messages with `send_keys: true` or `priority: "urgent"` are automatically typed into the tmux pane.
- All messages (including send-keys ones) are written to the inbox file.

## Checking for Messages

Read the inbox file to see pending messages:

```bash
cat /home/dev/.tapegun/inbox.json
```

If the file doesn't exist or contains `[]`, there are no messages.

## Acknowledging Messages

Messages are automatically cleared from the metadata service when fetched. The inbox file accumulates messages — new ones are merged in. If you want to acknowledge receipt explicitly:

```bash
curl -sf -X POST http://169.254.169.254/tapegun/inbox/ack \
  -H "Content-Type: application/json" \
  -d '{"message_ids": ["tg-1705312200000000000"]}'
```

## Responding to Messages

Since external agents monitor the tmux pane content via the tapegun activity API, the best way to "respond" is to do the work described in the message. The host-side agent will see your activity reflected in the pane content.

If you need to send an explicit response, you can write to a well-known file or output to the terminal — the host agent will see it in the pane content.
