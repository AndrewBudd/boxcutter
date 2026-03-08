# Boxcutter System Architecture

This document describes the complete architecture of Boxcutter as implemented. For detailed networking internals, see [network-architecture.md](network-architecture.md).

## Overview

Boxcutter provides ephemeral dev environment VMs using Firecracker microVMs. The system runs on a single physical host with three layers:

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
│    ├── Orchestrator VM (192.168.50.2, 2 vCPU, 4G RAM)           │
│    │     SSH control interface (:22)                              │
│    │     HTTP API (:8801) — scheduling, VM registry, SSH keys    │
│    │     MQTT client — publishes golden image versions            │
│    │     SQLite DB — minimal state (VMs, keys)                   │
│    │                                                             │
│    ├── Node VM 1 (192.168.50.3, 6 vCPU, 12G RAM)               │
│    │     boxcutter-node agent (:8800) — VM lifecycle, migration  │
│    │     vmid (:80) — fwmark-based VM identity + tokens          │
│    │     boxcutter-proxy (:8080) — MITM proxy, sentinel tokens   │
│    │     MQTT client — subscribes to golden image updates         │
│    │     Firecracker microVMs (each 10.0.0.2, isolated TAPs)    │
│    │                                                             │
│    └── Node VM N (auto-scaled)                                   │
│                                                                  │
│  If the control plane stops, everything keeps running.           │
│  Users can still create VMs, SSH in, use dev environments.       │
│  Only scaling, upgrades, and crash recovery stop working.        │
└──────────────────────────────────────────────────────────────────┘
```

## Control Plane vs Data Plane

| Activity | Plane | Why |
|---|---|---|
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

## Components

### boxcutter-host (Control Plane)

Go binary running on bare metal as a systemd service. Owns all infrastructure outside VMs.

**Responsibilities:**
- **Bootstrap**: Pull OCI images, create COW disks, generate cloud-init ISOs, launch VMs, wait for health, set up golden image — all in one command
- **Boot recovery**: On reboot, recreate bridge/NAT (idempotent), relaunch VMs from `cluster.json`
- **Health monitoring**: 10s polling, auto-restart crashed QEMU VMs
- **Auto-scaling**: 30s polling, query nodes for capacity, launch new node when >80% utilized
- **Drain**: Migrate all Firecrackers off a node via node agent's migrate API, then stop QEMU
- **OCI image management**: Pull/push VM images from/to `ghcr.io/andrewbudd/boxcutter`
- **MQTT broker**: Runs Mosquitto on the host bridge for golden image notifications

**State**: `/var/lib/boxcutter/cluster.json` — orchestrator + nodes (PIDs, disks, IPs)

**Interfaces:**
- CLI: `boxcutter-host run|bootstrap|status|upgrade|pull|version|build-image`
- Unix socket API: `/run/boxcutter-host.sock` (`GET /status`, `POST /drain/{nodeID}`)
- Nothing inside VMs can communicate with it (hard security boundary)

### Orchestrator VM (Data Plane)

QEMU VM running on the bridge at `192.168.50.2`. User-facing SSH control interface.

**Responsibilities:**
- **SSH control interface**: `ssh boxcutter new/list/destroy/status/nodes/...`
- **VM scheduling**: Pick best node for new VMs based on capacity
- **SSH key management**: `adduser/removeuser/keys` — stores in SQLite, distributes to nodes
- **Golden image head**: Tracks current golden version, publishes to MQTT when updated
- **Node registry**: Nodes self-register on boot; orchestrator queries them for live state

**State**: SQLite at `/var/lib/boxcutter/orchestrator.db`
- `vms` table: (name, node_id, status) — thin state, detail fetched from nodes on demand
- `ssh_keys` table: GitHub username -> public keys
- `golden_head`: current golden image version

**Does NOT own:**
- Migration (control plane coordinates)
- Node lifecycle (control plane creates/destroys)
- Persistent VM state tracking (queries nodes on demand)

### Node VMs (Data Plane)

QEMU VMs running Firecracker microVMs. Each node is immutable — upgrade by launching a new one, draining the old.

**Services on each node:**
- `boxcutter-node` (:8800) — Node agent, HTTP API for VM lifecycle + migration
- `vmid` (:80) — VM identity via fwmark, JWT tokens, GitHub tokens, sentinel store
- `boxcutter-proxy` (:8080) — MITM forward proxy, sentinel token swapping
- `derper` (:443) — Local Tailscale DERP relay
- `caddy` (:8880/:8443) — Reverse proxy

**Node agent API:**

| Endpoint | Description |
|---|---|
| `POST /api/vms` | Create + start a Firecracker VM |
| `DELETE /api/vms/{name}` | Stop + destroy a VM |
| `GET /api/vms` | List VMs on this node |
| `GET /api/vms/{name}` | VM details |
| `PATCH /api/vms/{name}` | Update VM state (pause/resume) |
| `POST /api/vms/{name}/migrate` | Migrate VM to another node |
| `POST /api/vms/{name}/import-snapshot` | Import a migrated VM snapshot |
| `GET /api/golden` | List golden image versions |
| `GET /healthz` | Health check |

## OCI Image Distribution

VM base images are distributed as OCI artifacts via GitHub Container Registry.

```
ghcr.io/andrewbudd/boxcutter/
  ├── node:latest          (~1.1GB zstd-compressed QCOW2)
  ├── orchestrator:latest  (~1.1GB zstd-compressed QCOW2)
  └── golden:latest        (~450MB zstd-compressed ext4)
