# Sending Messages to VMs via Tapegun

## Trigger

Use this skill when the user wants to:
- Send a task or instruction to a VM
- Communicate with a Claude Code agent running inside a VM
- Broadcast a message to all VMs
- Inject a command into a VM's tmux session

## Orchestrator Discovery

Same as the monitoring skill — see `monitoring-vms/SKILL.md` for discovery steps.

## API Reference

### Send a message to a specific VM

```
POST http://<orchestrator>:8801/api/tapegun/message/{name}
Content-Type: application/json

{
  "body": "Please run the test suite and report results",
  "from": "host-agent",
  "priority": "normal",
  "send_keys": false
}
```

Response (201 Created):
```json
{
  "status": "sent",
  "message_id": "tg-1705312200000000000"
}
```

### Broadcast to all VMs

```
POST http://<orchestrator>:8801/api/tapegun/broadcast
Content-Type: application/json

{
  "body": "Please commit your current work",
  "from": "host-agent",
  "priority": "normal",
  "send_keys": false,
  "filter": "running"
}
```

Response:
```json
{
  "sent": ["vm-1", "vm-2"],
  "failed": []
}
```

## Request Fields

- **`body`** (required): The message content. For `send_keys` messages, this is injected directly into tmux.
- **`from`** (optional): Identifier for the sender. Shows up in the VM's inbox.
- **`priority`**: `"normal"` (default) or `"urgent"`. Urgent messages are automatically injected into tmux via send-keys.
- **`send_keys`**: If `true`, the message body is injected into the VM's active tmux pane via `tmux send-keys`. Use this to run commands directly.
- **`filter`** (broadcast only): Filter VMs by status. Common values: `"running"`, `"stopped"`.

## Message Delivery Semantics

- **Normal messages** (`send_keys: false`, `priority: "normal"`): Written to `/home/dev/.tapegun/inbox.json` inside the VM. The agent reads this file to discover new tasks.
- **Send-keys messages** (`send_keys: true`): Both written to inbox AND injected into tmux. Use this to run a command in the VM's terminal.
- **Urgent messages** (`priority: "urgent"`): Same as send-keys — injected into tmux regardless of the `send_keys` field.

## Usage Patterns

1. **Assign work**: Send a normal-priority message describing the task. The guest-side Claude will read it from inbox.
2. **Run a command**: Send with `send_keys: true` and the command as the body (e.g., `"git pull && make test"`).
3. **Monitor + message loop**: Check activity first, then send targeted messages based on what VMs are doing, then poll activity again to confirm the message was received.
4. **Fleet-wide directive**: Use broadcast with `filter: "running"` to instruct all active VMs.
