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
│    - orchestrates version rollouts / deployments             │
│    - adds / frees capacity (node VMs)                        │
│    - ensures orchestrator + node VMs are up                  │
│    - calls INTO VMs to check capacity, move Firecrackers     │
│    - control plane: unix socket only (localhost)             │
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
| Version rollouts / deployments | Control | Upgrading the service |
| Add / free node capacity | Control | Scaling the service |
| Ensure orchestrator + nodes are up | Control | Keeping the service existing |
| Drain nodes / move Firecrackers between nodes | Control | Infrastructure + capacity management |
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
act (drain, capacity rebalance, health check), it queries nodes in the moment:

```
Control plane: "node-1, what Firecrackers are you running?"
Node-1: [bold-fox, shy-elk, warm-jay]
Control plane: "node-1, stop bold-fox and export its COW"
Control plane: "node-2, import this COW and start bold-fox"
```

These are **in-the-moment control decisions** — the control plane doesn't need
to remember what happened afterward.

### Nobody persistently tracks Firecrackers

Neither the control plane nor the orchestrator maintains a database of
Firecracker VMs. Both query nodes when they need to know:

- **Control plane** queries nodes during drains and capacity decisions. It
  doesn't remember afterward — each operation is self-contained.
- **Orchestrator** queries nodes when a user asks to create a VM (to find
  capacity) or when it needs to answer "where is my VM?" It doesn't maintain
  a persistent replica of node state.
- **Nodes** are the source of truth. Each node knows exactly what Firecrackers
  it's running. Everyone else asks.

## Installation Flow

### Phase 0: Install the control plane

The operator installs `boxcutter-host` on the physical machine. This is a Go
binary that runs as a systemd service on bare metal.

```bash
git clone https://github.com/... boxcutter && cd boxcutter
make install-host
# Compiles host/cmd/host/main.go → /usr/local/bin/boxcutter-host
# Installs systemd unit → boxcutter-host.service
# Creates config dir → /etc/boxcutter/
```

`boxcutter-host` is responsible for:
- Creating/managing the bridge device, TAP devices, and NAT rules
- Building QEMU VM images from source (`go build` + cloud-init ISO packaging)
- Creating/destroying/monitoring QEMU VMs (orchestrator + nodes)
- Health-checking the orchestrator and nodes, restarting them on crash
- Orchestrating version rollouts and deployments
- Adding/freeing node capacity
- Calling into nodes during upgrades to check capacity and move Firecrackers

The control plane is a **unix socket** (`/run/boxcutter-host.sock`) and a CLI
that talks to it. It is NOT exposed on any network interface. Nothing inside the
bridge network can reach it.

```bash
# Operator interacts via CLI on the physical machine only
boxcutter-host status
boxcutter-host node add --vcpu 12 --ram 48G
boxcutter-host node remove node-2
boxcutter-host upgrade --to v0.4.0
```

Config lives at `/etc/boxcutter/host.yaml`:

```yaml
repo: /home/user/boxcutter        # git checkout used for builds
nic: enp34s0                      # physical NIC for NAT
bridge_subnet: 192.168.50.0/24    # bridge network
secrets_dir: /etc/boxcutter/secrets/
```

### Phase 1: Bootstrap the cluster

The operator runs a single bootstrap command that creates the orchestrator
and the first node:

```bash
boxcutter-host bootstrap \
  --tailscale-authkey tskey-auth-XXXXX
```

This command:
1. Sets up the bridge device and NAT rules (idempotent)
2. Builds the orchestrator binary from the local git checkout
3. Packages it with cloud-init, secrets, and config into an ISO
4. Creates a QCOW2 disk, launches the orchestrator VM
5. Builds the node agent binary from the same git checkout
6. Packages it, creates disk, launches the first node VM
7. Calls into each VM to verify health
8. Records the cluster state in `/var/lib/boxcutter/cluster.json`

The control plane assigns all identities at build time:
- **Orchestrator**: bridge IP `192.168.50.2`, hostname `boxcutter-orchestrator`
- **Node 1**: bridge IP `192.168.50.3`, hostname `boxcutter-node-1`
- **Secrets**: Tailscale keys, SSH keypairs, CA certs — all baked into cloud-init

### Phase 2: System is operational

Once the orchestrator and at least one node are up, the **data plane takes
over**. The service is now running and handles everything at runtime:

- Users SSH into the orchestrator to create Firecracker dev VMs
- Orchestrator schedules Firecrackers onto available nodes
- Orchestrator distributes SSH keys, manages golden images
- Nodes run Firecrackers, vmid, proxy

The control plane's ongoing role is limited to:
- Health-checking the orchestrator and node VMs (restart on crash)
- Adding/removing node capacity when the operator requests it
- Upgrading versions when the operator requests it

