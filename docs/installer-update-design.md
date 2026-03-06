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

1. **Orchestrator is the brain** — it owns all cluster state, assigns identities,
   builds images, and boots everything
2. **Nodes are immutable cattle** — upgrade by building a new node VM and draining
   the old one, never by patching in place
3. **Orchestrator state is reconstructible** — everything in the orchestrator DB
   can be rebuilt by querying live nodes
4. **Build from source every time** — no binary distribution; git tags provide
   versioning
5. **Data flows downward** — orchestrator → node → VM; nodes never tell the
   orchestrator what to think, they only report status

## Architecture Overview

```
┌─────────────────────────────────────────────────────────┐
│  Physical Host                                          │
│                                                         │
│  boxcutter-host (small daemon)                          │
│    - manages bridge, TAP devices, NAT                   │
│    - launches/stops QEMU VMs                            │
│    - takes orders from orchestrator via bridge network   │
│                                                         │
│  ┌─────────────────────┐  ┌──────────────────────────┐  │
│  │ Orchestrator VM     │  │ Node VM (immutable)      │  │
│  │ - SQLite DB         │  │ - Firecracker microVMs   │  │
│  │ - builds node ISOs  │  │ - vmid, proxy, agent     │  │
│  │ - builds golden img │  │ - reports to orchestrator │  │
│  │ - assigns IDs/IPs   │  │                          │  │
│  │ - manages upgrades  │  │ Node VM 2... N           │  │
│  └─────────────────────┘  └──────────────────────────┘  │
└─────────────────────────────────────────────────────────┘
```

## Installation Flow

### Phase 0: Bootstrap the host

The human installs a small `boxcutter-host` daemon on the physical machine. This
is the only manual step and the only thing that runs directly on bare metal.

```bash
curl -sL https://raw.githubusercontent.com/.../install-host.sh | bash
# or: git clone ... && cd boxcutter && make install-host
```

`boxcutter-host` is a small Go binary (or shell script) that:
- Creates/manages the bridge device and NAT rules
- Creates/destroys TAP devices on demand
- Launches/stops QEMU VMs (given a disk image + cloud-init ISO)
- Exposes a local API on the bridge IP (192.168.50.1:8800) — only reachable
  from VMs on the bridge, not from the internet
- Persists a minimal config: `~/.boxcutter/host.yaml` with the NIC name, bridge
  subnet, and path to the git checkout

The host daemon does NOT need to understand boxcutter's internals. It's a
generic "run QEMU VMs on a bridge" service. The orchestrator tells it what to
launch.

### Phase 1: Bootstrap the orchestrator

On first install, the host daemon needs to be told "build and launch the
orchestrator." This is the one bootstrapping chicken-and-egg moment:

```bash
boxcutter-host bootstrap \
  --repo ~/boxcutter \
  --secrets ~/.boxcutter/secrets/ \
  --tailscale-authkey tskey-auth-XXXXX
```

This command:
1. Runs `go build` for the orchestrator binary (from the local git checkout)
2. Packages it with cloud-init, secrets, and config into an ISO
3. Creates a QCOW2 disk backed by the Ubuntu cloud image
4. Launches the orchestrator VM
5. Waits for it to come up (health check on 192.168.50.2:8801/healthz)

After this, the orchestrator is running and takes over all further management.

### Phase 2: Orchestrator provisions nodes

The orchestrator builds and launches node VMs by calling back to the host daemon:

```
Orchestrator                          Host Daemon
    │                                      │
    │  POST /api/vms/build                 │
    │  {type: "node", name: "node-1",      │
    │   git_ref: "v0.3.0",                 │
    │   secrets: {...}, config: {...}}      │
    │─────────────────────────────────────→ │
    │                                      │ go build ...
    │                                      │ package cloud-init ISO
    │                                      │ qemu-img create ...
    │  201 {disk: "...", iso: "..."}       │
    │ ←────────────────────────────────────│
    │                                      │
    │  POST /api/vms/launch                │
    │  {name: "node-1", disk: "...",       │
    │   vcpu: 12, ram: "48G"}              │
    │─────────────────────────────────────→ │
    │                                      │ qemu-system-x86_64 ...
    │  201 {pid: 12345}                    │
    │ ←────────────────────────────────────│
```

