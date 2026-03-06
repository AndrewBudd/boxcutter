# Multi-Host Architecture Design

## Core Design Principles

1. **All VMs have the same IP.** Every Firecracker VM is 10.0.0.2 with gateway 10.0.0.1, inside its own network namespace. No IP management anywhere. VM images are perfectly portable between nodes.
2. **Multi-host from day one.** The orchestrator, nodes, and storage are designed to span physical hosts.
3. **VM state preservation matters.** Migration means moving a running VM's disk state to another node with zero data loss. Tailscale identity (and thus IP) survives migration.
4. **Nodes are immutable and replaceable.** To upgrade, spin up a new node, migrate VMs off the old, destroy the old.
5. **Tailscale is the trusted network.** All inter-component communication (orchestrator ↔ nodes) happens over Tailscale. Policies ensure nodes can talk to each other but VMs cannot reach nodes (except the metadata IP on their own node).
6. **Go for all new services.** Orchestrator, node agent, and vmid are Go.

---

## Architecture Overview

```
                    Tailscale network
                          │
              ┌───────────┼───────────┐
              │           │           │
         Orchestrator   Node A      Node B
         (boxcutter)    (bc-xxxx)   (bc-yyyy)
              │           │           │
              │      ┌────┴────┐  ┌───┴────┐
              │      │ VM  VM  │  │ VM  VM │
              │      └─────────┘  └────────┘
              │
         TrueNAS (optional)
         NFS: golden images
         iSCSI: VM block devices
```

### Components

| Component | Language | Runs on | Tailscale name |
|-----------|----------|---------|----------------|
| Orchestrator | Go | Any host (or small VM) | `boxcutter` |
| Node agent | Go | Each QEMU node VM | `bc-<id>` |
| vmid | Go | Each node (current code, per-node) | — (internal only) |
| TrueNAS | — | Dedicated NAS box or VM | `truenas` (or similar) |

---

## Networking: Same IP, Network Namespaces

### The Problem

If VMs have unique IPs, migration requires IP reassignment. The VM image contains network state. IP pools must be managed per-node. This is unnecessary complexity.

### The Solution

Every VM gets the exact same network configuration:
- VM IP: `10.0.0.2/30`
- Gateway: `10.0.0.1`
- DNS: `8.8.8.8`

This works because each VM lives inside its own **Linux network namespace** on the node. The namespace provides complete isolation — each one has its own routing table, its own TAP device, its own iptables rules. There's no shared bridge.

### Per-VM Network Setup

```
Node host network
│
├── netns: vm-bold-fox
│   ├── tap0 (10.0.0.1/30)  ←── Firecracker connects here
│   │     └── VM sees: eth0 = 10.0.0.2/30, gw 10.0.0.1
│   ├── veth-bf-i (10.0.0.5/30) ←→ veth-bf-o (10.0.0.6/30) in host
│   ├── iptables MASQUERADE on veth-bf-i for outbound internet
│   └── DNAT 169.254.169.254 → vmid on host
│
├── netns: vm-calm-bear
│   ├── tap0 (10.0.0.1/30)
│   ├── veth-cb-i ←→ veth-cb-o in host
│   ├── iptables MASQUERADE
│   └── DNAT 169.254.169.254 → vmid
│
└── host network
    ├── veth-bf-o (10.0.0.6/30) → routed to internet
    ├── veth-cb-o (10.0.0.10/30) → routed to internet
    └── MASQUERADE to uplink
```

### Setup Steps (per VM)

