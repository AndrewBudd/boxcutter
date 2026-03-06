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

1. **Host daemon owns infrastructure** — it is the sole authority over VM lifecycle
   (create, destroy, health-check, upgrade, capacity). It runs on bare metal,
   outside the environment it manages. Its control plane is never exposed to
   anything inside the VMs or nodes.
2. **Orchestrator owns workloads** — it schedules dev VMs onto nodes, manages SSH
   keys, distributes golden images, and coordinates migrations. It lives *inside*
   the environment as a VM and has no knowledge of or control over the host daemon.
3. **Strict isolation between planes** — the host daemon's API (unix socket or
   localhost-only) is unreachable from the bridge network. The orchestrator and
   nodes cannot call up to it. The human operator is the only actor that interacts
   with the host daemon directly.
4. **Nodes are immutable cattle** — upgrade by building a new node VM and draining
   the old one, never by patching in place
5. **Orchestrator state is reconstructible** — everything in the orchestrator DB
   can be rebuilt by querying live nodes
6. **Build from source every time** — no binary distribution; git tags provide
   versioning
7. **Data flows downward at provisioning time** — the host daemon templates node
   config (IPs, secrets, identity) into cloud-init at build time. At runtime,
   nodes and orchestrator operate independently.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│  Physical Host (bare metal)                             │
│                                                         │
│  boxcutter-host (Go binary)                             │
│    - manages bridge, TAP devices, NAT                   │
│    - creates/destroys/monitors QEMU VMs                 │
│    - builds VM images from source (go build + ISO)      │
│    - upgrades nodes (build new, drain old, retire)      │
│    - manages capacity (vCPU/RAM allocation)             │
│    - control plane: unix socket only (localhost)        │
│    - NOT accessible from bridge network or VMs          │
│                                                         │
│  ┌─────────────────────┐  ┌──────────────────────────┐  │
│  │ Orchestrator VM     │  │ Node VM (immutable)      │  │
│  │ - SQLite DB         │  │ - Firecracker microVMs   │  │
│  │ - schedules dev VMs │  │ - vmid, proxy, agent     │  │
│  │ - distributes golden│  │ - reports to orchestrator │  │
│  │ - manages SSH keys  │  │                          │  │
│  │ - no host access    │  │ Node VM 2... N           │  │
│  └─────────────────────┘  └──────────────────────────┘  │
│                                                         │
│  The host daemon and VMs are in separate trust domains. │
│  VMs cannot reach the host daemon. The operator manages │
│  the host daemon via CLI on the physical machine.       │
└─────────────────────────────────────────────────────────┘
```

## Installation Flow

### Phase 0: Install the host daemon

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
- Creating/managing the bridge device and NAT rules
- Building VM images from source (`go build` + cloud-init ISO packaging)
- Creating/destroying/monitoring QEMU VMs
- Health-checking VMs and restarting them if they crash
- Upgrading nodes (build new, drain old, retire)
- Managing host capacity (vCPU/RAM budgets across VMs)

The control plane is a **unix socket** (`/run/boxcutter-host.sock`) and a CLI
that talks to it. It is NOT exposed on any network interface. Nothing inside the
bridge network (orchestrator, nodes, VMs) can reach it.

```bash
# Operator interacts via CLI on the physical machine only
boxcutter-host status
boxcutter-host node list
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

The operator runs a single bootstrap command that creates both the orchestrator
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
7. Waits for both to come up (health checks)
8. Records the cluster state in `/var/lib/boxcutter/cluster.json`

The host daemon assigns all identities at build time:
- **Orchestrator**: bridge IP `192.168.50.2`, hostname `boxcutter-orchestrator`
- **Node 1**: bridge IP `192.168.50.3`, hostname `boxcutter-node-1`
- **Secrets**: Tailscale keys, SSH keypairs, CA certs — all baked into cloud-init

### Phase 2: Add more nodes

The operator adds nodes via the host daemon CLI:

```bash
boxcutter-host node add --vcpu 12 --ram 48G
# Host daemon:
#   1. Assigns next bridge IP (192.168.50.4)
#   2. Builds node image from current git ref
#   3. Launches node VM
#   4. Node boots, registers with orchestrator
```

The host daemon decides IPs and resource allocation. The orchestrator discovers
new nodes when they register — it doesn't create them.

### Phase 3: Golden image build

Once at least one node is up, the orchestrator triggers the golden image build
on a node (same two-phase process as today):