The orchestrator owns the decision of:
- **What git ref to build** (tag, branch, or commit)
- **What IP/MAC to assign** (derived from node number, same as today)
- **What secrets to inject** (Tailscale keys, SSH keys, CA certs)
- **What config values to template** (bridge IP, orchestrator URL, hostname)

The host daemon is a dumb executor — it builds what it's told and launches what
it's told.

### Phase 3: Orchestrator builds the golden image

Once at least one node is up, the orchestrator triggers the golden image build
on a node (same two-phase process as today, but orchestrator-initiated):

```
Orchestrator → Node Agent: POST /api/golden/build
Orchestrator → Node Agent: POST /api/golden/provision
```

The golden image version is tracked in the orchestrator DB (`golden_images`
table, already exists). The orchestrator distributes the golden image to all
nodes (rsync over bridge network).

### Phase 4: System is operational

The orchestrator now boots everything on system startup:
1. Host daemon starts on boot (systemd)
2. Host daemon launches the orchestrator VM (it knows which disk/ISO to use)
3. Orchestrator comes up, launches all node VMs via the host daemon API
4. Node agents register with the orchestrator
5. Orchestrator tells nodes to start any VMs that were running before shutdown

## Versioning: Git Tags

Every release is a git tag: `v0.1.0`, `v0.2.0`, etc.

```bash
git tag -a v0.3.0 -m "Add paranoid mode sentinel tokens"
git push origin v0.3.0
```

The orchestrator knows:
- **Its own version** — baked in at build time via `-ldflags "-X main.Version=v0.3.0"`
- **What version each node is running** — reported during registration
- **What git ref to use for the next build** — configurable, defaults to
  latest tag

```go
// Injected at build time
var Version = "dev"

// Node registration includes version
type RegisterRequest struct {
    ID      string `json:"id"`
    Version string `json:"version"` // e.g. "v0.3.0"
    // ...
}
```

The orchestrator exposes version info:

```
GET /api/version
{
  "orchestrator": "v0.3.0",
  "nodes": {
    "node-1": "v0.3.0",
    "node-2": "v0.2.0"
  },
  "golden_image": "v0.3.0-golden-1",
  "available": "v0.4.0"  // latest tag in the repo
}
```

## Upgrade Process

### Upgrading a Node (zero-downtime)

Nodes are immutable. Upgrading a node means building a new one and draining the
old one. This is exactly how you'd handle it in Kubernetes — nodes are cattle.

```
1. Orchestrator pulls latest git ref (or specified tag)
2. Orchestrator calls host daemon: build new node VM image from that ref
3. Orchestrator calls host daemon: launch new node VM (node-3)
4. New node boots, registers with orchestrator
5. Orchestrator drains old node (node-1):
   - Sets node status to "draining"
   - Migrates each VM to the new node (or other nodes)
   - Migration uses existing export/import + rsync flow
6. Once drained, orchestrator tells host daemon to stop old node VM
7. Old node's disk is archived or deleted

Timeline:
  [0s]   Build new node image (~30s for go build + ISO)
  [30s]  Launch new node VM
  [90s]  New node is ready (cloud-init + registration)
  [90s+] Drain old node (depends on VM count, COW sizes)
         Each VM migration: stop + rsync COW + start on new node
  [done] Old node retired
```

Active VMs experience a brief interruption during migration (stop → transfer →
start), but the system as a whole stays available — other nodes continue serving.

### Upgrading the Orchestrator (brief downtime)