```bash
# 1. Create network namespace
ip netns add vm-${NAME}

# 2. Create TAP device inside namespace (for Firecracker)
ip netns exec vm-${NAME} ip tuntap add tap0 mode tap
ip netns exec vm-${NAME} ip addr add 10.0.0.1/30 dev tap0
ip netns exec vm-${NAME} ip link set tap0 up

# 3. Create veth pair for internet access
ip link add veth-${SHORT}-i type veth peer name veth-${SHORT}-o
ip link set veth-${SHORT}-i netns vm-${NAME}

# 4. Configure veth inside namespace
ip netns exec vm-${NAME} ip addr add 10.0.0.5/30 dev veth-${SHORT}-i
ip netns exec vm-${NAME} ip link set veth-${SHORT}-i up
ip netns exec vm-${NAME} ip route add default via 10.0.0.6

# 5. Configure veth on host side
ip addr add 10.0.0.6/30 dev veth-${SHORT}-o
ip link set veth-${SHORT}-o up

# 6. NAT inside namespace (VM → internet)
ip netns exec vm-${NAME} iptables -t nat -A POSTROUTING -o veth-${SHORT}-i -j MASQUERADE

# 7. NAT on host (namespace → real internet)
iptables -t nat -A POSTROUTING -s 10.0.0.5/30 -o ${UPLINK} -j MASQUERADE

# 8. Metadata service redirect (inside namespace)
ip netns exec vm-${NAME} ip addr add 169.254.169.254/32 dev lo
ip netns exec vm-${NAME} iptables -t nat -A PREROUTING -d 169.254.169.254 -p tcp --dport 80 \
    -j DNAT --to-destination ${VMID_IP}:8775

# 9. Firecracker runs inside the namespace
ip netns exec vm-${NAME} firecracker --config-file fc-config.json ...
```

### Kernel Boot Args

Every VM gets the identical `ip=` parameter:

```
ip=10.0.0.2::10.0.0.1:255.255.255.252:${HOSTNAME}:eth0:off:8.8.8.8
```

The only per-VM variation is the hostname, which is injected by the node agent at boot time and doesn't affect the network configuration.

### Why Not a Bridge?

The current design uses a shared bridge (`brvm0`) with unique IPs. That model requires:
- IP pool management per node
- ebtables/iptables for VM isolation
- IP reassignment on migration

With network namespaces:
- No IP management at all
- Isolation is structural (namespaces are kernel-enforced)
- Every VM image is identical — perfectly portable
- Metadata service routing is per-VM (cleaner than shared bridge DNAT)

### Veth Address Allocation

Each VM needs a unique veth address pair on the host side. Use a simple formula:
- VM index N (0-based, assigned at creation time on this node)
- Inside namespace: `10.0.0.5/30` (always the same)
- Host side: `10.0.0.{4*N + 6}/30` for the host veth

Or simpler: use `10.0.N.1/30` and `10.0.N.2/30` for the veth pair, where N is the VM's slot index on this node. The slot is ephemeral and node-local — it's just for routing, not identity. On migration, the new node assigns a new slot.

---

## Storage

### Option A: Local Storage (Simple, Fast)

```
Node disk
├── /var/lib/boxcutter/golden/rootfs.ext4  (read-only base)
├── /var/lib/boxcutter/vms/bold-fox/
│   ├── cow.img (device-mapper COW snapshot)
│   ├── vm.json
│   └── fc-config.json
```

- Golden image baked into node or distributed via rsync
- COW on local NVMe (fast I/O)
- Migration = rsync COW over Tailscale to target node
- Golden image must be identical on all nodes

**Pros:** Fast I/O, simple, works today.
**Cons:** Migration requires full COW transfer over network. Golden image distribution is manual.

### Option B: TrueNAS Shared Storage

```
TrueNAS
├── NFS export: /golden/
│   └── rootfs.ext4 (read-only, all nodes mount this)
├── iSCSI targets (one zvol per VM):
│   ├── boxcutter/bold-fox  (ZFS clone of golden snapshot)
│   └── boxcutter/calm-bear
```

**Golden image via NFS:**
- All nodes mount `truenas:/golden` read-only
- Single source of truth — rebuild once, all nodes see it
- Network read penalty on first access, but page cache helps
- Device-mapper COW still works (base is NFS-mounted file)

**VM storage via iSCSI zvols:**
- TrueNAS creates a ZFS snapshot of the golden zvol
- Each VM gets a ZFS clone (instant, COW at the ZFS layer)
- Node connects to iSCSI target, presents block device to Firecracker
- Migration = disconnect iSCSI on node A, connect on node B (instant, no data copy)
- ZFS handles COW natively — no device-mapper needed

**Pros:** Migration is instant (no data copy). Golden image management is centralized. ZFS snapshots/clones are fast and space-efficient.
**Cons:** All I/O goes over network. Latency depends on network speed (10GbE is fine, 1GbE will be noticeable for heavy I/O).

### Option C: Hybrid (Recommended)

