# Sending Messages to VMs via Tapegun

## Trigger

Use this skill when the user wants to:
- Send a task or instruction to a VM
- Communicate with a Claude Code agent running inside a VM
- Broadcast a message to all VMs
- Inject a command into a VM's tmux session

## Connection

Same as the monitoring skill — see `monitoring-vms/SKILL.md` for discovery steps.

## Commands

### Send a message to a specific VM

```bash
ssh <host> tapegun send <vm-name> <message text>
```

The message is written to `/home/dev/.tapegun/inbox.json` inside the VM. The guest-side Claude agent reads this file to discover new tasks.

### Inject a command into a VM's tmux pane

```bash
ssh <host> tapegun sendkeys <vm-name> <command>
```

The command is typed directly into the VM's active tmux pane via `tmux send-keys`. Use this to run commands in the VM's terminal. The message is also written to the inbox.

### Broadcast to all running VMs

```bash
ssh <host> tapegun broadcast <message text>
```

Sends the message to every running VM's inbox. Use for fleet-wide directives.

## Message Delivery Semantics

- **Normal messages** (`tapegun send`): Written to `/home/dev/.tapegun/inbox.json` inside the VM. The agent reads this file to discover new tasks.
- **Send-keys messages** (`tapegun sendkeys`): Both written to inbox AND injected into tmux. Use this to run a command in the VM's terminal.

## Usage Patterns

1. **Assign work**: Send a normal message describing the task. The guest-side Claude will read it from inbox.
2. **Run a command**: Use `sendkeys` with the command (e.g., `ssh <host> tapegun sendkeys my-vm git pull && make test`).
3. **Monitor + message loop**: Check activity first, then send targeted messages based on what VMs are doing, then poll activity again to confirm the message was received.
4. **Fleet-wide directive**: Use broadcast to instruct all active VMs (e.g., `ssh <host> tapegun broadcast please commit your current work`).
