# Immutable Node Design

## Current Architecture Summary

Today, boxcutter has a three-layer architecture:

```
Physical Host → Node VM (QEMU/KVM) → Firecracker microVMs
```

**Node VM** is a long-lived QEMU VM that:
- Runs Firecracker and manages VM lifecycle via `boxcutter-ctl`
- Hosts the internal bridge (`brvm0`, 10.0.1.0/24) connecting all Firecracker VMs
- Joins Tailscale as `boxcutter` (the control plane hostname)
- Exposes SSH control interface (`boxcutter-ssh`) for `ssh boxcutter new/list/destroy`
- Runs `vmid` — a Go service providing JWT tokens, GitHub App tokens, and VM identity via the 169.254.169.254 metadata IP
- Holds all state: golden image, VM COW snapshots, vm.json files, network config, SSH keys
- Has a fixed IP pool: 10.0.1.200-250 (51 VMs max)

**Firecracker VMs** are ephemeral dev environments that:
- Get an internal IP from the pool (10.0.1.x) via kernel boot parameter
- Join Tailscale with their name as hostname (e.g., `bold-fox`)
- Are reachable externally only via Tailscale IP
- Use device-mapper COW snapshots off the golden image

**There's also a nascent `boxcutter-gateway`** script that already has:
- A `/etc/boxcutter/hosts` file listing multiple nodes (name, IP, port)
- `best_host()` — picks the node with the most RAM headroom for `new`
- Aggregated `list` and `status` across all nodes
- Pass-through `shell`/`destroy`/`logs` to specific nodes

This gateway script is the seed of the orchestrator, but it's statically configured and SSH-based.

---

## Goal

Make nodes **immutable and replaceable**: when a new version of the node software is needed (new golden image, updated kernel, new boxcutter-ctl, etc.), we spin up a new node, migrate VMs from the old node to the new one, then discard the old node.

This requires a **service router / orchestrator** that sits above nodes and:
1. Is the stable entry point (gets the `boxcutter` Tailscale hostname)
2. Knows about all nodes, their capacity, and their VMs
3. Routes `new` / `list` / `destroy` / `shell` commands appropriately
4. Can tell a node to migrate a VM to another node
5. Can create new nodes or handle capacity exhaustion
6. Manages node lifecycle (register, drain, retire)

---