```
Orchestrator → Node Agent: POST /api/golden/build
Orchestrator → Node Agent: POST /api/golden/provision
```

The golden image version is tracked in the orchestrator DB. The orchestrator
distributes the golden image to all nodes (rsync over Tailscale).

### Phase 4: System is operational

On host reboot, the host daemon brings everything back up automatically:

1. systemd starts `boxcutter-host.service`
2. Host daemon reads cluster state from `/var/lib/boxcutter/cluster.json`
3. Recreates bridge + NAT (idempotent)
4. Launches orchestrator VM, then all node VMs
5. Health-checks each VM until it's responsive
6. Nodes boot, register with orchestrator
7. Orchestrator tells nodes to restart any dev VMs that were previously running

## Versioning: Git Tags

Every release is a git tag: `v0.1.0`, `v0.2.0`, etc.

```bash
git tag -a v0.3.0 -m "Add paranoid mode sentinel tokens"
git push origin v0.3.0
```

The host daemon knows:
- **Its own version** — baked in at build time via `-ldflags "-X main.Version=v0.3.0"`
- **What version each VM was built from** — recorded in `cluster.json`
- **What git ref to use for the next build** — configurable, defaults to latest tag

The orchestrator knows:
- **Its own version** — baked in at build time
- **What version each node is running** — reported during registration

```go
// Injected at build time (in all binaries)
var Version = "dev"

// Node registration includes version
type RegisterRequest struct {
    ID      string `json:"id"`
    Version string `json:"version"` // e.g. "v0.3.0"
    // ...
}
```

The host daemon exposes version info via CLI:

```bash
$ boxcutter-host status
Host daemon:    v0.4.0
Orchestrator:   v0.3.0 (bridge: 192.168.50.2, healthy)
Nodes:
  node-1:       v0.3.0 (bridge: 192.168.50.3, healthy, 8/12 vCPU used)
  node-2:       v0.3.0 (bridge: 192.168.50.4, healthy, 4/12 vCPU used)
Latest tag:     v0.4.0
```

## Upgrade Process

All upgrades are initiated by the operator via the host daemon CLI. The
orchestrator and nodes never trigger their own upgrades — they don't know the
host daemon exists.

### Upgrading a Node (zero-downtime)

Nodes are immutable. Upgrading means building a new one, waiting for VM
migration, then retiring the old one.

```bash
boxcutter-host upgrade --to v0.4.0
# Or: upgrade all nodes to latest tag
boxcutter-host upgrade --latest
```

The host daemon performs the following steps:

```
1. git fetch && git checkout v0.4.0 (in the configured repo)
2. Build new node image from that ref (go build + cloud-init ISO)
3. Launch new node VM with next available bridge IP
4. Wait for new node to pass health checks
5. New node registers with orchestrator automatically
6. Host daemon tells orchestrator to drain old node:
   - POST to orchestrator API: set old node status to "draining"
   - Orchestrator migrates each VM to available nodes
   - Migration uses existing export/import + rsync flow
7. Host daemon monitors drain progress (polls orchestrator health endpoint)
8. Once drained, host daemon stops old node VM
9. Old node's disk is archived or deleted
10. Updates cluster.json with new node state

Timeline:
  [0s]   Build new node image (~30s for go build + ISO)
  [30s]  Launch new node VM
  [90s]  New node is ready (cloud-init + registration)
  [90s+] Drain old node (depends on VM count, COW sizes)
         Each VM migration: stop + rsync COW + start on new node
  [done] Old node retired
```

Note: the host daemon communicates with the orchestrator during drain only to
request it to migrate workloads — this is an outbound call from host to
orchestrator via the bridge network. The orchestrator has no API to call back to
the host daemon. The host daemon initiates, the orchestrator executes within its
own domain (workload scheduling).

Active VMs experience a brief interruption during migration (stop → transfer →
start), but the system as a whole stays available — other nodes continue serving.

### Upgrading the Orchestrator (brief downtime)

The host daemon also upgrades the orchestrator. Existing VMs keep running during
the upgrade — they don't depend on the orchestrator at runtime.

**Strategy: rebuild state from nodes**

Everything in the orchestrator DB can be reconstructed by querying the nodes:

| Orchestrator DB table | Reconstructible? | How? |
|---|---|---|
| `nodes` | Yes | Nodes re-register on boot via `POST /api/nodes/register` |
| `vms` | Yes | Each node's `GET /api/vms` returns all VM state (name, mark, mode, vcpu, ram, disk, tailscale_ip, status) |
| `golden_images` | Partially | Active golden image hash can be read from nodes; historical versions are lost (acceptable) |
| `ssh_keys` | **No** | SSH keys are only stored in the orchestrator DB |

