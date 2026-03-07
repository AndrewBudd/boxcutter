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

1. **Control plane owns everything outside the VMs** — it is the sole authority
   over all infrastructure: creating/destroying VMs (both QEMU nodes and
   Firecracker dev VMs), version rollouts, capacity management, health
   monitoring, bridge/NAT/TAP networking. It runs on bare metal as a Go binary.
2. **Nothing inside the VMs can reach the control plane** — the control plane's
   API (unix socket) is strictly localhost-only. The orchestrator, nodes, and
   Firecracker VMs have no mechanism to communicate with it. This is a hard
   security boundary.
3. **Control plane calls into VMs, never the reverse** — the control plane
   reaches into nodes to check capacity, enumerate running Firecrackers, move
   workloads during drains, and verify health. Communication is always initiated
   by the control plane.
4. **Orchestrator is the user-facing entry point** — it runs inside a VM and
   handles SSH sessions, user authentication (SSH keys), and exposes the
   user-facing API. It has no infrastructure authority and no awareness of the
   control plane.
5. **Nodes are immutable cattle** — upgrade by building a new node VM and
   draining the old one, never by patching in place.
6. **Build from source every time** — no binary distribution; git tags provide
   versioning.

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────┐
│  Physical Host (bare metal)                                  │
│                                                              │
│  boxcutter-host (Go binary) — THE CONTROL PLANE              │
│    - creates ALL VMs: orchestrator, nodes, Firecracker VMs   │
│    - manages all resources outside VMs:                      │
│        bridge, TAP devices, NAT, disk images                 │
│    - orchestrates version rollouts / deployments             │
│    - adds / frees capacity                                   │
│    - ensures orchestrator + allocated VMs are up             │
│    - calls INTO VMs to check capacity, move Firecrackers     │
│    - control plane: unix socket only (localhost)             │
│    - NOTHING inside the VMs can communicate with it          │
│                                                              │
│  ┌─────────────────────┐  ┌──────────────────────────────┐   │
│  │ Orchestrator VM     │  │ Node VM (immutable)          │   │
│  │ - SSH entry point   │  │ - runs Firecracker microVMs  │   │
│  │ - user auth (keys)  │  │ - vmid, proxy, agent         │   │
│  │ - user-facing API   │  │ - responds to control plane  │   │
│  │ - no infra authority│  │   queries (capacity, VM list) │   │
│  │ - no host access    │  │                              │   │
│  └─────────────────────┘  │ Node VM 2... N               │   │
│                           └──────────────────────────────┘   │
│                                                              │
│  Communication is ONE-WAY: control plane → VMs.              │
│  VMs cannot initiate communication with the control plane.   │
│  The operator interacts with the control plane via CLI on    │
│  the physical machine.                                       │
└──────────────────────────────────────────────────────────────┘
```

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
- Building all VM images from source (`go build` + cloud-init ISO packaging)
- Creating/destroying/monitoring all VMs (QEMU nodes + Firecracker dev VMs)
- Health-checking the orchestrator and all allocated VMs, restarting on crash
- Orchestrating version rollouts and deployments
- Managing host capacity (vCPU/RAM budgets across all VMs)
- Calling into node VMs to check capacity, enumerate Firecrackers, move workloads

The control plane is a **unix socket** (`/run/boxcutter-host.sock`) and a CLI
that talks to it. It is NOT exposed on any network interface. Nothing inside the
bridge network can reach it.

```bash
# Operator interacts via CLI on the physical machine only
boxcutter-host status
boxcutter-host vm list
boxcutter-host vm create --repo github.com/org/repo
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
7. Waits for both to come up (health checks via calls into each VM)
8. Records the cluster state in `/var/lib/boxcutter/cluster.json`

