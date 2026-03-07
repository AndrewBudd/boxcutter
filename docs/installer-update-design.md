# Boxcutter Installer & Update Process Design

## Current State

Today, provisioning is manual and sequential:

1. Human runs `make setup` on the physical host (bridge, NAT, TAP)
2. Human runs `make provision-orchestrator` then `make launch-orchestrator`
3. Human runs `make provision-node` then `make launch-node`
4. Human SSHes into the node, runs `tailscale up`, places auth keys
5. Human runs `boxcutter-ctl golden build && golden provision`

Each of these steps builds from source (`go build` in provision.sh), packages
binaries + config into a cloud-init ISO, and boots a QEMU VM. There's no
versioning, no automated upgrades, and no way to update a running system without
rebuilding everything manually.

## Design Principles

1. **Control plane makes the service exist and keeps it existing** — it creates
   the QEMU VMs (orchestrator + nodes), deploys new versions, adds/frees
   capacity, and ensures the orchestrator and nodes stay up. It manages all
   resources outside the VMs: bridge, NAT, TAP, disk images. It runs on bare
   metal as a Go binary.
2. **Data plane is the service** — the orchestrator and node VMs are the
   running service. Everything that happens at runtime — creating Firecracker
   dev VMs, scheduling them onto nodes, distributing SSH keys, building golden
   images, handling user sessions — is a data plane activity owned by the
   orchestrator and nodes.
3. **Nothing inside the VMs can reach the control plane** — the control plane's
   API (unix socket) is strictly localhost-only. The orchestrator, nodes, and
   Firecracker VMs have no mechanism to communicate with it. This is a hard
   security boundary.
4. **Control plane calls into VMs, never the reverse** — during upgrades and
   capacity changes, the control plane reaches into nodes to check capacity
   and move Firecrackers. Communication is always initiated by the control
   plane.
5. **If the control plane goes away, everything keeps running** — the data
   plane operates independently. Users can create Firecracker VMs, SSH in,
   distribute keys, build golden images — all without the control plane. The
   only thing that stops working is adding capacity, upgrading, and crash
   recovery of QEMU VMs.
6. **Nodes are immutable cattle** — upgrade by building a new node VM and
   draining the old one, never by patching in place.