```

**Pull is anonymous** (public packages). Push requires `gh auth login`.

**Image build pipeline** (`host/publish-image.sh`):
1. Compile Go binaries for the VM type
2. Boot a temporary QEMU VM with cloud-init that installs everything
3. Clean instance-specific state (Tailscale logout, cloud-init reset, host keys)
4. Convert to standalone compressed QCOW2 + zstd
5. Push to ghcr.io with git hash + `latest` tags

**Deploy flow** (bootstrap or upgrade):
1. `oci.Pull()` downloads `.zst` from ghcr.io
2. `decompressZstd()` produces base QCOW2
3. `createCOWDisk()` creates thin overlay from base
4. `provision.sh --from-image` generates cloud-init ISO (injects secrets from `~/.boxcutter/`)
5. QEMU launches with COW disk + cloud-init ISO
6. Cloud-init runs a script that configures secrets, joins Tailscale, starts services

## Golden Image Distribution (MQTT)

The golden image is the Firecracker microVM rootfs (ext4). Nodes need it to create VMs.

```
                        MQTT (mosquitto on host)
                              │
    Orchestrator ──publish──> │ ──subscribe──> Node-1
    (sets golden head)        │                (pulls from OCI)
                              │ ──subscribe──> Node-2
```

**Flow:**
1. Operator pushes golden image to OCI: `make publish TYPE=golden`
2. Operator sets head: `ssh boxcutter golden set-head <version>`
3. Orchestrator publishes version to MQTT topic `boxcutter/golden/head` (retained, QoS 1)
4. Node MQTT clients receive notification
5. Nodes pull the golden image from OCI, decompress, sparsify
6. New VMs use the updated golden image

**MQTT details:**
- Broker: Mosquitto on host at `192.168.50.1:1883`
- Topic: `boxcutter/golden/head` (retained message)
- Client IDs: `boxcutter-orchestrator`, `boxcutter-node-<name>`
- paho.mqtt.golang with `SetConnectRetry(true)` + `SetAutoReconnect(true)`
- Single `Connect()` call only — paho handles all retries internally

## Migration (Snapshot-based)

Firecracker VMs migrate between nodes using Firecracker's snapshot/restore API. The VM pauses (not stops), its full state is captured, transferred, and resumed on the target.

```
Source Node                              Target Node
    │                                        │
    ├─ Pre-stage golden image ──────────────>│  (while VM runs, zero downtime)
    │                                        │
    ├─ PATCH /vm {"state":"Paused"}          │  (sub-millisecond pause)
    ├─ PUT /snapshot/create                  │  (vm.snap + vm.mem files)
    │                                        │
    ├─ tar --sparse COW+snap+mem ──SSH──────>│  (~10s for 2GB RAM)
    │                                        │
    │                                        ├─ fresh firecracker --api-sock
    │                                        ├─ PUT /snapshot/load {resume: true}
    │                                        ├─ vsock nudge -> tailscale netcheck
    │                                        │
    ├─ Stop source, cleanup                  │
    │                                        │