The control plane assigns all identities at build time:
- **Orchestrator**: bridge IP `192.168.50.2`, hostname `boxcutter-orchestrator`
- **Node 1**: bridge IP `192.168.50.3`, hostname `boxcutter-node-1`
- **Secrets**: Tailscale keys, SSH keypairs, CA certs — all baked into cloud-init

### Phase 2: Add capacity

The operator adds nodes via the control plane CLI:

```bash
boxcutter-host node add --vcpu 12 --ram 48G
# Control plane:
#   1. Assigns next bridge IP (192.168.50.4)
#   2. Builds node image from current git ref
#   3. Launches node VM
#   4. Calls into node to verify it's ready
```

The control plane decides IPs, resource allocation, and when nodes are healthy.
The orchestrator discovers new nodes only when users SSH in and the control
plane has already made them available.

### Phase 3: Golden image build

The control plane triggers the golden image build by calling into a node:

```
Control plane → Node: build golden image (debootstrap + provision)
Control plane → Nodes: distribute golden image to all nodes (rsync)
```

The golden image version is tracked by the control plane in `cluster.json`.

### Phase 4: Creating dev VMs

When a dev VM is needed, the control plane creates it:

```bash
boxcutter-host vm create \
  --name bold-fox \
  --repo github.com/org/repo \
  --vcpu 4 --ram 8G
```