```
TrueNAS
├── NFS: /golden/rootfs.ext4  (golden image, read-only)

Node (local NVMe)
├── /var/lib/boxcutter/golden/rootfs.ext4  (cached copy from NFS)
├── /var/lib/boxcutter/vms/bold-fox/cow.img  (local COW, fast I/O)
```

- Golden image lives on TrueNAS NFS — single source of truth
- Nodes cache golden image locally on first boot (or rsync on provisioning)
- COW layer is local for fast I/O
- Migration: rsync COW from source node to TrueNAS staging area, then to target node (or direct node-to-node over Tailscale)
- TrueNAS is a convenient transfer point but not in the hot path

**Alternatively:** TrueNAS iSCSI for VMs that need instant migration (stateful, long-lived VMs), local storage for ephemeral VMs that can be recreated.

### Golden Image Distribution

With TrueNAS:
1. Build golden image on any node (or a build VM)
2. Push to TrueNAS NFS share
3. All nodes see it immediately (or pull on next boot)

Without TrueNAS:
1. Build golden image on one node
2. rsync to all other nodes over Tailscale
3. Orchestrator tracks golden image version per node

### Device-Mapper vs ZFS Clones

| Feature | Device-mapper (current) | ZFS clones (TrueNAS) |
|---------|------------------------|---------------------|
| COW mechanism | dm-snapshot | ZFS clone |
| Create speed | ~250ms | ~50ms |
| Migration | Copy COW file | Reconnect iSCSI target |
| Cleanup | dmsetup + losetup | zfs destroy |
| Network dependency | None (local) | Yes (iSCSI) |
| Snapshot of snapshot | Complex | Native (ZFS) |

For TrueNAS-backed VMs, we'd skip device-mapper entirely and let ZFS handle COW. The Firecracker `path_on_host` points to the iSCSI block device directly.

---

## Orchestrator

### Responsibilities

- **SSH control interface**: Users `ssh boxcutter new/list/destroy/status`
- **Node registry**: Track all nodes, their IDs, capacity, health, version
- **VM registry**: Track all VMs — name, which node, Tailscale IP, resource allocation, golden image version
- **Scheduling**: Place new VMs on nodes with capacity
- **Migration**: Orchestrate VM moves between nodes
- **Node lifecycle**: Create, drain, retire nodes
- **Golden image management**: Track versions, trigger distribution

### State Storage

SQLite on the orchestrator's filesystem. This is the single source of truth:

```sql
CREATE TABLE nodes (
    id TEXT PRIMARY KEY,           -- "a1b2c3"
    tailscale_name TEXT,           -- "bc-a1b2c3"
    tailscale_ip TEXT,             -- "100.x.x.x"
    status TEXT,                   -- "active", "draining", "retired"
    version TEXT,                  -- node software version
    ram_total_mib INTEGER,
    vcpu_total INTEGER,
    disk_bytes INTEGER,
    registered_at TEXT,
    last_heartbeat TEXT
);

CREATE TABLE vms (
    name TEXT PRIMARY KEY,         -- "bold-fox"
    node_id TEXT REFERENCES nodes(id),
    tailscale_ip TEXT,
    vcpu INTEGER,
    ram_mib INTEGER,
    disk TEXT,
    golden_version TEXT,           -- which golden image this VM uses
    clone_url TEXT,
    github_repo TEXT,
    status TEXT,                   -- "running", "stopped", "migrating"
    created_at TEXT
);

CREATE TABLE golden_images (
    version TEXT PRIMARY KEY,      -- "2026-03-06-abc123"
    path TEXT,                     -- NFS path or distribution info
    created_at TEXT,
    active BOOLEAN                 -- is this the default for new VMs?
);
```

### API

HTTP over Tailscale. Nodes authenticate by Tailscale identity (the orchestrator verifies the source Tailscale IP matches a registered node).

```
# Node-facing API
POST   /api/nodes/register         Node calls this on boot
POST   /api/nodes/{id}/heartbeat   Periodic health check
DELETE /api/nodes/{id}             Node deregistration

# VM lifecycle (called by orchestrator internally, or by nodes reporting status)
POST   /api/vms                    Record new VM
PUT    /api/vms/{name}             Update VM state
DELETE /api/vms/{name}             Record VM deletion
GET    /api/vms                    List all VMs
GET    /api/vms/{name}             Get VM details (including which node)

# Migration (orchestrator-internal, triggers calls to nodes)
POST   /api/migrate                { vm: "bold-fox", to: "node-id" }

# Node management
POST   /api/nodes/{id}/drain       Start draining a node
GET    /api/nodes                  List all nodes with capacity
```