If the control plane process stops, the data plane continues operating
normally. Users can create and use Firecracker VMs without interruption.

## Versioning: Git Tags

Every release is a git tag: `v0.1.0`, `v0.2.0`, etc.

```bash
git tag -a v0.3.0 -m "Add paranoid mode sentinel tokens"
git push origin v0.3.0
```

The control plane knows:
- **Its own version** — baked in at build time via `-ldflags "-X main.Version=v0.3.0"`
- **What version each QEMU VM was built from** — recorded in `cluster.json`
- **What git ref to use for the next build** — configurable, defaults to latest tag

```go
// Injected at build time (in all binaries)
var Version = "dev"
```

The control plane exposes version info via CLI:

```bash
$ boxcutter-host status
Control plane:  v0.4.0
Orchestrator:   v0.3.0 (bridge: 192.168.50.2, healthy)
Nodes:
  node-1:       v0.3.0 (bridge: 192.168.50.3, healthy)
  node-2:       v0.3.0 (bridge: 192.168.50.4, healthy)
Latest tag:     v0.4.0
```

## Upgrade Process

All upgrades are initiated by the operator via the control plane CLI. Nothing
inside the VMs can trigger an upgrade or knows the control plane exists.

### Upgrading Nodes (zero-downtime)

Nodes are immutable. Upgrading means building a new one, moving Firecrackers
off the old one, then retiring it.

```bash
boxcutter-host upgrade --to v0.4.0
# Or: upgrade all components to latest tag
boxcutter-host upgrade --latest
```

The control plane performs the following steps:

```
1. git fetch && git checkout v0.4.0 (in the configured repo)
2. Build new node image from that ref (go build + cloud-init ISO)
3. Launch new node VM with next available bridge IP
4. Call into new node to verify it's healthy and ready
5. Drain old node:
   a. Call into old node: list all running Firecrackers
   b. For each Firecracker: stop it, rsync COW to new node, start on new node
   c. Control plane coordinates each migration directly
6. Once drained, stop old node VM
7. Old node's disk is archived or deleted
8. Update cluster.json

Timeline:
  [0s]   Build new node image (~30s for go build + ISO)
  [30s]  Launch new node VM
  [90s]  New node is ready (cloud-init + health check)
  [90s+] Drain old node (depends on VM count, COW sizes)
         Each VM migration: stop + rsync COW + start on new node
  [done] Old node retired
```

Active VMs experience a brief interruption during migration (stop → transfer →
start), but the system as a whole stays available — other nodes continue serving.

### Upgrading the Orchestrator (brief downtime)

The control plane upgrades the orchestrator. Running Firecracker VMs are
unaffected — they don't depend on the orchestrator at runtime.

The orchestrator's QCOW2 disk persists across upgrades — the control plane
replaces the cloud-init ISO (new binary + config) but reuses the same disk.
The SQLite DB on disk survives the upgrade.

```bash
boxcutter-host upgrade-orchestrator --to v0.4.0
```

**Upgrade flow:**

```
1. Control plane builds new orchestrator binary from target git ref
2. Control plane packages new cloud-init ISO (new binary, same secrets)
3. Control plane stops old orchestrator VM
4. Control plane launches orchestrator VM with same QCOW2 disk + new ISO
5. New orchestrator boots:
   - SQLite DB is intact on disk (SSH keys preserved)
   - Ready to serve SSH connections
6. System fully operational again

Downtime: ~60-90 seconds (VM stop + VM boot + cloud-init)
```

For major version changes that require a DB migration, the new orchestrator
binary handles it on startup (standard SQLite migration pattern).

### Upgrading the Golden Image

Golden image upgrades are a data plane concern — the orchestrator and nodes
handle this without control plane involvement. The operator triggers it via
SSH to the orchestrator:

```
1. Orchestrator triggers golden image rebuild on a node (debootstrap + provision)
2. Orchestrator distributes new image to all nodes (rsync over Tailscale)
3. New Firecracker VMs use the new golden image
4. Existing VMs are unaffected (they have their own COW snapshots)
```

No control plane involvement. No migration needed.

## Data Directionality

```
Control plane (bare metal)     ← operator controls via CLI
  │
  │ creates/destroys QEMU VMs
  │ calls into VMs during upgrades + health checks
  │ manages all resources outside VMs
  │ (one-way: control plane → VMs)
  ▼
┌──────────────────────────────────────────┐
│  Data Plane (the running service)        │
│                                          │
│  Orchestrator VM  ←→  Node VMs           │
│  (user-facing)        (run Firecrackers) │
│                                          │
│  Operates independently.                 │
│  Cannot reach control plane.             │
│  Communicates internally via bridge      │
│  network and Tailscale.                  │
└──────────────────────────────────────────┘
```

### Control plane → Nodes (in-the-moment queries and commands)

