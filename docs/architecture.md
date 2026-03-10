# Boxcutter System Architecture

Boxcutter provides ephemeral dev environment VMs using Firecracker microVMs. The system runs on a single physical host with three domains.

## Overview

```
┌──────────────────────────────────────────────────────────────────┐
│  Physical Host (bare metal, Ubuntu 24.04)                        │
│                                                                  │
│  CONTROL PLANE: boxcutter-host (Go binary, systemd service)      │
│    - Creates/destroys QEMU VMs (orchestrator + nodes)            │
│    - Bridge, TAP, NAT networking                                 │
│    - OCI image pull + VM provisioning                            │
│    - Health monitoring, auto-scaling                             │
│    - Drain/migration coordination                                │
│    - Mosquitto MQTT broker                                       │
│    - Unix socket API only — nothing inside VMs can reach it      │
│                                                                  │
│  br-boxcutter (192.168.50.1/24)                                  │
│    │                                                             │
│    ├── Orchestrator (192.168.50.2)                               │
│    │     SSH control interface (:22)                              │
│    │     HTTP API (:8801) — scheduling, VM registry, SSH keys    │
│    │     MQTT client — publishes golden image versions            │
│    │     SQLite DB — minimal state (VMs, keys)                   │
│    │                                                             │
│    ├── Node 1 (192.168.50.3)                                    │
│    │     boxcutter-node agent (:8800) — VM lifecycle, migration  │
│    │     vmid (:80) — fwmark-based VM identity + tokens          │
│    │     boxcutter-proxy (:8080) — MITM proxy, sentinel tokens   │
│    │     MQTT client — subscribes to golden image updates         │
│    │     Firecracker microVMs (each 10.0.0.2, isolated TAPs)    │
│    │                                                             │
│    └── Node N (auto-scaled)                                      │
│                                                                  │
│  If the control plane stops, everything keeps running.           │
│  Users can still create VMs, SSH in, use dev environments.       │
│  Only scaling, upgrades, and crash recovery stop working.        │
└──────────────────────────────────────────────────────────────────┘
```

## Three Domains

Each domain is defined by responsibility, not deployment topology.

### Host Control Plane (`host/`)

Infrastructure lifecycle. Owns everything outside the VMs: QEMU processes, bridge networking, NAT, OCI image distribution, auto-scaling, health monitoring, MQTT broker.

→ [host/docs/architecture.md](../host/docs/architecture.md) for internals

### Orchestrator (`orchestrator/`)

Distributed state manager and coordinator. Tracks which VMs exist on which nodes, handles user requests via SSH, manages SSH keys, coordinates golden image distribution. The fact that it runs inside a QEMU VM is incidental.

→ [orchestrator/docs/architecture.md](../orchestrator/docs/architecture.md) for internals

### Node (`node/`)

The fundamental system that manages Firecracker VMs as a resource. Contains the agent (VM lifecycle), vmid (identity), proxy (credential brokering), golden image (guest environment), and shell scripts (networking, TLS).

→ [node/docs/architecture.md](../node/docs/architecture.md) for internals

## Control Plane vs Data Plane

| Activity | Plane | Why |
|----------|-------|-----|
| Create/destroy QEMU VMs | Control | Making the service exist |
| Bridge, NAT, TAP networking | Control | Resources outside VMs |
| OCI image pull + provisioning | Control | Infrastructure lifecycle |
| Auto-scale capacity | Control | Scaling the service |
| Health monitoring + crash recovery | Control | Keeping the service existing |
| Drain nodes / coordinate migration | Control | Infrastructure management |
| Create/destroy Firecracker VMs | **Data** | User request (orchestrator schedules) |
| SSH key management | **Data** | User auth |
| Golden image builds | **Data** | Dev environment tooling |
| User SSH sessions | **Data** | Service behavior |

**Key test**: if the control plane goes away, does this still work? If yes, it's data plane.

## Communication Patterns

```
Host ──Unix socket──→ (local only, nothing in VMs can reach it)

Host ──MQTT broker──→ Orchestrator (publishes golden head)
                  ──→ Nodes (subscribe to golden updates)

Orchestrator ──HTTP──→ Node agents (VM CRUD, health, golden versions)
Node agents ──HTTP──→ Orchestrator (self-registration, heartbeat)

Users ──SSH──→ Orchestrator (ForceCommand interface)
```