SSH keys are the one piece of state that can't be reconstructed from nodes.
Solution: the orchestrator's QCOW2 disk persists across upgrades — the host
daemon replaces the cloud-init ISO (new binary + config) but reuses the same
disk. The SQLite DB on disk survives the upgrade.

```bash
boxcutter-host upgrade-orchestrator --to v0.4.0
```

**Upgrade flow:**

```
1. Host daemon builds new orchestrator binary from target git ref
2. Host daemon packages new cloud-init ISO (new binary, same secrets)
3. Host daemon stops old orchestrator VM
4. Host daemon launches orchestrator VM with same QCOW2 disk + new ISO
5. New orchestrator boots:
   - SQLite DB is intact on disk (SSH keys preserved)
   - Waits for nodes to re-register (they heartbeat every 30s)
   - Reconciles VM state by querying each node's /api/vms
6. System fully operational again

Downtime: ~60-90 seconds (VM stop + VM boot + cloud-init)
```

For major version changes that require a DB migration, the new orchestrator
binary handles it on startup (standard SQLite migration pattern).

### Upgrading the Golden Image

Golden image upgrades are orchestrator-managed (the host daemon is not
involved — this is a workload concern, not infrastructure):

```
1. Orchestrator triggers golden image rebuild on a node
2. New golden image is built (debootstrap + provision)
3. Orchestrator records new version in DB, distributes to all nodes
4. New VMs use the new golden image
5. Existing VMs are unaffected (they have their own COW snapshots
   backed by whatever golden version they were created from)
```

No migration needed. Old VMs continue working with their old golden base. When
they're destroyed and new ones created, the new golden image is used.

## Data Directionality Analysis

There are three distinct trust/control domains:

```
Host daemon (bare metal)     ← operator controls via CLI
  │
  │ builds + launches (provisioning time only)
  ▼
Orchestrator VM              ← manages workloads
  │
  │ schedules + coordinates (runtime)
  ▼
Node VMs → Firecracker VMs   ← execute workloads
```

### Host daemon → VMs (provisioning-time, downward)

- **Identity assignment**: host daemon assigns bridge IPs, MACs, hostnames
- **Build artifacts**: host daemon compiles Go, packages cloud-init ISOs
- **Secrets injection**: host daemon bakes Tailscale keys, SSH keys, CA certs
  into cloud-init at build time
- **Capacity decisions**: host daemon allocates vCPU/RAM per VM

### Orchestrator → Nodes (runtime, downward)

- **VM creation**: orchestrator picks node, sends create request with name/config
- **SSH keys**: orchestrator stores keys, passes them to nodes during VM creation
- **Golden image**: orchestrator triggers build, distributes to nodes
- **Migration**: orchestrator coordinates, tells source to export, target to import
- **Draining**: orchestrator moves VMs away when told a node is going away

### Upward flow (reporting only)

1. **Node registration**: Node reads its config (assigned by host daemon at
   build time) and reports to the orchestrator. The node doesn't decide its
   identity — it reads what the host daemon assigned.

2. **Mark allocation**: Marks are node-local (each node has its own fwmark
   space). The node allocates them deterministically (CRC32 of VM name) and
   reports to the orchestrator. This is reporting, not decision-making.

3. **Tailscale IP**: Assigned by Tailscale, reported upward. Inherently external.

### Host daemon ← Orchestrator (drain requests only)

During node upgrades, the host daemon makes a single API call to the
orchestrator to initiate a drain. This is the only cross-domain communication at
runtime, and it flows from the host daemon into the environment, never the
reverse.

### Bootstrap data for a new node

Minimal data a node needs to boot and function (all assigned by host daemon):

```yaml
# Assigned by host daemon, baked into cloud-init ISO
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

# Points at the orchestrator
orchestrator:
  url: "http://192.168.50.2:8801"
```

Everything else (golden image, VM definitions) comes from the orchestrator after
the node registers.

## Boot Sequence (After Install)

