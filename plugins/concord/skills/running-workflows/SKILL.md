---
name: running-workflows
description: >
  Executes Concord-style multi-step workflows on Boxcutter worker VMs. Creates
  an ephemeral VM, deploys a task agent via cp-to, runs sequential steps via exec,
  polls for completion, retrieves output via cp-from, and destroys the VM. All
  communication is mediated through the Boxcutter orchestrator — worker VMs never
  need outbound connectivity. Triggers on: running a workflow, executing a pipeline,
  CI/CD tasks, running steps on a remote VM, deploying and running code in isolation,
  or any mention of Concord workflows.
---

# Concord Workflows on Boxcutter

Run multi-step workflows on ephemeral Boxcutter VMs. The worker VM never connects back — all communication goes through boxcutter commands (exec, cp-to, cp-from).

## Architecture

```
You (host/orchestrator)
  │
  ├── ssh HOST new                       → Create worker VM
  ├── ssh HOST cp-to VM install.sh ...   → Deploy agent
  ├── ssh HOST cp-to VM task.json ...    → Send task payload
  ├── ssh HOST exec VM run-task.sh       → Execute task
  ├── ssh HOST exec VM cat status.json   → Poll status
  ├── ssh HOST cp-from VM output ...     → Retrieve results
  └── ssh HOST destroy VM                → Clean up
```

## Finding your Boxcutter host

Same as the boxcutter plugin — check CLAUDE.md, memory, or try `ssh boxcutter`.

All examples use `HOST` as a placeholder.

## Step-by-step workflow

### 1. Create a worker VM

```bash
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null HOST new
```

Parse the VM name from output (e.g., `bold-fox`).

### 2. Deploy the task agent

The agent is a lightweight shell script at `concord/agent/install.sh` in this repo.

```bash
# Copy the installer to the VM
ssh HOST cp-to <vm-name> concord/agent/install.sh /tmp/concord-install.sh

# Run it
ssh HOST exec <vm-name> "bash /tmp/concord-install.sh"
```

This creates `/opt/concord-agent/` with:
- `run-task.sh` — task executor
- `inbox/` — receives task payloads
- `outbox/` — output artifacts
- `logs/` — execution logs
- `workspace/` — working directory
- `status.json` — current status

### 3. Send a task payload

Create a task JSON file and copy it to the VM:

```json
{
  "id": "build-and-test",
  "steps": [
    {"name": "clone", "command": "git clone https://github.com/org/repo /opt/concord-agent/workspace/repo"},
    {"name": "build", "command": "cd /opt/concord-agent/workspace/repo && make build"},
    {"name": "test",  "command": "cd /opt/concord-agent/workspace/repo && make test"},
    {"name": "package", "command": "tar czf /opt/concord-agent/outbox/dist.tar.gz -C /opt/concord-agent/workspace/repo/dist ."}
  ]
}
```

```bash
# Write the JSON to a temp file, then copy
cat > /tmp/task.json << 'EOF'
{"id": "build-and-test", "steps": [...]}
EOF
ssh HOST cp-to <vm-name> /tmp/task.json /opt/concord-agent/inbox/task.json
```

### 4. Execute the task

```bash
ssh HOST exec <vm-name> "/opt/concord-agent/run-task.sh"
```

The task runner:
- Reads steps from `/opt/concord-agent/inbox/task.json`
- Executes each step sequentially
- Updates `/opt/concord-agent/status.json` after each step
- Logs to `/opt/concord-agent/logs/task.log`
- Stops on first failure

### 5. Poll for status

```bash
ssh HOST exec <vm-name> "cat /opt/concord-agent/status.json"
```

Status values:
- `ready` — agent installed, waiting for task
- `running` — executing steps (check `step` field for current)
- `completed` — all steps succeeded
- `failed` — a step failed (check `exit_code` and `detail`)
- `error` — agent error

### 6. Retrieve logs and output

```bash
# Get execution logs
ssh HOST exec <vm-name> "cat /opt/concord-agent/logs/task.log"

# Copy output artifacts
ssh HOST cp-from <vm-name> /opt/concord-agent/outbox/dist.tar.gz ./dist.tar.gz
```

### 7. Clean up

```bash
ssh HOST destroy <vm-name>
```

## Go dispatcher

For programmatic use, a Go dispatcher is available at `concord/dispatcher/`. It wraps the full lifecycle:

```bash
cd concord/dispatcher
go build -o concord-dispatcher ./cmd/concord-dispatcher/
./concord-dispatcher -task task.json -orchestrator http://192.168.50.2:8801
```

The dispatcher creates a VM, deploys the agent, submits the task, polls to completion, prints logs, and destroys the VM.

## Monitoring with tapegun

While a task is running, you can monitor the worker VM with tapegun:

```bash
ssh HOST tapegun activity <vm-name>
```

This shows the tmux pane content, which includes the task output.

## Tips

- **Cross-step persistence**: Steps share the same filesystem. Output from step 1 is available to step 2 via the `workspace/` directory.
- **Custom working directory**: Each step can specify a `workdir` field. Defaults to `/opt/concord-agent/workspace`.
- **Output artifacts**: Write files to `/opt/concord-agent/outbox/` during steps, then retrieve with cp-from.
- **No networking required**: Worker VMs don't need to reach any external service. All communication is mediated by boxcutter.
- **Idempotent cleanup**: Always destroy the VM when done, even if the task failed.
