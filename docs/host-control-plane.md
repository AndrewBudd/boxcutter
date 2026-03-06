# Host Control Plane

A service that runs on the physical host (not inside a VM). Manages the lifecycle of QEMU VMs.

## Responsibilities

### 1. Boot Recovery
- Starts on system boot (systemd service)
- Launches the orchestrator VM first
- Launches node VMs that were previously running
- Nodes self-register with orchestrator when they come up

### 2. Capacity Management
- Polls running nodes for resource utilization (RAM, CPU, VM count)
- When capacity across existing nodes is running low, checks if the physical host can support another node VM
- If so, provisions and launches a new node VM
- New node registers with orchestrator automatically on startup

### 3. Node Lifecycle
- Drain nodes (migrate VMs off, then shut down)
- Retire nodes (remove from rotation)
- No node creation commands flow through the orchestrator — the control plane owns this

### 4. Deployments
- Rolling updates: new golden images, new node agent binaries
- Spin up new node with updated software, drain old node, retire it
- Version management for the node VM image itself

## Key Design Points
- The orchestrator does NOT command nodes into existence
- Nodes are ephemeral — the control plane creates/destroys them
- The orchestrator only knows about nodes that have registered with it
- The control plane is the only thing that touches QEMU on the host
