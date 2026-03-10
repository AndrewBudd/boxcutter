# Orchestrator

Distributed state manager and coordinator. Tracks which VMs exist on which nodes, handles user requests via SSH, manages SSH keys, and coordinates golden image distribution. The fact that it runs inside a QEMU VM is incidental to its role.

## Binaries

| Binary | Entry Point | Role |
|--------|-------------|------|
| `boxcutter-orchestrator` | `cmd/orchestrator/main.go` | HTTP API server on `:8801` |
| `boxcutter-ssh-orchestrator` | `cmd/ssh/main.go` | SSH ForceCommand binary (invoked fresh per connection, not long-running) |

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/api/` | HTTP handlers — the bulk of orchestrator logic |
| `internal/config/` | YAML config loading (`/etc/boxcutter/boxcutter.yaml`) |
| `internal/db/` | SQLite with WAL mode — tables: nodes, vms, golden_images, golden_config, ssh_keys |
| `internal/mqtt/` | MQTT client — publishes golden image head versions |
| `internal/node/` | HTTP client to node agents — `Client` (5min timeout, mutations) and `FastClient` (2s timeout, queries) |
| `internal/scheduler/` | Node selection — picks node with most free RAM |
| `internal/ssh/` | SSH command parsing and dispatch |

## API Surface

Key HTTP endpoints on `:8801`:

**VMs**: create, destroy, start, stop, list, get, migrate
**Nodes**: register, heartbeat, list, get
**Golden images**: list versions, set head, get head
**SSH keys**: add (by GitHub username), list, remove
**Health**: `GET /healthz`

## Health Monitoring

Background loop (30s interval):
1. Query all registered nodes via `FastClient`
2. Sync VM inventory from node responses
3. Sync golden image versions from nodes
4. Mark unresponsive nodes

## Build

```bash
cd orchestrator && go build -o boxcutter-orchestrator ./cmd/orchestrator/
cd orchestrator && go build -o boxcutter-ssh-orchestrator ./cmd/ssh/
```

## Deploy (to running orchestrator VM)

```bash
scp boxcutter-orchestrator ubuntu@192.168.50.2:/tmp/
ssh ubuntu@192.168.50.2 "sudo mv /tmp/boxcutter-orchestrator /usr/local/bin/ && sudo systemctl restart boxcutter-orchestrator"
```

SSH binary doesn't need a restart — it's invoked fresh per connection.

## Detailed Documentation

- `docs/architecture.md` — SSH interface, HTTP API surface, scheduling, state, MQTT, health monitoring
- `docs/development.md` — Building, deploying, debugging

## Domain Boundary

This component **owns**:
- VM name → node assignment (scheduling)
- Node registry and health tracking
- SSH key distribution
- Golden image head version (which version nodes should run)
- User-facing SSH interface
- Cluster-wide VM inventory

This component **does not own**:
- Firecracker VM lifecycle (delegates to node agents via HTTP)
- Networking inside nodes (TAP, fwmark — that's node domain)
- QEMU VM management (that's host domain)
- Golden image building (that's node domain)
- MQTT broker (that's host domain; orchestrator is a client)

**Design principle**: thin orchestrator, smart nodes. The orchestrator tells nodes *what* to do; nodes decide *how*.