### SSH Interface

The orchestrator gets the `boxcutter` Tailscale hostname and runs sshd with ForceCommand:

```
ssh boxcutter new [--clone repo] [--vcpu N] [--ram MiB]
ssh boxcutter list
ssh boxcutter destroy <name>
ssh boxcutter status
ssh boxcutter shell <name>          # Proxy through node to VM
ssh boxcutter nodes                 # List all nodes
ssh boxcutter drain <node-id>       # Drain node for retirement
ssh boxcutter migrate <name> [--to <node-id>]
```

`new` flow:
1. Generate name (boxcutter-names)
2. Pick best node (most headroom, or specific node if requested)
3. Call node agent API: `POST /api/vms` with name, resources, clone_url
4. Node creates VM, starts it, joins Tailscale
5. Node reports back: Tailscale IP, status
6. Orchestrator records VM, returns info to user

`shell` flow:
1. Look up VM → find node
2. SSH proxy: orchestrator → node → VM (double hop over Tailscale)
3. Or: use `nc` to proxy TCP directly to VM's Tailscale IP

`destroy` flow:
1. Look up VM → find node
2. Call node agent: `DELETE /api/vms/{name}`
3. Node stops VM, cleans up storage
4. Orchestrator removes VM record

---

## Node Agent

### Responsibilities

- **VM lifecycle**: Create, start, stop, destroy Firecracker VMs
- **Network namespace management**: Set up per-VM namespaces
- **Storage management**: Device-mapper snapshots (local) or iSCSI connect (TrueNAS)
- **Tailscale provisioning**: Join VMs to Tailscale on start
- **vmid**: Run metadata service for VMs (existing code)
- **Health reporting**: Heartbeat to orchestrator
- **Migration support**: Export/import VM state

### API

HTTP server, listening on Tailscale interface:

```
POST   /api/vms                     Create + start VM
DELETE /api/vms/{name}              Stop + destroy VM
GET    /api/vms                     List VMs on this node
GET    /api/vms/{name}              VM details
POST   /api/vms/{name}/stop        Stop VM (keep state)
POST   /api/vms/{name}/start       Start stopped VM
POST   /api/vms/{name}/export      Stop + prepare COW for transfer
POST   /api/vms/{name}/import      Receive COW + start VM
GET    /api/health                  Node health + capacity
```

### Boot Sequence

1. Node VM boots (QEMU, cloud-init)
2. Node agent starts (systemd)
3. Sets up base networking (IP forwarding, NAT to uplink)
4. Joins Tailscale as `bc-<id>`
5. Starts vmid (metadata service)
6. Registers with orchestrator: `POST orchestrator/api/nodes/register`
7. Begins accepting API calls

### VM Create Flow (on Node)

```
1. Receive POST /api/vms { name: "bold-fox", vcpu: 4, ram_mib: 8192, ... }

2. Create network namespace
   ip netns add vm-bold-fox
   [set up tap0, veth pair, NAT, metadata DNAT — see networking section]

3. Create storage
   Option A (local):
     truncate -s 50G cow.img
     losetup + dmsetup create bc-bold-fox (COW on golden)
   Option B (TrueNAS):
     API call to TrueNAS: clone golden zvol → bold-fox zvol
     iscsiadm: connect to new target

4. Inject SSH keys into rootfs
   mount block device → copy authorized_keys → umount

5. Write fc-config.json
   kernel boot args: ip=10.0.0.2::10.0.0.1:255.255.255.252:bold-fox:eth0:off:8.8.8.8
   drive: /dev/mapper/bc-bold-fox (local) or /dev/sdX (iSCSI)
   network: tap0 in namespace

6. Launch Firecracker inside namespace
   ip netns exec vm-bold-fox firecracker --config-file fc-config.json

7. Wait for SSH ready (poll 10.0.0.2 from inside namespace)
   ip netns exec vm-bold-fox ssh dev@10.0.0.2 echo ready

8. Join Tailscale
   ip netns exec vm-bold-fox ssh dev@10.0.0.2 \
     "sudo tailscale up --authkey=... --hostname=bold-fox"

9. Get Tailscale IP, register with vmid

10. Report back to orchestrator: { tailscale_ip: "100.x.x.x", status: "running" }
```