```
Host reboot
  │
  ├─ systemd starts boxcutter-host.service
  │
  ├─ boxcutter-host reads /var/lib/boxcutter/cluster.json:
  │    - recreates bridge + NAT (idempotent)
  │    - launches orchestrator VM (knows its disk + ISO path)
  │    - launches all node VMs (in parallel)
  │    - health-checks each VM
  │
  ├─ Orchestrator VM boots
  │    - systemd starts boxcutter-orchestrator
  │    - orchestrator reads its SQLite DB
  │    - waits for nodes to register
  │
  ├─ Node VMs boot
  │    - each node's systemd starts: boxcutter-net → vmid → boxcutter-node
  │    - node agent registers with orchestrator
  │    - orchestrator tells node to start any dev VMs that were previously running
  │
  └─ System operational

Note: the host daemon launches ALL VMs (orchestrator + nodes). The orchestrator
does not participate in launching nodes — it discovers them when they register.
```

## Orchestrator State Reconstruction

When the host daemon upgrades the orchestrator (or the DB is lost), it can
rebuild state:

```
1. Orchestrator starts with empty DB
2. Import SSH keys from export file (or operator re-adds them)
3. Wait for nodes to register (they heartbeat every 30s, or on boot)
4. For each registered node:
   a. GET /api/vms → list of all VMs on that node
   b. For each VM: record in orchestrator DB
      (name, node_id, mark, mode, vcpu, ram_mib, disk, tailscale_ip, status)
5. State fully reconstructed

Everything the orchestrator needs is already available via existing node APIs.
```

This means the orchestrator DB is effectively a **cache** of the cluster state,
not the source of truth for VM data. The source of truth for "what VMs exist" is
the nodes themselves. The orchestrator is the source of truth for:
- **SSH keys** (must be persisted/exported)
- **Node assignments** (which IPs/names map to which nodes — but this is in the
  provisioning config, not just the DB)
- **Scheduling decisions** (but these are stateless — just pick the node with
  most free RAM)

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

The host daemon detects what changed between versions and only upgrades what's
necessary:

```bash
git diff v0.3.0..v0.4.0 --name-only
# If orchestrator/ changed → orchestrator upgrade (stop, rebuild ISO, relaunch)
# If node/ changed → rolling node upgrade (build new, drain old, retire)
# If host/ changed → host daemon self-update (rebuild binary, systemctl restart)
# If golden/ changed → tell operator to trigger golden rebuild via orchestrator
```

The host daemon handles infrastructure upgrades (orchestrator, nodes, itself).
Golden image rebuilds remain the orchestrator's responsibility since they're a
workload concern — the host daemon logs a notice if `golden/` changed and the
operator triggers the rebuild via SSH to the orchestrator.

## Responsibility Matrix

| Concern | Owner | Why |
|---|---|---|
| Bridge, NAT, TAP devices | Host daemon | Infrastructure — bare metal networking |
| Building VM images (go build + ISO) | Host daemon | Requires git checkout + Go toolchain on host |
| Launching/stopping QEMU VMs | Host daemon | Infrastructure — process management on host |
| Health-checking VMs, restart on crash | Host daemon | Infrastructure — availability |
| Rolling node upgrades | Host daemon | Infrastructure — build new, drain, retire |
| Orchestrator upgrades | Host daemon | Infrastructure — rebuild ISO, relaunch |
| Host capacity (vCPU/RAM budgets) | Host daemon | Infrastructure — physical resource limits |
| Scheduling dev VMs onto nodes | Orchestrator | Workload — pick node with most free capacity |
| SSH key management | Orchestrator | Workload — user access control |
| Golden image builds | Orchestrator | Workload — dev environment tooling |
| VM migration during drain | Orchestrator | Workload — rsync COW + restart on new node |
| Firecracker VM lifecycle | Node agent | Workload — create/destroy/snapshot |

## Summary

| Component | Install | Upgrade | State |
|---|---|---|---|
| **Host daemon** | `make install-host` (Go binary + systemd) | Self-update via `boxcutter-host upgrade` | `/etc/boxcutter/host.yaml` + `/var/lib/boxcutter/cluster.json` |
| **Orchestrator** | Built by host daemon during bootstrap | Host daemon rebuilds ISO, relaunches (same disk) | SQLite DB on QCOW2 disk (survives upgrades) |
| **Node VMs** | Built by host daemon (`node add` or bootstrap) | Host daemon: build new → drain old → retire | VM state on disk (owned by VMs, not node) |
| **Golden image** | Built by orchestrator on a node | Rebuild + distribute (existing VMs unaffected) | ext4 file on each node |
| **Firecracker VMs** | Created by orchestrator via node agent | N/A (ephemeral, destroy and recreate) | COW snapshot (per-VM, immutable base) |
