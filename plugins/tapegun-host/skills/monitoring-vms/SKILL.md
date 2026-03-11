# Monitoring VMs via Tapegun

## Trigger

Use this skill when the user wants to:
- Check what VMs are doing
- Monitor agent activity across VMs
- See tmux pane content from a VM
- Check if a VM is idle or active
- View pending messages for a VM

## Orchestrator Discovery

To call the tapegun API, you need the orchestrator's address:
1. Check if `ORCHESTRATOR_HOST` or `BOXCUTTER_HOST` is set in the environment
2. Try `boxcutter:8801` (the Tailscale hostname)
3. Try `192.168.50.2:8801` (the default bridge IP)
4. Ask the user for the orchestrator address

## API Reference

### Get all VM activity

```
GET http://<orchestrator>:8801/api/tapegun/activity
```

Returns a JSON array of activity entries for every VM in the cluster:

```json
[
  {
    "name": "my-vm",
    "node_id": "node-1",
    "node_name": "boxcutter-node-1",
    "vm_status": "running",
    "activity": {
      "timestamp": "2025-01-15T10:30:00Z",
      "pane_content": "$ go test ./...\nok  pkg/foo  0.5s\n$ ",
      "status": "active",
      "summary": ""
    },
    "pending_messages": 0
  }
]
```

### Get single VM activity

```
GET http://<orchestrator>:8801/api/tapegun/activity/{name}
```

Returns a single activity entry for the named VM.

## Response Fields

- **`pane_content`**: The last 50 lines of the VM's tmux pane. This shows what the agent is currently seeing/doing.
- **`status`**: `"active"` if the pane has content, `"idle"` if empty.
- **`pending_messages`**: Number of unread messages in the VM's inbox.
- **`vm_status`**: The VM's lifecycle status (`"running"`, `"stopped"`, etc.).
- **`timestamp`**: When this activity was last reported. Stale timestamps (>10s old) may indicate the tapegun daemon isn't running yet.

## Usage Patterns

- **Quick check**: `curl -s http://<orchestrator>:8801/api/tapegun/activity | jq .` to see all VMs at a glance.
- **Watch a specific VM**: Poll `GET /api/tapegun/activity/{name}` to observe what a VM agent is doing.
- **Detect idle VMs**: Look for VMs with `status: "idle"` and no `pending_messages` — they may be ready for new work.
- **No activity data**: If `activity` is null, the VM may still be booting or the tapegun daemon hasn't started yet.