---

## Migration

### Flow

```
Orchestrator                     Node A (source)              Node B (target)
     │                               │                            │
     ├── POST /vms/bold-fox/export ──>│                            │
     │                               ├── Stop Firecracker         │
     │                               ├── Flush COW to disk        │
     │                               ├── rsync cow.img ──────────>│ (or via TrueNAS)
     │<── { export_path, size } ─────┤                            │
     │                               │                            │
     ├── POST /vms/bold-fox/import ──────────────────────────────>│
     │                               │                            ├── Set up netns
     │                               │                            ├── Set up storage (with received COW)
     │                               │                            ├── Start Firecracker
     │                               │                            ├── Tailscale rejoins (identity in COW)
     │<──────────────────────────────────── { tailscale_ip } ─────┤
     │                               │                            │
     ├── Update VM record ───────────────────────────────────────>│
     ├── DELETE /vms/bold-fox ───────>│                            │
     │                               ├── Cleanup netns + storage  │
     │                               │                            │
```

### Tailscale Identity Preservation

The Tailscale node state lives in `/var/lib/tailscale/` on the VM's rootfs. Since we migrate the COW image (which contains the full rootfs delta from golden), the Tailscale identity comes with it. When the VM boots on the new node:
- Tailscale daemon finds existing state in `/var/lib/tailscale/`
- Rejoins the tailnet with the same node key
- Gets the same Tailscale IP and hostname
- Users see zero disruption

### Transfer Methods

**Direct node-to-node (default):**
```bash
# On source node, after stopping VM:
rsync --sparse -e "ssh" /var/lib/boxcutter/vms/bold-fox/cow.img \
  bc-yyyy:/var/lib/boxcutter/vms/bold-fox/cow.img
```
- Uses Tailscale for transport (encrypted, authenticated)
- `--sparse` preserves sparse files (COW images are mostly empty)
- Speed depends on actual written data, not allocated size

**Via TrueNAS staging (for large transfers or multi-hop):**
```bash
# Source pushes to TrueNAS
rsync --sparse cow.img truenas:/staging/bold-fox/cow.img
# Target pulls from TrueNAS
rsync --sparse truenas:/staging/bold-fox/cow.img /var/lib/boxcutter/vms/bold-fox/cow.img
```

**Via TrueNAS iSCSI (instant, if using TrueNAS storage):**
- Source disconnects iSCSI target
- Target connects to same iSCSI target
- No data copy at all — ZFS zvol is on TrueNAS, nodes just mount/unmount

### Migration Duration

| Method | 1GB COW | 10GB COW | 50GB COW |
|--------|---------|----------|----------|
| rsync over 1GbE | ~10s | ~90s | ~7min |
| rsync over 10GbE | ~1s | ~10s | ~45s |
| iSCSI reconnect | instant | instant | instant |

Note: COW images are sparse. A VM with 50GB allocated disk but 2GB actual writes only transfers 2GB.

---

## Node Drain & Retirement

```
1. ssh boxcutter drain <node-id>
2. Orchestrator marks node as "draining" — no new VMs scheduled
3. For each VM on node:
   a. Find target node with capacity (or create one)
   b. Migrate VM to target
   c. Wait for migration to complete
4. When empty, mark node "retired"
5. Orchestrator shuts down node VM (if on same host) or notifies admin
```

### Rolling Upgrade

```
1. ssh boxcutter upgrade
2. Orchestrator creates new node VM with latest software/golden image
3. New node boots, registers
4. Orchestrator drains old node(s) one at a time
5. Migrated VMs now run on new node(s)
6. Old nodes destroyed
```

---

## TrueNAS Integration Details

### What TrueNAS Provides

**NFS for golden images:**
- Share: `truenas:/mnt/pool/boxcutter/golden`
- Mounted read-only on all nodes at `/var/lib/boxcutter/golden`
- Contains: `rootfs.ext4` (the golden base image)
- Multiple versions supported (e.g., `rootfs-v1.ext4`, `rootfs-v2.ext4`)