```

**What survives migration:**
- All running processes and memory state
- Tailscale identity and IP (state in `/var/lib/tailscale/` on rootfs)
- Network connections (after DERP re-establishment via vsock nudge)

**Downtime:** ~10 seconds for a 2GB RAM VM (dominated by memory file transfer over bridge network). gzip compression is slower than raw transfer on local bridge (CPU-bottlenecked).

**vsock nudge:** After snapshot restore, the node agent connects to the VM's vsock device (guest_cid=3, port 52) and triggers `tailscale netcheck` inside the VM. This re-establishes DERP connections through the new node's network path.

## Secrets and Bootstrap

All secrets live in `~/.boxcutter/secrets/` on the host (gitignored). They are injected into VMs via cloud-init ISOs at provisioning time.

| Secret | Purpose |
|---|---|
| `tailscale-node-authkey` | Tailscale auth key for orchestrator + node VMs (reusable, not ephemeral) |
| `tailscale-vm-authkey` | Tailscale auth key for Firecracker microVMs (reusable, ephemeral) |
| `cluster-ssh.key` / `.pub` | SSH key for inter-node communication (migration, deploy) |
| `authorized-keys` | SSH public keys for the boxcutter control interface |
| `github-app.pem` | GitHub App private key (for repo cloning in VMs) |

Config template at `~/.boxcutter/boxcutter.yaml` defines GitHub App IDs, Tailscale key paths, and per-node placeholders that are templated during provisioning.

## Networking

See [network-architecture.md](network-architecture.md) for the complete networking design including:
- Per-TAP fwmark routing (every VM is 10.0.0.2)
- vmid fwmark-based identity
- Normal vs paranoid mode
- Sentinel token swapping
- TLS infrastructure
- Packet flow diagrams

## File Structure

```
boxcutter/
├── host/                           # Physical host control plane
│   ├── boxcutter.env               # Network and resource configuration
│   ├── boxcutter-host.service      # systemd unit
│   ├── cmd/host/main.go            # Control plane binary
│   ├── internal/                   # bridge, cluster state, qemu, OCI
│   ├── provision.sh                # Cloud-init ISO generation
│   ├── build-image.sh              # Build VM base images (QCOW2)
│   ├── publish-image.sh            # Build + push images to ghcr.io
│   └── mosquitto.conf              # MQTT broker config
├── orchestrator/                   # Orchestrator VM (Go)
│   ├── cmd/orchestrator/           # HTTP API server (:8801)
│   ├── cmd/ssh/                    # SSH ForceCommand binary
│   └── internal/                   # api, db, mqtt, scheduler
├── node/                           # Node VM
│   ├── agent/                      # Node agent (Go)
│   │   ├── cmd/node/               # HTTP API server (:8800)
│   │   └── internal/               # vm, fcapi, networking, mqtt, golden
│   ├── golden/                     # Golden image (Firecracker rootfs)
│   │   ├── build.sh                # Phase 1: debootstrap rootfs
│   │   ├── provision.sh            # Phase 2: install dev tools
│   │   ├── nss_catchall.c          # NSS module for any-username SSH
│   │   └── vsock_listen.c          # vsock listener for migration nudge
│   ├── proxy/                      # MITM forward proxy (Go)
│   ├── vmid/                       # VM identity & token broker (Go)
│   ├── scripts/                    # boxcutter-net, boxcutter-tls, etc.
│   └── systemd/                    # Service unit files
├── docs/                           # Documentation
├── Makefile                        # Build and management targets
└── .images/                        # VM disk images (gitignored)
```