## Proposed Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  Orchestrator (boxcutter)                                    │
│  Tailscale hostname: boxcutter                               │
│  Runs: orchestrator service, SSH control interface            │
│  Small VM or process on the physical host                    │
│                                                              │
│  Knows: all nodes, all VMs, capacity, routes commands        │
│  State: node registry, VM→node mapping                       │
├──────────────────────────────────────────────────────────────┤
│         │              │              │                       │
│    ┌────▼────┐    ┌────▼────┐    ┌────▼────┐                 │
│    │ Node-1  │    │ Node-2  │    │ Node-3  │  ...            │
│    │bc-a1b2c3│    │bc-d4e5f6│    │bc-g7h8i9│                 │
│    │QEMU/KVM │    │QEMU/KVM │    │QEMU/KVM │                 │
│    │TS: bc-  │    │TS: bc-  │    │TS: bc-  │                 │
│    │  a1b2c3 │    │  d4e5f6 │    │  g7h8i9 │                 │
│    │         │    │         │    │         │                  │
│    │ [VMs]   │    │ [VMs]   │    │ [VMs]   │                 │
│    └─────────┘    └─────────┘    └─────────┘                 │
└──────────────────────────────────────────────────────────────┘
```

### Components

#### 1. Orchestrator

The stable control plane. Gets the `boxcutter` Tailscale hostname. This is what users SSH to.

**Options for where it runs:**
- (a) Directly on the physical host as a lightweight process
- (b) In its own small QEMU VM (separate from node VMs)
- (c) In a container on the physical host

**Recommendation: (a) on the physical host.** The orchestrator is tiny — it's a router, not a hypervisor. Running it on the host avoids nesting complexity and gives it direct access to create/destroy node VMs. The host already has Tailscale (or could), and the orchestrator's state is minimal.

**Responsibilities:**
- SSH control interface (`ssh boxcutter new/list/destroy/status`)
- Node registry: tracks all nodes, their IDs, capacity, health
- VM registry: tracks which VM is on which node
- Command routing: `new` → pick best node, `list` → aggregate, `destroy` → route to correct node
- Migration orchestration: tell source node to export, tell target node to import
- Node lifecycle: create new nodes, drain old nodes, retire nodes
- Capacity management: when all nodes are full, spin up a new one (or reject)

**State storage:**
- Simple JSON or SQLite file on the physical host
- Persists node list, VM→node mappings, node versions, capacity
- This is the only stateful component that must survive across node replacements

#### 2. Nodes (Immutable)

Each node is a QEMU/KVM VM running Firecracker, just like today's single Node VM, but:
- **Named `boxcutter-<id>`** on Tailscale (e.g., `boxcutter-a1b2c3`)
- **No longer the SSH control plane** — the orchestrator handles that
- **Registers with the orchestrator** on boot
- **Has a fixed capacity declaration** (e.g., "I can run 6 VMs at 8GB each")
- **Exposes an API** for the orchestrator to call (create VM, destroy VM, export VM, import VM, health check)
- **Immutable**: no in-place upgrades. To update, spin up a new node and migrate VMs off the old one

**Node identity:**
- Each node gets a random short ID at creation time (e.g., `a1b2c3`)
- Tailscale hostname: `boxcutter-a1b2c3` (or `bc-a1b2c3` for brevity)
- Internal naming: consistent — `bc-<id>`

#### 3. VM Networking (Fixed IP Model)

**Current problem:** Each VM gets an internal IP (10.0.1.x) that is node-local and a Tailscale IP that is globally unique. During migration, the internal IP is meaningless (different bridge), and the Tailscale IP would change if the VM re-registers.

**Proposed: VMs always have the same internal IP.**

Every VM gets the same internal IP address (e.g., `10.0.1.100`) within its node. Since:
- VMs are isolated on the bridge (can't talk to each other)
- The only consumer of the internal IP is the node itself (SSH from node to VM, metadata service)
- External access is via Tailscale only

There's no collision risk — each VM has its own TAP device, and the node routes to them by MAC address / TAP device, not by IP.

**Wait — this doesn't quite work with a shared bridge.** If all VMs have the same IP on the same bridge, the bridge can't route. Options:

**(a) Per-VM network namespace on the node:**
Each VM gets its own network namespace with its own bridge instance. The node can still reach each VM via the namespace. This is clean but adds complexity.

**(b) Unique internal IPs but they're irrelevant externally:**
Keep the current model where each VM gets a unique internal IP from the pool, but treat these as ephemeral node-local details. The orchestrator only cares about Tailscale IPs. Migration re-assigns a new internal IP on the target node.

**(c) Per-VM veth pair with NAT (no bridge):**
Each VM gets a point-to-point link to the node. All VMs can have IP 10.0.1.2 on their side, with the node having 10.0.1.1 on each veth. Separate routing tables per veth. The node uses source NAT / policy routing.

**Recommendation: (b) for now, consider (c) later.** The internal IP is a node-local implementation detail. The orchestrator tracks VMs by name and Tailscale IP. During migration, the VM gets a new internal IP on the new node and re-registers with Tailscale (the Tailscale IP changes — see below). The simplicity of keeping the current bridge model outweighs the elegance of fixed IPs, because...

**Tailscale IP and migration:** When a VM migrates, it will get a new Tailscale IP (it's a new Tailscale node registration). With ephemeral keys, the old registration auto-expires. Users already access VMs by MagicDNS name (`ssh bold-fox`), which Tailscale updates automatically when the node re-registers with the same hostname. So migration is transparent to users who use names rather than IPs.

**Alternative: Tailscale node identity preservation.** If we want the Tailscale IP to survive migration, we'd need to transfer the Tailscale node key (stored in `/var/lib/tailscale/`) from the old VM to the new one. This is feasible — it's part of the VM's rootfs which we'd be migrating anyway. If we migrate the COW snapshot, the Tailscale state comes with it and the VM would rejoin with the same identity/IP.

**Recommendation: Preserve Tailscale identity during migration** by migrating the full VM state (COW image + Tailscale state). This means the Tailscale IP, hostname, and node identity are preserved. Users see zero disruption.

---

## Design Details

### Node Registration Protocol

When a new node boots:

1. Node starts, runs its setup, starts its API server
2. Node calls the orchestrator's registration endpoint:
   ```
   POST /internal/nodes/register
   {
     "node_id": "a1b2c3",
     "tailscale_hostname": "bc-a1b2c3",
     "capacity": {
       "max_vms": 6,
       "ram_total_mib": 49152,
       "vcpu_total": 12,
       "disk_bytes": 150000000000
     },
     "api_endpoint": "http://bc-a1b2c3:8080",
     "version": "2025-03-06-abc123"
   }
   ```
3. Orchestrator adds node to registry, marks it `active`
4. Orchestrator may immediately begin scheduling VMs to this node

**How does the node find the orchestrator?**
- Option A: Hard-coded Tailscale hostname (`boxcutter`)
- Option B: Passed as a kernel/cloud-init parameter at node creation time
- Option C: The orchestrator creates nodes, so it injects its own address

Recommendation: **(C)** — the orchestrator creates node VMs and passes its own address as a parameter. The node calls home on boot.

### Node Capacity Model

Each node declares fixed capacity at registration:
- `max_vms`: hard limit on concurrent VMs (derived from RAM / default VM size)
- `ram_total_mib`: total RAM available for VMs
- `vcpu_total`: total vCPUs
- `disk_bytes`: available disk

The orchestrator tracks current usage per node:
- `allocated_vms`: count of running + stopped VMs
- `allocated_ram_mib`: sum of VM RAM allocations
- `allocated_vcpu`: sum of VM vCPU allocations

Scheduling decision for `new`:
1. Filter nodes that have enough free capacity for the requested VM size
2. Among eligible nodes, pick the one with the most headroom (bin-packing or spread)
3. If no node has capacity, either create a new node or reject

### VM Migration

Migration flow (orchestrator-driven):

1. **Orchestrator decides to migrate** VM `bold-fox` from `node-1` to `node-2`
   - Triggered by: node drain (for retirement), rebalancing, or node failure
2. **Orchestrator tells node-1: pause and export**
   ```
   POST node-1/api/vms/bold-fox/export
   ```
   - node-1 stops the Firecracker process (graceful shutdown)
   - node-1 streams the COW image to a shared location or directly to node-2
3. **Orchestrator tells node-2: import and start**
   ```
   POST node-2/api/vms/bold-fox/import
   { cow_source: "node-1:/var/lib/boxcutter/vms/bold-fox/cow.img" }
   ```
   - node-2 pulls the COW image
   - node-2 creates a new VM with the same name, using the imported COW
   - node-2 starts the VM — Tailscale rejoins with the same identity (preserved in COW)
4. **Orchestrator updates its registry**: `bold-fox` now lives on `node-2`
5. **Orchestrator tells node-1: cleanup**
   ```
   DELETE node-1/api/vms/bold-fox
   ```

**COW image transfer mechanism:**
- Option A: rsync/scp between nodes over Tailscale (simple, works)
- Option B: Shared NFS/9p mount from the physical host (faster for same-host nodes)
- Option C: Stream over HTTP between node APIs

Recommendation: **(A) rsync over Tailscale** for simplicity. The COW images are sparse and typically small (only changed blocks from golden). rsync with `--sparse` handles this well.

**Golden image synchronization:**
All nodes must have the same golden image (since COW snapshots reference it). Options:
- Bake the golden image into the node VM disk at creation time
- Share it via 9p from the physical host (current approach, works for all nodes on same host)
- Distribute via HTTP/rsync when nodes span multiple physical hosts

For single-host deployments: the 9p mount already shares the repo; we'd extend it to share the golden image.

### Node Drain and Retirement

To retire a node (e.g., for upgrades):

1. **Mark node as `draining`** — no new VMs scheduled here
2. **For each VM on the node:**
   - Find a target node with capacity (or create one)
   - Migrate VM to target
3. **Once empty, mark node as `retired`**
4. **Shut down and destroy the node VM**

### Orchestrator SSH Interface

The orchestrator replaces the current `boxcutter-ssh` as the user-facing control plane:

```
ssh boxcutter new [--clone repo] [--vcpu N] [--ram MiB]
ssh boxcutter list
ssh boxcutter destroy <name>
ssh boxcutter status
ssh boxcutter nodes                    # list all nodes
ssh boxcutter drain <node-id>          # drain a node for retirement
ssh boxcutter upgrade                  # create new node, drain old, retire old
```

The `list` command aggregates across all nodes. `destroy` routes to the correct node. `new` picks the best node.

### Orchestrator API

Internal HTTP API (for node communication):

```
POST   /internal/nodes/register        — node registration
DELETE /internal/nodes/{id}             — node deregistration
GET    /internal/nodes                  — list nodes
GET    /internal/nodes/{id}/health      — node health check