The orchestrator is a single point of coordination, so upgrading it requires a
brief window where no new VMs can be created and no migrations can happen. But
existing VMs keep running — they don't depend on the orchestrator at runtime.

**Strategy: rebuild state from nodes**

The key insight from the codebase: **everything in the orchestrator DB can be
reconstructed by querying the nodes.** Let's verify:

| Orchestrator DB table | Reconstructible? | How? |
|---|---|---|
| `nodes` | Yes | Nodes re-register on boot via `POST /api/nodes/register` |
| `vms` | Yes | Each node's `GET /api/vms` returns all VM state (name, mark, mode, vcpu, ram, disk, tailscale_ip, status) |
| `golden_images` | Partially | Active golden image hash can be read from nodes; historical versions are lost (acceptable) |
| `ssh_keys` | **No** | SSH keys are only stored in the orchestrator DB |

SSH keys are the one piece of state that can't be reconstructed from nodes.
Solution: **the orchestrator persists ssh_keys to a file that survives
upgrades.** This is a small JSON file (`/var/lib/boxcutter/ssh-keys.json`)
written alongside the DB, or we simply keep the SQLite file and only rebuild the
volatile tables.

**Upgrade flow:**

```
1. Orchestrator decides to upgrade itself (triggered via API or SSH command)
2. Orchestrator exports durable state:
   - SSH keys → /var/lib/boxcutter/ssh-keys-export.json
   - Current config → /var/lib/boxcutter/config-export.yaml
   (Written to the QCOW2 disk, which persists across VM restarts)
3. Orchestrator tells host daemon:
   - Build new orchestrator image from target git ref
   - The new ISO includes the exported state as bootstrap data
4. Host daemon stops old orchestrator VM
5. Host daemon launches new orchestrator VM (same disk, new ISO)
   OR: new disk that imports the exported state
6. New orchestrator boots:
   - Imports SSH keys from export file
   - Waits for nodes to re-register (they heartbeat every 30s)
   - Reconciles VM state by querying each node's /api/vms
7. System fully operational again

Downtime: ~60-90 seconds (VM stop + VM boot + cloud-init)
```

**Alternative: in-place binary swap** (even less downtime)

Since the orchestrator VM is just Ubuntu + our binary + systemd:

```
1. Orchestrator builds new binary from target git ref
   (go build on the orchestrator VM itself, or request host daemon to build)
2. Orchestrator copies new binary to /usr/local/bin/boxcutter-orchestrator-new
3. Orchestrator writes a systemd override to use the new binary
4. Orchestrator runs: systemctl restart boxcutter-orchestrator

Downtime: ~2-3 seconds (process restart)
```

This is simpler but violates the "immutable" principle. It's a reasonable
pragmatic choice for the orchestrator specifically, since it's a single
long-lived component. Use the full VM rebuild for major version changes, binary
swap for minor updates.

### Upgrading the Golden Image

The golden image upgrade is the simplest case — it's already designed for this:

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

Current data flow has some directionality concerns worth addressing:

### Good (downward flow, orchestrator → node → VM)

- **VM creation**: orchestrator picks node, sends create request with name/config
- **SSH keys**: orchestrator stores keys, passes them to nodes during VM creation
- **Golden image**: orchestrator triggers build, distributes to nodes
- **Migration**: orchestrator coordinates, tells source to export, target to import
- **Draining**: orchestrator initiates, migrates VMs away

### Needs fixing (upward flow, currently node-initiated)