The control plane:
1. Checks capacity across nodes (calls into each node's agent)
2. Picks the node with the most available resources
3. Calls into the selected node agent to create the Firecracker VM
4. Records the VM in `cluster.json`
5. Returns the VM's Tailscale IP / connection info

### Phase 5: System is operational

On host reboot, the control plane brings everything back up automatically:

1. systemd starts `boxcutter-host.service`
2. Control plane reads cluster state from `/var/lib/boxcutter/cluster.json`
3. Recreates bridge + NAT (idempotent)
4. Launches orchestrator VM, then all node VMs
5. Calls into each VM to verify health
6. Calls into each node to restart any Firecracker VMs that were previously
   allocated
7. System operational

## Versioning: Git Tags

Every release is a git tag: `v0.1.0`, `v0.2.0`, etc.

```bash
git tag -a v0.3.0 -m "Add paranoid mode sentinel tokens"
git push origin v0.3.0
```

The control plane knows:
- **Its own version** — baked in at build time via `-ldflags "-X main.Version=v0.3.0"`
- **What version each VM was built from** — recorded in `cluster.json`
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
  node-1:       v0.3.0 (bridge: 192.168.50.3, healthy, 8/12 vCPU used)
  node-2:       v0.3.0 (bridge: 192.168.50.4, healthy, 4/12 vCPU used)
Firecracker VMs: 6 running across 2 nodes
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
   c. Control plane orchestrates each migration directly
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

The control plane coordinates the entire drain directly — it calls into nodes
to stop/start Firecrackers and rsyncs data between them. No orchestrator
involvement.

Active VMs experience a brief interruption during migration (stop → transfer →
start), but the system as a whole stays available — other nodes continue serving.

### Upgrading the Orchestrator (brief downtime)

The control plane also upgrades the orchestrator. Running Firecracker VMs are
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

```
1. Control plane triggers golden image rebuild on a node
   (calls into node agent to run debootstrap + provision)
2. Control plane distributes new image to all nodes (rsync)
3. Control plane records new version in cluster.json
4. New Firecracker VMs use the new golden image
5. Existing VMs are unaffected (they have their own COW snapshots
   backed by whatever golden version they were created from)
```

No migration needed. Old VMs continue working with their old golden base.

## Data Directionality

Communication is strictly one-way: control plane → VMs. Nothing inside the
VMs can initiate a connection to the control plane.

```
Control plane (bare metal)     ← operator controls via CLI
  │
  │ calls into VMs (bridge network)
  │ creates/destroys VMs (QEMU + Firecracker)
  │ manages all resources outside VMs
  ▼
┌──────────────────────────────────────────┐
│  Environment (inside VMs)                │
│                                          │
│  Orchestrator VM  ←→  Node VMs           │
│  (user-facing)        (run Firecrackers) │
│                                          │
│  Cannot reach control plane.             │
│  Can communicate with each other via     │
│  bridge network and Tailscale.           │
└──────────────────────────────────────────┘
```

### Control plane → VMs (all communication)

The control plane calls into VMs for everything:

- **Node capacity checks**: `GET /api/capacity` on node agents
- **Firecracker creation**: `POST /api/vms` on node agents
- **Firecracker listing**: `GET /api/vms` on node agents
- **Firecracker stop/start**: `POST /api/vms/{name}/stop` on node agents
- **Drain coordination**: stop Firecracker on old node, rsync COW, start on new
- **Health checks**: periodic probes to orchestrator and all nodes
- **Golden image build**: trigger build + distribute via rsync

### Inside the environment (VM ↔ VM)

Within the environment, the orchestrator and nodes can communicate with each
other over the bridge network and Tailscale. This is internal to the
environment and invisible to the control plane:

- **Node registration with orchestrator**: nodes report their identity so the
  orchestrator knows what's available for user-facing queries
- **SSH key distribution**: orchestrator pushes authorized keys to nodes
- **User-facing VM info**: orchestrator queries nodes for VM status to display
  to users via SSH

### Upward flow (none)

There is no upward flow. The orchestrator and nodes cannot reach the control
plane. They don't know it exists.

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

# Points at the orchestrator (for internal VM-to-VM communication only)
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
  ├─ Calls into each node to restart allocated Firecracker VMs
  │
  └─ System operational

The control plane owns the entire boot sequence. It knows what VMs should
exist (from cluster.json), creates them, and verifies they're healthy by
calling into them. The orchestrator and nodes are passive — they boot and
wait to be used.
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
# If golden/ changed → golden image rebuild (trigger on a node, distribute)
```

## Responsibility Matrix

| Concern | Owner | Why |
|---|---|---|
| Bridge, NAT, TAP devices | Control plane | Bare metal resource |
| Building VM images (go build + ISO) | Control plane | Requires git checkout + Go toolchain on host |
| Creating/destroying QEMU node VMs | Control plane | Bare metal process management |
| Creating/destroying Firecracker VMs | Control plane | Calls into node agents |
| Health-checking all VMs | Control plane | Availability — restart on crash |
| Rolling node upgrades | Control plane | Build new, drain (move Firecrackers), retire |
| Orchestrator upgrades | Control plane | Rebuild ISO, relaunch |
| Golden image builds + distribution | Control plane | Triggers on node, distributes via rsync |
| Capacity management (vCPU/RAM) | Control plane | Physical resource limits + scheduling |
| Drain coordination (moving Firecrackers) | Control plane | Calls into nodes to stop/rsync/start |
| SSH entry point for users | Orchestrator | User-facing service inside environment |
| SSH key management | Orchestrator | User authentication |
| User-facing VM status queries | Orchestrator | Queries nodes for display to users |

## Summary

| Component | Install | Upgrade | State |
|---|---|---|---|
| **Control plane** | `make install-host` (Go binary + systemd) | Self-update via `boxcutter-host upgrade` | `/etc/boxcutter/host.yaml` + `/var/lib/boxcutter/cluster.json` |
| **Orchestrator** | Built by control plane during bootstrap | Control plane rebuilds ISO, relaunches (same disk) | SQLite DB on QCOW2 disk (SSH keys, survives upgrades) |
| **Node VMs** | Built by control plane (`node add` or bootstrap) | Control plane: build new → drain → retire | Stateless (Firecrackers have their own COW state) |
| **Golden image** | Built by control plane on a node | Rebuild + distribute (existing VMs unaffected) | ext4 file on each node |
| **Firecracker VMs** | Created by control plane via node agent | N/A (ephemeral, destroy and recreate) | COW snapshot (per-VM, backed by golden image) |
