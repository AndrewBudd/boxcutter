# Monitoring VMs via Tapegun

## Trigger

Use this skill when the user wants to:
- Check what VMs are doing
- Monitor agent activity across VMs
- See tmux pane content from a VM
- Check if a VM is idle or active
- View pending messages for a VM

## Connection

Tapegun commands are accessed through the Boxcutter SSH interface. You need SSH access to the orchestrator.

1. Check if `BOXCUTTER_SSH` is set in the environment (e.g., `ssh svc:boxcutter`)
2. Try `ssh boxcutter` (Tailscale MagicDNS)
3. Ask the user for the SSH command

## Commands

### Get all VM activity

```bash
ssh <host> tapegun activity
```

Shows activity for every VM: node assignment, status, pending messages, and the last 10 lines of tmux pane content.

### Get single VM activity

```bash
ssh <host> tapegun activity <vm-name>
```

Shows detailed activity for one VM, including the full tmux pane capture.

## Response Fields

- **pane content**: The last 50 lines of the VM's tmux pane — shows what the agent is currently seeing/doing.
- **Activity status**: `active` if the pane has content, `idle` if empty.
- **Pending**: Number of unread messages in the VM's inbox.
- **Status**: The VM's lifecycle status (`running`, `stopped`, etc.).

## Usage Patterns

- **Quick check**: `ssh <host> tapegun activity` to see all VMs at a glance.
- **Watch a specific VM**: Run `ssh <host> tapegun activity <name>` to observe what a VM agent is doing.
- **Detect idle VMs**: Look for VMs with `Activity: idle` and `Pending: 0` — they may be ready for new work.
- **No activity data**: If activity shows "no data", the VM may still be booting or the tapegun daemon hasn't started yet.