1. **Node self-registration**: Today, the node agent reads its own config file
   and registers with the orchestrator. This means the node decides its own ID,
   bridge IP, etc.

   **Fix**: The orchestrator should assign all of this. The node should boot with
   minimal bootstrap data (just "where is the orchestrator") and receive its
   identity from the orchestrator.

   ```
   Current:
     Node reads boxcutter.yaml → node knows its bridge_ip, hostname
     Node calls POST /api/nodes/register with self-assigned ID

   Proposed:
     Node boots with only: orchestrator_url + bootstrap_token
     Node calls POST /api/nodes/bootstrap {token: "xxx"}
     Orchestrator responds: {id: "node-1", bridge_ip: "192.168.50.3", ...}
     Node configures itself from the response
   ```

   But wait — the bridge IP must be set *before* the node can reach the
   orchestrator, because the bridge IP is the node's network identity on the
   bridge. So the orchestrator must assign the IP *before boot*, during
   provisioning.

   **Revised**: The orchestrator assigns the IP/MAC/hostname during the
   `host-daemon build` step (it already does this — see `provision.sh` line 110
   where it templates `BRIDGE_IP_PLACEHOLDER`). The node just needs to confirm
   its identity on first registration. This is actually already correct. The
   orchestrator templates the config, the node reads it, and registers. The data
   flows down at provisioning time.

2. **Mark allocation**: Today, marks are allocated by the node agent using CRC32
   of the VM name. This is deterministic and collision-resistant but means the
   node decides marks independently.

   This is actually fine — marks are node-local (each node has its own fwmark
   space). The orchestrator doesn't need to allocate them. The node reports the
   assigned mark back, and the orchestrator records it. This is reporting, not
   decision-making.

3. **Tailscale IP assignment**: Assigned by Tailscale, reported upward. No fix
   needed — this is inherently external.

### Bootstrap data for a new node

Minimal data a node needs to boot and function:

```yaml
# Assigned by orchestrator, baked into cloud-init ISO
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
  ├─ systemd starts boxcutter-host daemon
  │
  ├─ boxcutter-host reads its config:
  │    - recreates bridge + NAT (idempotent)
  │    - launches orchestrator VM (knows its disk + ISO path)
  │
  ├─ Orchestrator VM boots
  │    - systemd starts boxcutter-orchestrator
  │    - orchestrator reads its DB, finds registered nodes
  │    - orchestrator calls host daemon: launch each node VM
  │
  ├─ Node VMs boot
  │    - each node's systemd starts: boxcutter-net → vmid → boxcutter-node
  │    - node agent registers with orchestrator
  │    - orchestrator tells node to start any VMs that were previously running
  │
  └─ System operational
```

## Orchestrator State Reconstruction

When the orchestrator upgrades (or its DB is lost), it can rebuild state:

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

# Operator triggers upgrade (from any tailnet device)
ssh boxcutter upgrade --to v0.4.0
# or: auto-upgrade to latest tag
ssh boxcutter upgrade --latest

# Orchestrator handles it:
# 1. git fetch && git checkout v0.4.0
# 2. Build new node images
# 3. Rolling upgrade of nodes (build → launch → drain old → retire)
# 4. Optionally upgrade itself (if orchestrator code changed)
# 5. Optionally rebuild golden image (if golden/ changed)
```

The orchestrator can detect what changed between versions:

```bash
git diff v0.3.0..v0.4.0 --name-only
# If orchestrator/ changed → orchestrator upgrade needed
# If node/ changed → node upgrade needed
# If golden/ changed → golden image rebuild needed
```

## Summary

| Component | Install | Upgrade | State |
|---|---|---|---|
| **Host daemon** | Manual (`make install-host`) | Manual (rare, just a small binary) | Minimal config file |
| **Orchestrator** | Built by host daemon bootstrap | Binary swap (minor) or VM rebuild (major) | SQLite DB (reconstructible except SSH keys) |
| **Node VMs** | Built by orchestrator via host daemon | Build new → drain old → retire (immutable) | VM state on disk (owned by VMs, not node) |
| **Golden image** | Built by orchestrator on a node | Rebuild + distribute (existing VMs unaffected) | ext4 file on each node |
| **Firecracker VMs** | Created by orchestrator via node agent | N/A (ephemeral, destroy and recreate) | COW snapshot (per-VM, immutable base) |