POST   /internal/vms                    — record new VM
DELETE /internal/vms/{name}             — record VM deletion
GET    /internal/vms                    — list all VMs
GET    /internal/vms/{name}             — get VM info (including which node)

POST   /internal/migrate               — initiate migration
       { vm: "bold-fox", from: "node-1", to: "node-2" }
```

### Node API

Each node exposes an API (replaces current SSH-based control):

```
POST   /api/vms                         — create VM
DELETE /api/vms/{name}                   — destroy VM
GET    /api/vms                          — list VMs
GET    /api/vms/{name}                   — VM details
POST   /api/vms/{name}/start            — start VM
POST   /api/vms/{name}/stop             — stop VM
POST   /api/vms/{name}/export           — stop + prepare for migration
POST   /api/vms/{name}/import           — receive + start migrated VM
GET    /api/health                       — node health + capacity
```

---

## Open Questions

### Q1: Single physical host or multi-host?

The current design assumes one physical host with multiple QEMU node VMs. Should we design for multi-host from the start?

**If single-host only:**
- Orchestrator runs on the host, nodes are QEMU VMs
- Shared storage via 9p or host filesystem is trivial
- Migration is fast (same disk, just copy COW file)
- Creating a new node = launching a new QEMU VM

**If multi-host:**
- Orchestrator needs to be reachable from all hosts (Tailscale solves this)
- Golden image distribution across hosts
- Migration requires network transfer of COW images
- Node creation requires provisioning on remote hosts
- More complex but more scalable

**Recommendation:** Design the abstractions for multi-host but implement single-host first. The orchestrator API is the same either way; only the "create node" and "transfer COW" implementations differ.

### Q2: Where does vmid live?

Currently vmid runs on the node and provides metadata/tokens to VMs. Options:

**(a) Keep vmid per-node:** Each node runs its own vmid instance. Simplest, matches current design. The metadata IP (169.254.169.254) is node-local anyway.

**(b) Move vmid to the orchestrator:** Centralize identity. Would need to route metadata requests from VMs through their node to the orchestrator.

**Recommendation: (a) Keep per-node.** The metadata IP and iptables redirect are inherently node-local. The JWT signing key can be shared across nodes (distributed at node creation time).

### Q3: What triggers a node upgrade?

Options:
- Manual: operator runs `ssh boxcutter upgrade`
- Automatic: orchestrator detects version drift and auto-upgrades
- Hybrid: orchestrator notifies, operator approves

**Recommendation:** Start manual, add automation later. The `upgrade` command would:
1. Build/prepare a new node VM image with the latest software
2. Launch the new node
3. Wait for it to register
4. Drain the old node (migrate all VMs)
5. Retire the old node

### Q4: How do we handle the golden image during node replacement?

If the golden image changes (new dev tools, OS updates), all nodes need the same version for COW snapshots to work.

**Options:**
- (a) Golden image is baked into the node VM disk — each node version includes a specific golden image version
- (b) Golden image lives on the host and is shared via 9p to all nodes
- (c) Golden image is distributed via the orchestrator

**(b) is simplest for single-host.** All nodes share the same golden image via the 9p mount. When the golden image is rebuilt, existing VMs (whose COW snapshots reference the old golden) must be migrated or destroyed before the old golden can be removed.

**Actually, this is a key constraint:** COW snapshots are tied to a specific golden image. If we update the golden, we need a way to handle VMs that reference the old one. Options:
- Keep old golden images around until all VMs referencing them are gone
- Treat golden image updates as a "new generation" — new VMs get the new golden, old VMs keep running on old golden
- On node upgrade, migrate VMs to fresh VMs (re-create, not COW-migrate) — lose state but get latest image

**Recommendation:** Support multiple golden image generations. The orchestrator tracks which golden version each VM uses. Old golden images are retained until no VMs reference them. This is cleanest for the immutable model.

### Q5: Orchestrator high availability?

For a single physical host, the orchestrator is a single point of failure. Acceptable?

**Recommendation:** Yes, for now. The physical host is already a SPOF. If it goes down, all nodes and VMs go down anyway. The orchestrator's state file should be backed up, but HA is overkill for v1.

### Q6: How does the orchestrator create new nodes?

The orchestrator needs to launch QEMU VMs. Options:
- Shell out to the existing `host/launch.sh` (adapted for multiple nodes)
- Use a libvirt/QEMU API
- Use a simple script that the orchestrator calls

**Recommendation:** Adapt `host/launch.sh` into a parameterized `create-node.sh` that takes a node ID and resource allocation. The orchestrator calls this script. Each node gets its own QCOW2 disk (COW on a base node image), its own TAP device, and its own cloud-init ISO with the node ID and orchestrator address baked in.

### Q7: How do we handle the "out of capacity" problem?

When all nodes are full:
- (a) Create a new node automatically (if host has capacity)
- (b) Reject the request with an error
- (c) Queue the request and create a node

**Recommendation:** (a) with a host-level capacity check. The orchestrator knows the host's total resources and what's allocated to nodes. If there's room for another node, create one. If not, reject with a clear error ("no capacity — N VMs running across M nodes, host has X GB RAM free").

### Q8: What about the proxy command / shell access?

Currently `ssh boxcutter shell <name>` uses `nc` to proxy to the VM's internal IP. With multiple nodes, the orchestrator doesn't have direct network access to VMs on other nodes' bridges.

**Options:**
- (a) Orchestrator SSH-bounces through the node: `ssh boxcutter proxy bold-fox` → orchestrator SSHs to the correct node → node proxies to the VM
- (b) Users SSH directly to VMs via Tailscale (already the primary path)
- (c) Orchestrator uses Tailscale to reach the VM directly

**Recommendation: (b) is already the primary UX.** `ssh bold-fox` via Tailscale is the standard way. The `shell`/`proxy` commands are convenience shortcuts. For multi-node, implement (a) as a hop-through-node proxy.

---

## Implementation Phases

### Phase 1: Orchestrator Foundation
- Create orchestrator service (Go, runs on physical host or in small VM)
- Node registry + health checking
- VM registry (VM→node mapping)
- SSH control interface on the orchestrator (replaces boxcutter-ssh for user-facing commands)
- Orchestrator gets the `boxcutter` Tailscale hostname

### Phase 2: Node API
- Add HTTP API to nodes (create/destroy/list/health)
- Node registration with orchestrator on boot
- Capacity reporting
- Orchestrator routes `new`/`list`/`destroy` through node APIs

### Phase 3: Multi-Node
- Parameterized node creation (orchestrator can launch new node VMs)
- Each node gets `boxcutter-<id>` Tailscale hostname
- Capacity-aware scheduling in the orchestrator
- Auto-create nodes when capacity is exhausted

### Phase 4: Migration
- VM export/import API on nodes
- COW image transfer between nodes
- Tailscale identity preservation
- `drain` command on orchestrator
- `upgrade` command: new node → drain old → retire old

### Phase 5: Multi-Generation Golden Images
- Track golden image versions
- Support VMs from different golden generations on the same node
- Garbage-collect old golden images when no VMs reference them

---

## Summary of Key Decisions

| Decision | Recommendation |
|----------|---------------|
| Where does orchestrator run? | Physical host (lightweight process) |
| Node Tailscale naming | `boxcutter-<short-id>` (e.g., `boxcutter-a1b2c3`) |
| Orchestrator Tailscale naming | `boxcutter` (the stable name users know) |
| VM internal IPs | Keep current model (unique per-node, ephemeral) |
| Migration IP preservation | Migrate COW image (includes Tailscale state) → same TS IP |
| Node-to-orchestrator communication | HTTP API over Tailscale |
| Orchestrator-to-node communication | HTTP API over Tailscale |
| vmid location | Per-node (keep current model) |
| Capacity management | Fixed declaration at node registration + orchestrator tracking |
| Out-of-capacity handling | Auto-create new node if host has resources |
| Golden image sharing | 9p from host (single-host), distribute for multi-host |
| State persistence | JSON/SQLite on physical host |
| Node creation | Adapted launch.sh, called by orchestrator |
