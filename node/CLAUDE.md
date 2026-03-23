# Node

The fundamental system that manages Firecracker and QEMU VMs as a resource. Turns bare compute into isolated, networked, identity-aware dev environments. Contains multiple sub-components that work together.

## Sub-Components

### Agent (`agent/`) — Go module

The node agent manages Firecracker VM lifecycle. Runs as `boxcutter-node` on `:8800`.

| Package | Responsibility |
|---------|---------------|
| `internal/api/` | HTTP handlers — VM CRUD, migration, golden image, health |
| `internal/vm/` | VM process management via VMBackend interface — create, destroy, start, stop, snapshot, restore, migration (FC + QEMU) |
| `internal/golden/` | Golden image versioning — tracks available versions, active head |
| `internal/network/` | TAP device and fwmark setup per VM |
| `internal/mqtt/` | MQTT client — subscribes to golden image updates from orchestrator |
| `internal/vmid/` | HTTP client to the vmid service |
| `internal/config/` | Agent configuration |

Entry point: `agent/cmd/node/main.go`

### VM Identity (`vmid/`) — Go module

Maps Firecracker VMs to identities using Linux fwmark-based routing. Serves the metadata API at `169.254.169.254:80` and an admin Unix socket at `/run/vmid/admin.sock`.

| Package | Responsibility |
|---------|---------------|
| `internal/api/` | HTTP handlers — metadata API (public) + admin API (Unix socket) |
| `internal/config/` | Configuration |
| `internal/middleware/` | fwmark extraction from socket options |
| `internal/registry/` | Mark ↔ VM mapping, mark allocation |
| `internal/sentinel/` | Sentinel token store — maps sentinel tokens to real credentials |
| `internal/token/` | JWT generation and GitHub token exchange |

Entry point: `vmid/cmd/vmid/main.go`

**This is the only module with unit tests**: `cd node/vmid && go test ./...`

### MITM Proxy (`proxy/`) — Go module

Forward proxy on `:8080` that swaps sentinel tokens for real credentials. Used in "paranoid mode" where VMs never see real API keys.

Entry point: `proxy/cmd/proxy/main.go`

### Golden Image (`golden/`)

Defines the Firecracker guest environment (not a Go module):

| File | Purpose |
|------|---------|
| `Dockerfile` | Guest rootfs definition — Ubuntu base + tools |
| `docker-to-ext4.sh` | Converts Docker image → sparse ext4 filesystem |
| `nss_catchall.c` | NSS module enabling any-username SSH login |
| `vsock_listen.c` | Listens for migration nudge over vsock |
| `config/` | systemd units, SSH config, metadata fetch scripts installed in guest |

## Shell Scripts (`scripts/`)

| Script | Role |
|--------|------|
| `boxcutter-ctl` | Firecracker VM manager — create, destroy, list, shell, logs, golden build |
| `boxcutter-setup` | Bundle validation + secret generation on first boot |
| `boxcutter-net` | Per-VM TAP device + fwmark-based policy routing setup |
| `boxcutter-tls` | CA + leaf certificate generation |
| `boxcutter-ssh` | SSH identity wrapper |

## Systemd Units (`systemd/`)

Service units for all node-side daemons: `boxcutter-node`, `vmid`, `boxcutter-proxy`, `boxcutter-net` (oneshot), `boxcutter-derper`.

## Key Architectural Concepts

**Networking**: Every Firecracker VM gets IP `10.0.0.2` on its own TAP device. VMs are distinguished by fwmark-based policy routing — each TAP gets a unique fwmark, and iptables/ip-rule uses fwmarks to route traffic correctly. See `node/docs/network.md`.

**Identity**: The vmid service identifies which VM a request comes from by reading the fwmark from the socket. This allows it to serve VM-specific metadata and tokens without VMs needing to authenticate.

**Sentinel tokens**: In paranoid mode, VMs receive sentinel tokens (fake credentials). When a VM makes an API call through the proxy, the proxy asks vmid to swap the sentinel for the real credential. Real credentials never enter the VM.

**Migration**: Firecracker snapshot/restore. The agent snapshots VM state + memory, transfers to the target node, and restores. The vsock listener inside the guest handles the "nudge" to flush state before snapshot.

## Build

```bash
cd node/agent && go build -o boxcutter-node ./cmd/node/
cd node/vmid && go build -o vmid ./cmd/vmid/
cd node/proxy && go build -o boxcutter-proxy ./cmd/proxy/
```

## Deploy (to running node VM)

```bash
scp boxcutter-node ubuntu@192.168.50.3:/tmp/
ssh ubuntu@192.168.50.3 "sudo mv /tmp/boxcutter-node /usr/local/bin/ && sudo systemctl restart boxcutter-node"
```

Same pattern for vmid and boxcutter-proxy.

## Detailed Documentation

- `docs/architecture.md` — Agent API, vmid endpoints, proxy, golden image, migration, normal/paranoid mode
- `docs/network.md` — fwmark routing, TAP setup, vmid identity, sentinel tokens, TLS, DERP, packet flows
- `docs/development.md` — Building, deploying, debugging, golden image rebuilding

## Domain Boundary

This component **owns**:
- Firecracker VM lifecycle (create, destroy, start, stop, migrate)
- Per-VM networking (TAP devices, fwmark routing, policy rules)
- VM identity (fwmark → VM mapping, metadata API)
- Credential brokering (sentinel tokens, proxy)
- Golden image building (Dockerfile → ext4)
- Node-local resource tracking (RAM, CPU, disk available)

This component **does not own**:
- VM scheduling (which node a VM lands on — that's orchestrator)
- SSH key management (that's orchestrator)
- MQTT broker (that's host; node is a client)
- Golden image head version (that's orchestrator; node pulls what it's told)
