# Orchestrator Architecture

The orchestrator is the distributed state manager and coordinator for the Boxcutter cluster. It tracks which VMs exist on which nodes, handles user requests via SSH, manages SSH keys, and coordinates golden image distribution. The fact that it runs inside a QEMU VM is incidental to its role.

## Binaries

| Binary | Entry Point | Role |
|--------|-------------|------|
| `boxcutter-orchestrator` | `cmd/orchestrator/main.go` | HTTP API server on `:8801` |
| `boxcutter-ssh-orchestrator` | `cmd/ssh/main.go` | SSH ForceCommand binary (invoked fresh per connection) |

## SSH Control Interface

Users interact with Boxcutter through SSH to the orchestrator:

```bash
ssh boxcutter new [options]        # Create a new VM
  --clone <repo>                   #   Clone repo on creation
  --vcpu <N>                       #   CPU cores (default: 2)
  --ram <MiB>                      #   RAM in MiB (default: 2048)
  --disk <size>                    #   Disk size (default: 50G)
  --mode normal|paranoid           #   Network mode (default: normal)
  --node <node-id>                 #   Pin to specific node
ssh boxcutter list                 # List all VMs
ssh boxcutter destroy <name>       # Destroy a VM
ssh boxcutter stop <name>          # Stop a running VM
ssh boxcutter start <name>         # Start a stopped VM
ssh boxcutter cp <name> [new-name] # Clone a VM's disk
ssh boxcutter status               # Cluster capacity summary
ssh boxcutter nodes                # List all nodes with health
ssh boxcutter images               # List golden images on nodes
ssh boxcutter adduser <github>     # Add SSH keys from GitHub
ssh boxcutter removeuser <github>  # Remove SSH keys
ssh boxcutter keys                 # List configured SSH keys
```

The SSH binary is invoked via OpenSSH `ForceCommand` — each SSH connection gets a fresh process. No long-running daemon.

## HTTP API

`:8801` — used by nodes and the SSH binary.

**VM management:**

| Endpoint | Description |
|----------|-------------|
| `POST /api/vms` | Create + schedule a VM to a node |
| `GET /api/vms` | List all VMs across all nodes |
| `GET /api/vms/{name}` | VM details |
| `DELETE /api/vms/{name}` | Destroy a VM |
| `POST /api/vms/{name}/stop` | Stop a VM |
| `POST /api/vms/{name}/start` | Start a stopped VM |
| `POST /api/vms/{name}/copy` | Clone a VM's disk |
| `POST /api/vms/{name}/repos` | Add repos to a VM |
| `GET /api/vms/{name}/repos` | List repos on a VM |
| `DELETE /api/vms/{name}/repos/{repo...}` | Remove a repo from a VM |

**Node management:**

| Endpoint | Description |
|----------|-------------|
| `POST /api/nodes/register` | Node self-registration on boot |
| `POST /api/nodes/{id}/heartbeat` | Node heartbeat |
| `GET /api/nodes` | List all nodes |
| `GET /api/nodes/{id}` | Node details |

**Golden image:**

| Endpoint | Description |
|----------|-------------|
| `GET /api/golden` | List golden image versions across nodes |
| `GET /api/golden/head` | Current head version |
| `POST /api/golden/head` | Set golden head (publishes to MQTT) |

**SSH keys:**

| Endpoint | Description |
|----------|-------------|
| `POST /api/keys/add` | Add SSH keys by GitHub username |
| `GET /api/keys` | List all configured keys |
| `DELETE /api/keys/{user}` | Remove keys for a user |

**Operations:**

| Endpoint | Description |
|----------|-------------|
| `POST /api/migrate` | Self-migration (orchestrator moving to new node) |
| `POST /api/prepare-migrate` | Prepare for orchestrator migration |
| `POST /api/shutdown` | Graceful shutdown |
| `GET /api/health` | Health check |
| `GET /healthz` | Health check (alias) |

## Scheduling

The scheduler picks a node by free RAM. On VM creation:

1. Query all healthy nodes for capacity
2. Pick node with most free RAM
3. Call node agent's `POST /api/vms` to create the VM
4. Store the VM record in SQLite

## State

SQLite at `/var/lib/boxcutter/orchestrator.db` with WAL mode. Uses `modernc.org/sqlite` (pure Go, no CGO).

```sql
CREATE TABLE nodes (
  id TEXT PRIMARY KEY,
  tailscale_name TEXT NOT NULL, tailscale_ip TEXT, bridge_ip TEXT,
  api_addr TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',  -- active, down, draining, retired, provisioning
  registered_at TEXT NOT NULL, last_heartbeat TEXT
);

CREATE TABLE vms (
  name TEXT PRIMARY KEY,
  node_id TEXT NOT NULL REFERENCES nodes(id),
  status TEXT DEFAULT 'running'
);

CREATE TABLE golden_images (
  version TEXT NOT NULL, node_id TEXT NOT NULL REFERENCES nodes(id),
  discovered_at TEXT NOT NULL, PRIMARY KEY (version, node_id)
);

CREATE TABLE golden_config (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE ssh_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT, github_user TEXT NOT NULL,
  public_key TEXT NOT NULL UNIQUE, added_at TEXT NOT NULL
);
```

**Thin state design:** The `vms` table stores only `(name, node_id, status)` — not detailed state. All other info (RAM, vCPU, Tailscale IP, repos) is fetched from nodes on demand. This avoids state divergence: if a Firecracker VM crashes, the orchestrator learns at the next health sync (30s) rather than serving stale data.

## Health Monitoring

Background loop (30s interval):

1. Query all registered nodes via HTTP (2s timeout)
2. Sync VM inventory from node responses
3. Sync golden image versions from nodes
4. Mark unresponsive nodes

## MQTT

Publishes golden image head version to `boxcutter/golden/head` (retained, QoS 1) via Mosquitto broker on the host. Nodes subscribe and pull new versions from OCI.

## Design Principle

Thin orchestrator, smart nodes. The orchestrator tells nodes *what* to do; nodes decide *how*. The orchestrator doesn't know about Firecracker internals, TAP devices, fwmarks, or proxy configuration — those are entirely the node's concern.