The control plane calls into nodes when it needs to act. It does not
persistently track Firecracker state — it queries and acts in the moment:

- **Health checks**: periodic probes to orchestrator and node VMs
- **Capacity queries**: ask nodes how much vCPU/RAM is free (during capacity decisions)
- **Firecracker listing**: ask nodes what's running (during drain)
- **Firecracker stop/start/export/import**: direct commands to nodes during drain
- **rsync COW data**: transfer Firecracker disk between nodes during drain

The control plane talks directly to node agents for all of this. It never
needs to involve the orchestrator in infrastructure operations.

### Inside the data plane (VM ↔ VM, all runtime activity)

The orchestrator and nodes communicate over bridge network and Tailscale.
All service behavior lives here:

- **Firecracker creation**: user SSHes to orchestrator → orchestrator calls
  node agent to create Firecracker VM
- **Scheduling**: orchestrator checks node capacity, picks best node
- **SSH key distribution**: orchestrator pushes authorized keys to nodes
- **Golden image builds**: orchestrator triggers on a node, distributes
- **GitHub key / credential management**: orchestrator distributes to nodes
- **Node registration**: nodes report to orchestrator on startup so it knows
  what nodes are available. Orchestrator queries nodes for Firecracker state
  as needed — no persistent tracking.

### Upward flow (none)

There is no upward flow. The orchestrator and nodes cannot reach the control
plane. They don't know it exists. The control plane is not part of their
configuration, not in their DNS, not on any network they can reach.

### Bootstrap data for a new node

Minimal data a node needs to boot (all assigned by control plane at build time):

```yaml
# Assigned by control plane, baked into cloud-init ISO
node:
  id: "node-1"
  hostname: "boxcutter-node-1"
  bridge_ip: "192.168.50.3"

# Passed through from operator-provided secrets
secrets:
  tailscale_node_authkey: "tskey-auth-XXX"    # persistent key for node itself
  tailscale_vm_authkey: "tskey-auth-YYY"      # ephemeral key for VMs
  ssh_private_key: "..."                       # for SSHing into VMs
  authorized_keys: ["ssh-rsa ...", ...]        # who can access

# Points at the orchestrator (data plane internal communication)
orchestrator:
  url: "http://192.168.50.2:8801"
```

## Boot Sequence (After Install)

```
Host reboot
  │
  ├─ systemd starts boxcutter-host.service
  │
  ├─ boxcutter-host reads /var/lib/boxcutter/cluster.json:
  │    - recreates bridge + NAT (idempotent)
  │    - launches orchestrator VM
  │    - launches all node VMs (in parallel)
  │    - calls into each VM to verify health
  │
  ├─ Data plane boots:
  │    - orchestrator starts, reads its DB (SSH keys)
  │    - nodes start, restart their own Firecrackers, register with orchestrator
  │
  └─ System operational

The control plane launches the QEMU VMs and verifies they're healthy.
The data plane handles everything else: restarting Firecrackers,
re-establishing node registration, resuming service.
```

## Version / Release Workflow

```bash
# Developer workflow
git checkout main
# ... make changes ...
git commit -m "Add feature X"
git tag -a v0.4.0 -m "Release v0.4.0: feature X"
git push origin main --tags

# Operator triggers upgrade on the physical host
boxcutter-host upgrade --to v0.4.0
# or: auto-upgrade to latest tag
boxcutter-host upgrade --latest
```

The control plane detects what changed between versions and upgrades
accordingly:

```bash
git diff v0.3.0..v0.4.0 --name-only
# If orchestrator/ changed → orchestrator upgrade (stop, rebuild ISO, relaunch)
# If node/ changed → rolling node upgrade (build new, drain old, retire)
# If host/ changed → control plane self-update (rebuild binary, systemctl restart)
# If golden/ changed → no control plane action (data plane concern)
```

## Summary

| Component | Install | Upgrade | State | Plane |
|---|---|---|---|---|
| **Control plane** | `make install-host` (Go binary + systemd) | Self-update via `boxcutter-host upgrade` | `/etc/boxcutter/host.yaml` + `/var/lib/boxcutter/cluster.json` | Control |
| **Orchestrator** | Built by control plane during bootstrap | Control plane rebuilds ISO, relaunches (same disk) | SQLite DB on QCOW2 disk (SSH keys only, survives upgrades) | Data |
| **Node VMs** | Built by control plane (`node add` or bootstrap) | Control plane: build new → drain → retire | Stateless (Firecrackers have their own COW state) | Data |
| **Golden image** | Built by orchestrator on a node | Rebuild + distribute (data plane, no control plane) | ext4 file on each node | Data |
| **Firecracker VMs** | Created by orchestrator via node agent | N/A (ephemeral, destroy and recreate) | COW snapshot (per-VM, backed by golden image) | Data |