7. **Build from source every time** — no binary distribution; git tags provide
   versioning.

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────┐
│  Physical Host (bare metal)                                  │
│                                                              │
│  boxcutter-host (Go binary) — CONTROL PLANE                  │
│    - creates QEMU VMs (orchestrator + nodes)                 │
│    - manages all resources outside VMs:                      │
│        bridge, TAP devices, NAT, disk images                 │
│    - auto-scales capacity (adds/removes node VMs)            │
│    - drains nodes (snapshot migration via node agent API)     │
│    - ensures orchestrator + node VMs are up                  │
│    - calls INTO VMs to check capacity, move Firecrackers     │
│    - control plane: unix socket only (localhost)              │
│    - NOTHING inside the VMs can communicate with it          │
│                                                              │
│  ┌───────────────────────────────────────────────────────┐   │
│  │  DATA PLANE (the running service)                     │   │
│  │                                                       │   │
│  │  Orchestrator VM          Node VMs (immutable)        │   │
│  │  - SSH entry point        - run Firecracker microVMs  │   │
│  │  - user auth (SSH keys)   - vmid, proxy, agent        │   │
│  │  - schedules Firecrackers - golden image builds       │   │
│  │    onto nodes             - report to orchestrator    │   │
│  │  - distributes keys         on startup                │   │
│  │  - queries nodes for state Node VM 2... N             │   │
│  │                                                       │   │
│  │  Operates independently of control plane.             │   │
│  │  Cannot reach control plane.                          │   │
│  └───────────────────────────────────────────────────────┘   │
│                                                              │
│  The operator interacts with the control plane via CLI on    │
│  the physical machine.                                       │
└──────────────────────────────────────────────────────────────┘
```

## Control Plane vs Data Plane

| Activity | Plane | Why |
|---|---|---|
| Create/destroy QEMU VMs (orchestrator, nodes) | Control | Making the service exist |
| Bridge, NAT, TAP networking | Control | Resources outside VMs |
| Auto-scale capacity (add/remove nodes) | Control | Scaling the service |
| Ensure orchestrator + nodes are up | Control | Keeping the service existing |
| Drain nodes / move Firecrackers between nodes | Control | Infrastructure + capacity management |
| Version rollouts / deployments | Control | Upgrading the service (future) |
| Create/destroy Firecracker dev VMs | **Data** | Service behavior (user request) |
| Schedule Firecrackers onto nodes | **Data** | Service behavior (orchestrator decides) |
| SSH key management + distribution | **Data** | Service behavior (user auth) |
| Golden image builds + distribution | **Data** | Service behavior (dev environment tooling) |
| User SSH sessions | **Data** | Service behavior |
| GitHub key / credential distribution | **Data** | Service behavior |

**Key test**: if the control plane goes away, does this still work? If yes,
it's data plane. If no, it's control plane.

### How the control plane interacts with nodes

The control plane does **not** persistently track Firecracker VMs. It doesn't
maintain a database of which Firecrackers are on which nodes. When it needs to
act (drain, capacity rebalance), it queries nodes in the moment:

```
Control plane: "node-1, what Firecrackers are you running?"
Node-1: [bold-fox, shy-elk, warm-jay]
Control plane: "node-1, migrate bold-fox to node-2"
(node-1 pauses VM, snapshots, transfers, node-2 resumes from snapshot)
```

These are **in-the-moment control decisions** — the control plane doesn't need
to remember what happened afterward.

### Nobody persistently tracks Firecrackers

Neither the control plane nor the orchestrator maintains a database of
Firecracker VMs. Both query nodes when they need to know:

- **Control plane** queries nodes during drains and capacity decisions. It
  doesn't remember afterward — each operation is self-contained.
- **Orchestrator** queries nodes when a user asks to create a VM (to find
  capacity) or when it needs to answer "where is my VM?" It fans out to
  all nodes and aggregates. No persistent replica of node state.
- **Nodes** are the source of truth. Each node knows exactly what Firecrackers
  it's running. Everyone else asks.

### Migration mechanics

Migration uses Firecracker snapshot/restore — the VM pauses (not stops),
its memory and state are snapshotted, transferred to the target node, and
resumed. Processes, memory, and Tailscale connections survive. Downtime is
~10s for a 2GB VM (dominated by memory file transfer).

The **node agent** implements all migration mechanics:
- `POST /api/vms/{name}/migrate` — pause, snapshot, transfer, resume on target
- `POST /api/vms/{name}/import-snapshot` — receive and resume a snapshot
- vsock nudge after resume to trigger Tailscale path re-discovery

The **control plane** coordinates: decides when to drain, which VMs to move,
which target node to use. It calls the node agent's API to execute.

The **orchestrator** never initiates migrations. Users cannot directly
migrate VMs or drain nodes.

## Control Plane Implementation

### `boxcutter-host` — Go binary on bare metal

```bash
# Operator interacts via CLI on the physical machine only
boxcutter-host status
boxcutter-host bootstrap --tailscale-authkey tskey-auth-XXXXX
```

Config lives at `/etc/boxcutter/host.yaml`:

```yaml
repo: /home/user/boxcutter        # git checkout used for builds
nic: enp34s0                      # physical NIC for NAT
bridge_subnet: 192.168.50.0/24    # bridge network
secrets_dir: /etc/boxcutter/secrets/
```

Cluster state in `/var/lib/boxcutter/cluster.json`:

```json
{
  "orchestrator": {
    "bridge_ip": "192.168.50.2",
    "disk": "/var/lib/boxcutter/images/orchestrator.qcow2",
    "pid": 1234
  },
  "nodes": [
    {
      "id": "node-1",
      "bridge_ip": "192.168.50.3",
      "disk": "/var/lib/boxcutter/images/node-1.qcow2",
      "pid": 5678,
      "vcpu": 6,
      "ram": "12G"
    }
  ]
}
```

### Responsibilities

1. **Boot recovery**: on startup, recreate bridge/NAT (idempotent), launch
   all VMs from cluster.json, verify health.

2. **Health monitoring**: periodic probes to orchestrator and node VMs.
   Restart crashed QEMU VMs.

3. **Auto-scaling**: poll nodes for capacity. When aggregate free capacity
   is low, check if the physical host can support another node VM. If so,
   provision and launch it. When nodes are underutilized, drain and retire
   excess nodes.

4. **Drain**: for each Firecracker on the node, call the node agent's
   migrate API to snapshot-migrate it to another node. Once empty, stop
   the QEMU VM and clean up.

### Bootstrap Flow

```bash
boxcutter-host bootstrap --tailscale-authkey tskey-auth-XXXXX
```

1. Set up bridge device and NAT rules (idempotent)
2. Build orchestrator binary from local git checkout
3. Package with cloud-init, secrets, config → ISO
4. Create QCOW2 disk, launch orchestrator VM
5. Build node agent binary from same checkout
6. Package, create disk, launch first node VM
7. Verify health of both VMs
8. Write cluster.json

### Boot Sequence (After Reboot)

```
Host reboot
  │
  ├─ systemd starts boxcutter-host.service
  │
  ├─ boxcutter-host reads cluster.json:
  │    - recreates bridge + NAT (idempotent)
  │    - launches orchestrator VM
  │    - launches all node VMs (in parallel)
  │    - verifies health
  │
  ├─ Data plane boots:
  │    - orchestrator starts, reads its DB (SSH keys)
  │    - nodes start, restart their Firecrackers, register with orchestrator
  │
  └─ System operational
```

## Data Plane Changes

### Orchestrator

The orchestrator is the user-facing SSH interface. It handles:
- `new` / `create` — schedule a Firecracker onto a node
- `ls` — fan out to all nodes, aggregate VM lists
- `destroy` / `stop` / `start` — proxy to the correct node
- `keys add/remove` — SSH key management
- `help` — show available commands

The orchestrator does NOT:
- Track VMs in a database (queries nodes on demand)
- Initiate migrations or drains (control plane only)
- Add or remove nodes (control plane only)

### Node Agent

Keeps all existing functionality:
- Firecracker VM lifecycle (create, start, stop, destroy)
- Snapshot-based migration (pause, snapshot, transfer, import, vsock nudge)
- Golden image management
- Health/capacity reporting
- Self-registration with orchestrator on boot

## Summary

| Component | Owns | Doesn't Own |
|---|---|---|
| **Control plane** | QEMU VMs, bridge/TAP/NAT, capacity scaling, drain/migration coordination, boot recovery | Firecracker VMs, user auth, scheduling |
| **Orchestrator** | User SSH, VM scheduling, SSH keys, golden image triggers | Migration, drain, node lifecycle, VM state DB |
| **Node agent** | Firecracker lifecycle, migration mechanics, golden images, capacity reporting | When to migrate, when to scale |
