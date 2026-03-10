# Boxcutter

Ephemeral Firecracker dev environment VMs on a single physical host.

## Architecture

Three domains, defined by responsibility:

```
Host Control Plane (host/)
  │  Infrastructure lifecycle: QEMU VMs, networking, OCI images, scaling
  │
  ├── Orchestrator (orchestrator/)
  │     Distributed state & coordination: scheduling, SSH interface, key mgmt
  │
  └── Node (node/)
        Manages Firecracker VMs as a resource: lifecycle, identity, networking, proxy
        │
        └── Guest (node/golden/)
              The environment inside each Firecracker VM
```

## Go Modules

Five independent modules — each can be built/tested independently:

| Module | Path | Binary |
|--------|------|--------|
| Host control plane | `host/` | `boxcutter-host` |
| Orchestrator | `orchestrator/` | `boxcutter-orchestrator`, `boxcutter-ssh-orchestrator` |
| Node agent | `node/agent/` | `boxcutter-node` |
| VM identity | `node/vmid/` | `vmid` |
| MITM proxy | `node/proxy/` | `boxcutter-proxy` |

## Communication Patterns

- **HTTP**: Orchestrator ↔ Node agents (scheduling, VM lifecycle, health)
- **MQTT**: Golden image version distribution (broker on host, clients in orchestrator + nodes)
- **Unix socket**: Host API (`/run/boxcutter-host.sock`) — unreachable from inside VMs
- **SSH**: User-facing interface through orchestrator (ForceCommand)

## Network Topology

- `br-boxcutter` bridge: `192.168.50.1/24`
- Host: `192.168.50.1` (bridge IP, MQTT broker on `:1883`)
- Orchestrator: `192.168.50.2`
- Nodes: `192.168.50.3`, `.4`, `.5`... (auto-scaled)
- Firecracker VMs: each gets `10.0.0.2` on an isolated TAP device, distinguished by fwmark

## Key Make Targets

```bash
make build-host              # Build boxcutter-host binary
make install-host            # Build + install + systemd setup
make provision-node          # Build node VM (binaries + cloud-init + disk)
make provision-orchestrator  # Build orchestrator VM
make build-image TYPE=node   # Build OCI-distributable QCOW2 image
make publish TYPE=node       # Build + push image to ghcr.io
make help                    # Show all targets
```

## Testing

```bash
cd node/vmid && go test ./...   # Unit tests (only module with tests currently)
```

Other modules are validated through integration testing on real VMs.

## Documentation

Top-level:
- `docs/architecture.md` — System overview, domain boundaries, communication patterns
- `docs/development.md` — Prerequisites, code layout, build targets, CI/CD
- `docs/README.md` — Quick start / bootstrap guide

Domain-specific (architecture, networking, development):
- `host/docs/` — Host control plane internals
- `orchestrator/docs/` — Orchestrator internals
- `node/docs/` — Node internals (fwmark routing, vmid, proxy, packet flows)