**iSCSI for VM block devices (optional, for instant migration):**
- TrueNAS creates a zvol per VM: `pool/boxcutter/vms/bold-fox`
- Each zvol is a ZFS clone of the golden zvol snapshot
- Node connects via iSCSI: `iscsiadm -m discovery -t st -p truenas`
- Block device appears as `/dev/disk/by-path/...` on the node
- Firecracker uses this directly as `path_on_host`

**API for automation:**
- TrueNAS has a REST API for creating datasets, zvols, snapshots, clones, iSCSI targets
- Node agent (or orchestrator) calls TrueNAS API to provision storage
- Example: `POST /api/v2.0/pool/dataset` to create a zvol clone

### TrueNAS API Usage

```go
// Create VM storage (ZFS clone of golden)
POST truenas/api/v2.0/zfs/snapshot/clone
{
    "snapshot": "pool/boxcutter/golden@latest",
    "dataset_dst": "pool/boxcutter/vms/bold-fox"
}

// Create iSCSI target for the zvol
POST truenas/api/v2.0/iscsi/extent
{
    "name": "bold-fox",
    "type": "DISK",
    "disk": "zvol/pool/boxcutter/vms/bold-fox"
}

// Delete VM storage
DELETE truenas/api/v2.0/pool/dataset/id/pool%2Fboxcutter%2Fvms%2Fbold-fox
```

### Network Considerations

- NFS reads for golden image: happens once per VM creation (mount + read base), then page cache serves
- iSCSI I/O: every read/write goes over network
  - 10GbE: ~1GB/s throughput, ~0.1ms latency — good enough for dev VMs
  - 1GbE: ~100MB/s throughput, ~0.5ms latency — noticeable for heavy I/O
- Recommendation: 10GbE between nodes and TrueNAS, or use local storage + rsync migration

---

## vmid (Per-Node, Unchanged Architecture)

vmid continues to run per-node with its current design:
- Identity middleware: looks up VM by source IP (now always 10.0.0.2, disambiguated by network namespace routing)
- JWT tokens: audience-scoped, signed by node's private key
- GitHub tokens: policy-based, via GitHub App

### Change: IP-Based Lookup with Namespaces

With all VMs at 10.0.0.2, vmid can't identify VMs by source IP alone. Two approaches:

**Option 1: Port-based dispatch.** Each VM's metadata DNAT maps to a unique port on vmid:
```bash
# VM slot 0: 169.254.169.254:80 → vmid:8775 (with X-VM-ID header injected by iptables)
# VM slot 1: 169.254.169.254:80 → vmid:8776
```
vmid listens on multiple ports, each mapped to a VM.

**Option 2: Unique metadata IPs.** Each namespace DNATs to vmid with a unique source:
```bash
# In namespace, SNAT the metadata request to a unique IP before forwarding:
iptables -t nat -A POSTROUTING -d ${VMID_IP} -j SNAT --to-source 10.0.${SLOT}.2
```
vmid sees requests from 10.0.0.2, 10.0.1.2, etc. — can look up by source IP.

**Option 3: Unix socket per VM.** vmid exposes a Unix socket per VM. The namespace's metadata DNAT routes to a socat proxy that connects to the right socket. Over-engineered.

**Recommendation: Option 2 (unique source NAT).** The namespace iptables rules SNAT metadata requests to `10.0.{SLOT}.2` before forwarding to vmid. vmid's registry maps these source IPs to VM records. The node agent registers VMs with their SNAT IP. This requires zero changes to vmid's identity middleware — it already looks up by source IP.

---

## Implementation Plan

### Phase 1: Network Namespace Foundation

Replace the shared bridge model with per-VM network namespaces.

**Changes:**
- New Go package: `internal/netns` — create/destroy network namespaces, TAP devices, veth pairs, iptables rules
- Update `boxcutter-ctl create/start/stop/destroy` to use namespaces (or rewrite as Go)
- Firecracker launches inside `ip netns exec vm-${NAME}`
- vmid: register VMs with SNAT IP instead of bridge IP
- Remove: bridge creation, IP allocation, `allocate_ip()`, `ip_to_mac()`