- **HTTP**: Orchestrator ↔ Node agents (scheduling, VM lifecycle, health)
- **MQTT**: Golden image version distribution (broker on host at `192.168.50.1:1883`)
- **Unix socket**: Host API (`/run/boxcutter-host.sock`) — unreachable from VMs
- **SSH**: User-facing interface through orchestrator (ForceCommand)

## Network Topology

| Subnet/Address | Purpose |
|----------------|---------|
| `192.168.50.0/24` | Host bridge — orchestrator + all node VMs |
| `10.0.0.0/30` (per TAP) | Node to each Firecracker VM (point-to-point) |
| `100.x.x.x` | Tailscale overlay (external access) |

| Port | Service | Listener |
|------|---------|----------|
| 22 | SSH control interface | Orchestrator |
| 80 | vmid (metadata) | 169.254.169.254 on nodes |
| 443 | DERP relay | Node (`10.0.0.1`) |
| 1883 | Mosquitto MQTT broker | Host (`192.168.50.1`) |
| 8080 | Forward proxy | Nodes |
| 8800 | Node agent API | Nodes |
| 8801 | Orchestrator API | Orchestrator |

→ [node/docs/network.md](../node/docs/network.md) for fwmark routing, vmid, proxy, packet flows
→ [host/docs/network.md](../host/docs/network.md) for bridge setup

## Golden Image Distribution (MQTT)

The golden image is the Firecracker microVM rootfs (ext4). Cross-cutting concern spanning all three domains.

```
                        MQTT (mosquitto on host)
                              │
    Orchestrator ──publish──> │ ──subscribe──> Node-1
    (sets golden head)        │                (pulls from OCI)
                              │ ──subscribe──> Node-2
```

1. Operator pushes golden image to OCI: `sudo boxcutter-host push-golden`
2. Operator sets head: `ssh boxcutter golden set-head <version>`
3. Orchestrator publishes version to MQTT topic `boxcutter/golden/head` (retained, QoS 1)
4. Nodes pull the golden image from OCI, decompress, sparsify
5. New VMs use the updated golden image

## OCI Image Distribution

VM base images are distributed as OCI artifacts via GitHub Container Registry:

```
ghcr.io/andrewbudd/boxcutter/
  ├── node:latest          (~1.1GB zstd-compressed QCOW2)
  ├── orchestrator:latest  (~1.1GB zstd-compressed QCOW2)
  └── golden:latest        (~450MB zstd-compressed ext4)
```

Pull is anonymous (public packages). Push requires `gh auth login`.

→ [host/docs/architecture.md](../host/docs/architecture.md) for build pipeline and deploy flow details

## Migration

Firecracker VMs migrate between nodes using snapshot/restore. The host coordinates *when* to drain a node; the node agent handles *how* to migrate each VM. Downtime is ~10 seconds for a 2GB RAM VM.

→ [node/docs/architecture.md](../node/docs/architecture.md) for migration details

## Domain-Specific Documentation

| Domain | Architecture | Network | Development |
|--------|-------------|---------|-------------|
| Host | [host/docs/architecture.md](../host/docs/architecture.md) | [host/docs/network.md](../host/docs/network.md) | [host/docs/development.md](../host/docs/development.md) |
| Orchestrator | [orchestrator/docs/architecture.md](../orchestrator/docs/architecture.md) | — | [orchestrator/docs/development.md](../orchestrator/docs/development.md) |
| Node | [node/docs/architecture.md](../node/docs/architecture.md) | [node/docs/network.md](../node/docs/network.md) | [node/docs/development.md](../node/docs/development.md) |

## Improvement Proposal

See [improvement-proposal.md](improvement-proposal.md) for the architecture review and phased improvement plan covering:
- **Phase 1 (Safety):** Control plane lock, composite health checks, migration timeouts
- **Phase 2 (Observability):** Structured logging, layered health reporting
- **Phase 3 (Code Organization):** Split monolithic files, per-VM locks