**Deliverable:** Single-node boxcutter with per-VM namespaces. All VMs are 10.0.0.2. Existing functionality preserved.

### Phase 2: Node Agent

Extract VM lifecycle management from bash into a Go HTTP service.

**New binary:** `boxcutter-node` (Go)
- HTTP API for VM CRUD
- Calls Firecracker, manages namespaces, manages storage
- Reports capacity and health
- Subsumes `boxcutter-ctl` functionality

**vmid integration:** Node agent embeds or co-starts vmid.

**Deliverable:** Node is controllable via HTTP API. `boxcutter-ctl` becomes a thin CLI wrapper around the API.

### Phase 3: Orchestrator

The central brain.

**New binary:** `boxcutter-orchestrator` (Go)
- SSH control interface (replaces `boxcutter-ssh` on the gateway)
- Node registry + heartbeat
- VM registry (SQLite)
- Scheduling: pick best node for `new`
- Command routing: `list` aggregates, `destroy` routes

**Deliverable:** Users SSH to orchestrator. VMs created across nodes. Single pane of glass.

### Phase 4: Migration

**Node agent additions:**
- `POST /api/vms/{name}/export` — stop VM, prepare COW for transfer
- `POST /api/vms/{name}/import` — receive COW, start VM

**Orchestrator additions:**
- `POST /api/migrate` — orchestrate export → transfer → import
- `ssh boxcutter drain <node>` — migrate all VMs off a node
- `ssh boxcutter migrate <vm> [--to <node>]`

**Deliverable:** VMs can be moved between nodes. Tailscale identity preserved. Nodes can be drained and retired.

### Phase 5: TrueNAS Integration (Optional)

**Storage backend abstraction:**
- Interface: `CreateVMStorage(name) → blockDevice`, `DeleteVMStorage(name)`, `MigrateVMStorage(name, targetNode)`
- Local backend: device-mapper (current)
- TrueNAS backend: ZFS clones via TrueNAS REST API + iSCSI

**Golden image management:**
- NFS mount from TrueNAS for golden images
- Orchestrator manages golden image versions
- Nodes auto-pull latest golden on boot

**Deliverable:** VMs can use TrueNAS-backed storage. Migration is instant for iSCSI-backed VMs.

---

## Key Differences from Previous Design

| Aspect | Previous Design | This Design |
|--------|----------------|-------------|
| VM networking | Shared bridge, unique IPs (10.0.1.x) | Per-VM namespace, all 10.0.0.2 |
| IP management | Pool allocation per node | None — all VMs identical |
| VM isolation | ebtables FORWARD DROP | Kernel namespaces (structural) |
| Multi-host | "Design for, implement later" | Multi-host from day one |
| Orchestrator location | Physical host | Anywhere on Tailscale |
| Storage | Local only | Local + TrueNAS option |
| Migration speed | Always rsync COW | rsync or instant (iSCSI) |
| Node communication | SSH-based | HTTP API over Tailscale |
| State preservation | Secondary concern | Primary concern |

---

## Open Questions

### Q1: Orchestrator placement

The orchestrator needs to be highly available (it's the SSH entry point). Options:
- (a) Small VM on one physical host (simple, SPOF)
- (b) Container on any host (easy to restart)
- (c) Redundant pair with shared SQLite (complex)

Recommendation: (a) for now, with SQLite backed up to TrueNAS. If the orchestrator host dies, restore from backup on another host.

### Q2: TrueNAS API authentication

How does the node agent authenticate to TrueNAS? Options:
- API key (static, stored on orchestrator)
- Tailscale-based auth (TrueNAS verifies source IP)
- No auth (TrueNAS on trusted network)

Recommendation: API key distributed to nodes at creation time by orchestrator.

### Q3: Golden image builds

Where do golden image builds happen?
- On a dedicated build node
- On any node (orchestrator picks one)
- On the orchestrator itself

Recommendation: On any node. The orchestrator picks a node, tells it to build, then distributes the result to TrueNAS NFS.

### Q4: Node auto-scaling

Should the orchestrator automatically create new nodes when capacity is exhausted?
- This requires knowing the physical host's remaining resources
- Multi-host: orchestrator needs to know which physical hosts exist and their capacity

This is a multi-host provisioning problem. For v1: manual node creation, orchestrator just tracks what exists.
