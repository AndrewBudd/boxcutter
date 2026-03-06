# Multi-Host Architecture Design

## Status

This document builds on the existing single-node boxcutter implementation. The networking layer (per-TAP fwmark routing, same-IP VMs), vmid (fwmark-based VM identity), sentinel token store, forward proxy, internal TLS, device-mapper COW snapshots, and the SSH control interface are all implemented and working. This design extends the system to support multiple physical hosts, VM migration, centralized storage, and standardized configuration.

For full details on the current networking implementation, see [network-architecture.md](network-architecture.md).

---

## Core Design Principles

1. **Multi-host from day one.** The orchestrator, nodes, and storage are designed to span physical hosts.
2. **VM state preservation matters.** Migration means moving a running VM's disk state to another node with zero data loss. Tailscale identity (and thus IP) survives migration.
3. **Nodes are immutable and replaceable.** To upgrade, spin up a new node, migrate VMs off the old, destroy the old.
4. **Tailscale is the trusted network.** All inter-component communication (orchestrator ↔ nodes) happens over Tailscale. Policies ensure nodes can talk to each other but VMs cannot reach nodes (except via their own TAP gateway).
5. **Go for all new services.** Orchestrator and node agent are Go.
6. **Secrets are bootstrapped, not distributed.** All credentials come from a single, standardized config bundle provided at setup time. The orchestrator does not distribute secrets at runtime.
7. **Build on what works.** The existing per-TAP networking, vmid, COW snapshots, proxy, and SSH interface are proven. Extend, don't replace.

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
         TrueNAS
         NFS: golden images
         NFS: migration staging
```

### Components

| Component | Language | Runs on | Tailscale name | Status |
|-----------|----------|---------|----------------|--------|
| Orchestrator | Go | Any host (or small VM) | `boxcutter` | **Planned** |
| Node agent | Go | Each QEMU node VM | `bc-<id>` | **Planned** (replaces bash scripts) |
| vmid | Go | Each node (per-node) | — (port 80, internal) | **Implemented** |
| boxcutter-proxy | Go | Each node (per-node) | — (port 8080, internal) | **Implemented** |
| boxcutter-ctl | Bash | Each node | — | **Implemented** (to be subsumed by node agent) |
| boxcutter-net | Bash | Each node | — | **Implemented** |
| TrueNAS | — | Dedicated NAS box or VM | `truenas` (or similar) | **Planned** |

---

## Current Networking Architecture (Implemented)

The networking layer is implemented and working. Every VM gets the same IP address (10.0.0.2) on an isolated point-to-point TAP link. Linux fwmark-based policy routing handles return traffic. There is no shared bridge.

```
┌──────────────────────────────────────────────────────────────┐
│  Physical Host                                                │
│                                                              │
│  tap-node0 (192.168.50.1/30) ──→ Node VM                    │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Node VM (QEMU/KVM) — 192.168.50.2                    │  │
│  │  Tailscale: 100.x.x.x                                │  │
│  │                                                        │  │
│  │  Per-VM isolated TAP links (no shared bridge):         │  │
│  │    tap-bold-fox   10.0.0.1 ↔ 10.0.0.2  mark: 41022   │  │
│  │    tap-calm-otter 10.0.0.1 ↔ 10.0.0.2  mark: 8193    │  │
│  │                                                        │  │
│  │  Services:                                             │  │
│  │    vmid           :80   (fwmark-based VM identity)     │  │
│  │    boxcutter-proxy :8080 (MITM proxy, sentinel swap)   │  │
│  │    derper          :443  (Tailscale DERP relay)         │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

### How It Works

- **Per-TAP point-to-point links**: Each VM gets its own TAP with `10.0.0.1 peer 10.0.0.2`. No bridge, no IP pool, no MAC conflicts
- **fwmark policy routing**: Each VM gets a unique mark (CRC32 of name, range 1–65535). iptables mangle marks packets per-TAP; CONNMARK saves/restores marks in conntrack so return traffic routes to the correct TAP
- **VM isolation**: No shared L2 domain — each VM only sees its point-to-point link to 10.0.0.1. VMs cannot reach each other
- **Internet access**: NAT masquerade for 10.0.0.2/32 on the uplink interface
- **Tailscale overlay**: Each VM joins Tailscale at boot (ephemeral key, never stored on disk), getting a stable 100.x.x.x IP
- **Instant networking**: Kernel `ip=10.0.0.2::10.0.0.1:255.255.255.252:hostname:eth0:off:8.8.8.8`
- **SSH to VMs**: `socat - TCP:10.0.0.2:22,so-bindtodevice=tap-<name>` (SO_BINDTODEVICE binds to specific TAP)
- **Normal/paranoid modes**: Normal VMs have direct internet; paranoid VMs route through the MITM proxy with sentinel token swapping

### What This Architecture Already Provides

| Concern | How it's handled |
|---------|-----------------|
| VM isolation | Per-TAP point-to-point (no shared L2, no routes between TAPs) |
| Internet access | NAT masquerade + fwmark policy routing |
| External reachability | Tailscale overlay (100.x.x.x) |
| VM identity | vmid reads fwmark via `getsockopt(SO_MARK)` on accepted TCP connections |
| Name resolution | Tailscale MagicDNS (ssh bold-fox) |
| Fast boot | Kernel ip= parameter, no DHCP |
| Auth key security | Key stays on Node VM, provisioned via SSH over TAP, never on VM disk |
| Credential isolation | Paranoid mode: sentinel tokens + MITM proxy, real creds never touch VM |
| Internal TLS | EC P-256 CA + leaf cert, CA injected per-VM at create time |

### Networking Implications for Multi-Host

The per-TAP same-IP model makes multi-host migration **simpler** than a bridge model:

1. **No IP reassignment needed.** Every VM is 10.0.0.2 everywhere. The destination node creates a new TAP with the same point-to-point addressing.
2. **Mark can be preserved or regenerated.** The destination node can reuse the same mark (if no collision with local VMs) or allocate a new one — the mark is internal infrastructure, not part of the VM's identity.
3. **The Tailscale IP** (100.x.x.x) is the VM's stable identity and survives migration (state lives in `/var/lib/tailscale/` on the VM's rootfs).
4. **vmid registration** just needs the new mark — `POST /internal/vms` with the VM's name and assigned mark.

No networking changes are required to support multi-host. Each node runs its own TAP infrastructure independently.

---

## vmid (Implemented)

vmid is the VM Identity & Token Broker service, implemented in Go and running per-node on port 80.

### What It Does

- **Identity**: Identifies VMs by fwmark read via `getsockopt(SO_MARK)` on accepted TCP connections (requires `net.ipv4.tcp_fwmark_accept=1`)
- **JWT tokens**: Mints audience-scoped ES256 tokens signed with a per-deployment ECDSA key
- **GitHub tokens**: Policy-based GitHub App installation tokens for repo access
- **Sentinel tokens**: In-memory one-time-use token store for paranoid mode credential wrapping
- **JWKS endpoint**: Public key endpoint at `/.well-known/jwks.json` for external token verification

### Interfaces

**VM-facing API** (VMs reach vmid at `http://10.0.0.1/` — their TAP gateway):
- `GET /` — Metadata root (VM ID, available endpoints)
- `GET /identity` — VM record (vm_id, mark, mode)
- `GET /token?audience=<aud>` — Mint JWT token
- `GET /token/github` — Mint GitHub App token (sentinel-wrapped in paranoid mode)
- `GET /.well-known/jwks.json` — Public JWKS (no auth required)

**Admin API** (Unix socket at `/run/vmid/admin.sock`, node-local only):
- `POST /internal/vms` — Register VM (id, mark, mode, github_repo)
- `DELETE /internal/vms/{id}` — Deregister VM (purges sentinels)
- `GET /internal/vms` — List registered VMs
- `GET /internal/sentinel/{sentinel}` — Swap sentinel for real token (used by proxy)

### Multi-Host Consideration

vmid runs per-node with no changes needed. Each node's vmid has its own registry of local VMs and its own sentinel store. For consistent JWT verification across nodes, all nodes share the same ECDSA signing key (distributed via the bootstrap bundle).

---

## Forward Proxy (Implemented)

boxcutter-proxy is a Go binary using `elazarl/goproxy`, running per-node on port 8080.

### What It Does

- **MITM HTTPS**: Uses the internal CA cert to intercept and inspect HTTPS traffic
- **Sentinel token swapping**: Scans Authorization headers, resolves sentinels via vmid admin socket, replaces with real credentials before forwarding
- **Egress allowlist**: Restricts which domains paranoid VMs can reach

### Multi-Host Consideration

Runs per-node with no changes needed. Each node's proxy resolves sentinels against the local vmid's sentinel store.

---

## Secrets & Bootstrap Configuration

### The Problem

Today, secrets are scattered across multiple locations with no standard structure:

| Secret | Current location | How it gets there |
|--------|-----------------|-------------------|
| Tailscale auth key (VMs) | `/etc/boxcutter/tailscale-authkey` | Manual placement |
| SSH keypair (node→VM) | `/var/lib/boxcutter/ssh/id_ed25519` | Auto-generated by install.sh |
| User SSH public keys | `/etc/boxcutter/authorized_keys` | Seeded from cloud-init, then manual |
| GitHub App private key | Referenced in `/etc/vmid/config.yaml` | Manual placement |
| GitHub App ID + Install ID | `/etc/vmid/config.yaml` | Manual edit |
| JWT signing key | Auto-generated by vmid | Auto-generated |
| Internal CA + leaf cert | `/etc/boxcutter/ca.{crt,key}`, `leaf.{crt,key}` | Auto-generated by boxcutter-tls |
| TrueNAS API key | nowhere yet | — |

This means spinning up a new node requires manually placing files in multiple locations, knowing which paths each component expects, and hoping nothing was forgotten. **We experienced this firsthand: a Node VM rebuild lost all manually-placed secrets (Tailscale auth key, GitHub App config) because they weren't part of any reproducible provisioning.**

### The Solution: `boxcutter.yaml` + `/etc/boxcutter/secrets/`

A single config file (`boxcutter.yaml`) defines the entire deployment. A single directory (`/etc/boxcutter/secrets/`) holds all secret files. Together, they are the **bootstrap bundle** — everything needed to provision any component from scratch.

### Bootstrap Bundle Structure

```
/etc/boxcutter/
├── boxcutter.yaml                    # Single config file — the source of truth
└── secrets/
    ├── tailscale-node-authkey        # Tailscale auth key for node VMs to join tailnet
    ├── tailscale-vm-authkey          # Tailscale auth key for resource VMs to join tailnet
    ├── node-ssh.key                  # Ed25519 private key: node → VM SSH access
    ├── node-ssh.pub                  # Corresponding public key
    ├── authorized-keys               # SSH public keys of all humans who can access the system
    ├── github-app.pem                # GitHub App RSA private key (optional)
    ├── jwt-signing.pem               # ECDSA P-256 key for vmid JWT signing (optional, auto-gen if absent)
    ├── ca.crt                        # Internal CA cert (auto-gen if absent)
    ├── ca.key                        # Internal CA key (auto-gen if absent)
    └── truenas-api-key               # TrueNAS REST API key (optional)
```

### `boxcutter.yaml`

```yaml
# boxcutter.yaml — single config file for the entire deployment

# Tailscale
tailscale:
  # Auth key for node VMs joining the tailnet (reusable, not ephemeral)
  node_authkey_path: /etc/boxcutter/secrets/tailscale-node-authkey
  # Auth key for resource VMs joining the tailnet (reusable, ephemeral)
  vm_authkey_path: /etc/boxcutter/secrets/tailscale-vm-authkey
  # Orchestrator's stable hostname on the tailnet
  orchestrator_hostname: boxcutter
  # Node hostname prefix (nodes become bc-<id>)
  node_hostname_prefix: bc

# SSH
ssh:
  # Keypair for node-to-VM SSH access (auto-generated on first boot if absent)
  private_key_path: /etc/boxcutter/secrets/node-ssh.key
  public_key_path: /etc/boxcutter/secrets/node-ssh.pub
  # Public keys of humans who can SSH to VMs and the orchestrator
  authorized_keys_path: /etc/boxcutter/secrets/authorized-keys

# GitHub App (optional — enables GitHub token minting for VMs)
github:
  enabled: false
  app_id: 0
  installation_id: 0
  private_key_path: /etc/boxcutter/secrets/github-app.pem

# JWT (vmid token signing)
jwt:
  # ECDSA P-256 key. Auto-generated if absent. Share across nodes for consistent token verification.
  key_path: /etc/boxcutter/secrets/jwt-signing.pem
  ttl: 10m

# TLS (internal CA for MITM proxy)
tls:
  # Auto-generated if absent. Share CA across nodes so VMs trust any node's proxy.
  ca_cert_path: /etc/boxcutter/secrets/ca.crt
  ca_key_path: /etc/boxcutter/secrets/ca.key

# Storage
storage:
  # Golden image source (TrueNAS NFS)
  golden_nfs_path: ""
  # golden_nfs_path: "truenas:/mnt/pool/boxcutter/golden"
  golden_local_path: /var/lib/boxcutter/golden/rootfs.ext4
  # Migration staging area on TrueNAS NFS (optional)
  staging_nfs_path: ""

# TrueNAS (optional)
truenas:
  enabled: false
  # host: truenas
  # api_key_path: /etc/boxcutter/secrets/truenas-api-key

# VM defaults
vm_defaults:
  vcpu: 4
  ram_mib: 8192
  disk: 50G
  dns: 8.8.8.8
  mode: normal    # "normal" or "paranoid"
```

### How the Bootstrap Bundle Flows

```
Operator creates bundle:
  /etc/boxcutter/boxcutter.yaml + /etc/boxcutter/secrets/*
         │
         ├──> Orchestrator reads boxcutter.yaml at startup
         │    Uses: tailscale node_authkey, ssh authorized_keys, github config
         │
         ├──> Node VM receives bundle via cloud-init (baked into ISO)
         │    The orchestrator (or host setup script) injects the bundle
         │    into the cloud-init ISO when creating a new node VM.
         │    Node agent reads boxcutter.yaml at startup.
         │
         └──> Golden image build reads bundle for SSH key + CA cert injection
              Public key from node-ssh.pub goes into golden image
              authorized-keys goes into golden image
              CA cert injected per-VM at create time (not baked into golden)
```

### Key Requirement: Two Tailscale Auth Keys

The current system uses one Tailscale auth key for VMs. The new system needs two:

| Key | Used by | Type | Why |
|-----|---------|------|-----|
| `tailscale-node-authkey` | Node VMs | Reusable, **not** ephemeral | Nodes are long-lived. We don't want them auto-removed if they temporarily disconnect (e.g., host reboot). |
| `tailscale-vm-authkey` | Resource VMs | Reusable, **ephemeral** | VMs should auto-remove from tailnet when destroyed/stopped. Keeps tailnet clean. |

Both are generated at https://login.tailscale.com/admin/settings/keys. They expire after 90 days and must be rotated.

### Secret Rotation

| Secret | Rotation | Impact of expiry |
|--------|----------|-----------------|
| Tailscale node authkey | 90 days | New nodes can't join tailnet. Existing nodes unaffected. |
| Tailscale VM authkey | 90 days | New VMs can't join tailnet. Existing VMs unaffected. |
| GitHub App key | Never expires (unless revoked) | GitHub token minting fails. VMs can't push to repos. |
| SSH keys | Manual rotation | Old keys lose access. Inject new keys via golden image rebuild + authorized-keys update. |
| JWT signing key | Never expires (regenerate to revoke) | External services can't verify old tokens. |
| Internal CA | 10-year validity | Proxy MITM fails. Regenerate + rebuild golden images. |
| TrueNAS API key | Manual rotation | Storage provisioning fails. Existing VMs unaffected. |

### What's Auto-Generated vs. Required

| Secret | Required at setup? | Auto-generated? |
|--------|-------------------|-----------------|
| Tailscale node authkey | **Yes** | No — must come from Tailscale admin |
| Tailscale VM authkey | **Yes** | No — must come from Tailscale admin |
| SSH keypair (node→VM) | No | **Yes** — generated on first boot if absent |
| Authorized keys | **Yes** (at least one human key) | No — operator provides |
| GitHub App key | No (optional) | No — comes from GitHub |
| JWT signing key | No | **Yes** — auto-generated if absent |
| Internal CA + leaf cert | No | **Yes** — auto-generated by boxcutter-tls if absent |
| TrueNAS API key | No (optional) | No — comes from TrueNAS |

**Minimum viable bootstrap:** Tailscale VM authkey + one SSH public key in authorized-keys. Everything else is optional or auto-generated.

---

## Storage: Hybrid (TrueNAS NFS + Local COW)

### Architecture

```
TrueNAS
├── NFS: /mnt/pool/boxcutter/golden/
│   ├── rootfs-v1.ext4          (golden image version 1)
│   └── rootfs-v2.ext4          (golden image version 2)
├── NFS: /mnt/pool/boxcutter/staging/
│   └── bold-fox/cow.img        (temporary, during migration)

Node (local NVMe)
├── /var/lib/boxcutter/golden/
│   └── rootfs.ext4             (cached copy from TrueNAS NFS)
├── /var/lib/boxcutter/vms/bold-fox/
│   ├── cow.img                 (device-mapper COW snapshot, local)
│   ├── vm.json                 (name, mark, mode, tap, mac, resources)
│   └── fc-config.json
```

### How It Works

**Golden image (TrueNAS NFS → local cache):**
- Canonical golden images live on TrueNAS NFS: `truenas:/mnt/pool/boxcutter/golden/`
- Each node caches the golden image locally at boot time (or on first use)
- Cache is a simple `rsync` from NFS mount to local disk
- Node agent checks golden image hash on startup; re-syncs if TrueNAS has a newer version

**VM COW layer (local NVMe) — already implemented:**
- Device-mapper snapshot on local disk (current mechanism, unchanged)
- Fast I/O — no network in the hot path
- `truncate -s 50G cow.img` → losetup → dmsetup (same as today)

**Migration (via TrueNAS staging or direct):**
- **Default:** Direct node-to-node rsync over Tailscale
  ```bash
  rsync --sparse -e "ssh" cow.img bc-yyyy:/var/lib/boxcutter/vms/bold-fox/cow.img
  ```
- **Via TrueNAS staging** (for slow inter-host links or multi-hop):
  ```bash
  # Source pushes to TrueNAS staging
  rsync --sparse cow.img /mnt/truenas-staging/bold-fox/cow.img
  # Target pulls from TrueNAS staging
  rsync --sparse /mnt/truenas-staging/bold-fox/cow.img /var/lib/boxcutter/vms/bold-fox/cow.img
  ```

### Golden Image Versioning

COW snapshots are tied to a specific golden base image. The orchestrator tracks which golden version each VM uses:

```sql
CREATE TABLE golden_images (
    version TEXT PRIMARY KEY,      -- "v3-2026-03-06-abc123"
    nfs_path TEXT,                 -- "rootfs-v3.ext4"
    sha256 TEXT,                   -- hash for integrity verification
    created_at TEXT,
    active BOOLEAN                 -- default for new VMs
);
```

### Migration — No IP Reassignment Needed

Since every VM is 10.0.0.2 on every node, migration is straightforward:

1. Stop VM on source node, tear down TAP + fwmark rules
2. Transfer COW image to destination node (rsync --sparse)
3. Destination node: allocate mark, create TAP, set up fwmark routing (same as `setup_vm_tap()`)
4. Boot Firecracker with the received COW image — kernel boot args are identical (`ip=10.0.0.2::10.0.0.1:...`)
5. Register with local vmid (new mark)
6. Tailscale rejoins automatically — identity preserved in `/var/lib/tailscale/` on the COW image

No kernel boot args need rewriting. No internal IP changes. The only thing that changes is the fwmark, which is internal infrastructure.

---

## Orchestrator

### Responsibilities

- **SSH control interface**: Users `ssh boxcutter new/list/destroy/status`
- **Node registry**: Track all nodes, their IDs, capacity, health, version
- **VM registry**: Track all VMs — name, which node, Tailscale IP, resource allocation, golden image version, mode
- **Scheduling**: Place new VMs on nodes with capacity
- **Migration**: Orchestrate VM moves between nodes
- **Node lifecycle**: Create, drain, retire nodes
- **Golden image management**: Track versions, trigger distribution
- **Bootstrap bundle distribution**: Package secrets into cloud-init ISOs for new nodes

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
    mark INTEGER,                  -- fwmark on current node
    mode TEXT,                     -- "normal" or "paranoid"
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
    version TEXT PRIMARY KEY,
    nfs_path TEXT,
    sha256 TEXT,
    created_at TEXT,
    active BOOLEAN
);
```

### API

HTTP over Tailscale. Nodes authenticate by Tailscale identity (the orchestrator verifies the source Tailscale IP matches a registered node).

```
# Node-facing API
POST   /api/nodes/register         Node calls this on boot
POST   /api/nodes/{id}/heartbeat   Periodic health check
DELETE /api/nodes/{id}             Node deregistration

# VM lifecycle
POST   /api/vms                    Record new VM
PUT    /api/vms/{name}             Update VM state
DELETE /api/vms/{name}             Record VM deletion
GET    /api/vms                    List all VMs
GET    /api/vms/{name}             Get VM details (including which node)

# Migration
POST   /api/migrate                { vm: "bold-fox", to: "node-id" }

# Node management
POST   /api/nodes/{id}/drain       Start draining a node
GET    /api/nodes                  List all nodes with capacity
```

### SSH Interface

The orchestrator gets the `boxcutter` Tailscale hostname and runs sshd with ForceCommand (same pattern as current boxcutter-ssh, extended for multi-node):

```
ssh boxcutter new [--clone repo] [--vcpu N] [--ram MiB] [--mode normal|paranoid]
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
3. Call node agent API: `POST /api/vms` with name, resources, clone_url, mode
4. Node creates VM (allocate mark, create TAP, dm-snapshot, inject CA cert + SSH keys), starts it, joins Tailscale
5. Node reports back: Tailscale IP, mark, status
6. Orchestrator records VM, returns info to user

`shell` flow:
1. Look up VM → find node
2. SSH proxy: orchestrator → node → VM (double hop over Tailscale)
3. Or: use `nc` to proxy TCP directly to VM's Tailscale IP

`destroy` flow:
1. Look up VM → find node
2. Call node agent: `DELETE /api/vms/{name}`
3. Node stops VM, tears down TAP + fwmark rules, deregisters from vmid (purges sentinels), cleans up storage
4. Orchestrator removes VM record

---

## Node Agent

### Responsibilities

Replaces the current bash scripts (`boxcutter-ctl`, `boxcutter-net`, `boxcutter-ssh`) with a single Go binary.

- **VM lifecycle**: Create, start, stop, destroy Firecracker VMs
- **TAP + fwmark management**: Per-VM TAP creation, iptables mangle rules, ip rule/route policy routing (same as current `setup_vm_tap()` / `teardown_vm_tap()`)
- **Mark allocation**: CRC32-based mark allocation with collision detection (same as current `allocate_mark()`)
- **Storage management**: Device-mapper snapshots on local disk (same as current)
- **Tailscale provisioning**: Join VMs to Tailscale on start via `vm_ssh()` (socat SO_BINDTODEVICE)
- **vmid coordination**: Register/deregister VMs with vmid via admin socket (including mark and mode)
- **Paranoid mode**: iptables FORWARD rules to block direct internet, proxy env injection
- **CA cert injection**: Mount rootfs at create time, inject CA cert, run update-ca-certificates
- **Health reporting**: Heartbeat to orchestrator
- **Migration support**: Export/import VM state
- **Golden image cache**: Sync from TrueNAS NFS on boot

### Configuration

The node agent reads `/etc/boxcutter/boxcutter.yaml` (delivered via cloud-init at node creation time). All secret paths are defined there. No hardcoded paths.

### API

HTTP server, listening on Tailscale interface:

```
POST   /api/vms                     Create + start VM
DELETE /api/vms/{name}              Stop + destroy VM
GET    /api/vms                     List VMs on this node
GET    /api/vms/{name}              VM details (mark, mode, tailscale_ip)
POST   /api/vms/{name}/stop        Stop VM (keep state)
POST   /api/vms/{name}/start       Start stopped VM
POST   /api/vms/{name}/export      Stop + prepare COW for transfer
POST   /api/vms/{name}/import      Receive COW + start VM
GET    /api/health                  Node health + capacity
```

### Boot Sequence

1. Node VM boots (QEMU, cloud-init)
2. Cloud-init unpacks `/etc/boxcutter/boxcutter.yaml` + `secrets/`
3. Node agent starts (systemd), reads `boxcutter.yaml`
4. Sets up one-time network infrastructure (fwmark CONNMARK rules, NAT, `tcp_fwmark_accept=1` — same as current boxcutter-net)
5. Generates TLS certs if absent (same as current boxcutter-tls)
6. Syncs golden image from TrueNAS NFS (if configured)
7. Joins Tailscale as `bc-<id>` using `tailscale-node-authkey`
8. Starts vmid (metadata service, port 80) and boxcutter-proxy (port 8080)
9. Registers with orchestrator: `POST orchestrator/api/nodes/register`
10. Begins accepting API calls

### VM Create Flow (on Node)

This is the same flow as current `boxcutter-ctl create` + `start`, translated to Go:

```
1. Receive POST /api/vms { name: "bold-fox", vcpu: 4, ram_mib: 8192, mode: "normal" }

2. Allocate mark (CRC32 of name % 65535 + 1, collision check)
   Same logic as current allocate_mark() in boxcutter-ctl

3. Create storage (local device-mapper)
   truncate -s 50G cow.img
   losetup + dmsetup create bc-bold-fox (COW on cached golden)

4. Inject CA cert + SSH keys into rootfs
   Mount dm device, copy ca.crt to /usr/local/share/ca-certificates/,
   chroot update-ca-certificates, copy authorized_keys

5. Write fc-config.json
   kernel boot args: ip=10.0.0.2::10.0.0.1:255.255.255.252:bold-fox:eth0:off:8.8.8.8
   drive: /dev/mapper/bc-bold-fox
   network: tap-bold-fox
   MAC: AA:FC:00:00:00:01 (fixed — no shared L2)

6. Create TAP with fwmark routing (setup_vm_tap)
   ip tuntap add dev tap-bold-fox mode tap
   ip addr add 10.0.0.1 peer 10.0.0.2 dev tap-bold-fox
   ip link set tap-bold-fox up
   iptables -t mangle -I PREROUTING 2 -i tap-bold-fox -j MARK --set-mark <mark>
   ip rule add fwmark <mark> lookup <mark> priority <10000 + mark % 20000>
   ip route add 10.0.0.2 dev tap-bold-fox table <mark>
   ip route add default via <gw> dev <uplink> table <mark>
   iptables -I FORWARD -i tap-bold-fox -j ACCEPT

7. If paranoid mode:
   iptables -I FORWARD -i tap-bold-fox -d 10.0.0.1/32 -p tcp --dport 8080 -j ACCEPT
   iptables -I FORWARD -i tap-bold-fox ! -d 10.0.0.0/24 -j DROP

8. Launch Firecracker
   firecracker --config-file fc-config.json

9. Wait for SSH ready (poll via socat SO_BINDTODEVICE)
   vm_ssh tap-bold-fox echo ready

10. Join Tailscale (using vm_authkey from boxcutter.yaml)
    vm_ssh tap-bold-fox "sudo tailscale up --authkey=<vm-authkey> --hostname=bold-fox"

11. Register with vmid (admin socket)
    POST /internal/vms { vm_id: "bold-fox", ip: "10.0.0.2", mark: <mark>, mode: "normal" }

12. If paranoid mode: inject proxy environment
    vm_ssh tap-bold-fox "write /etc/profile.d/boxcutter-proxy.sh"

13. Report back to orchestrator: { tailscale_ip: "100.x.x.x", mark: <mark>, status: "running" }
```

---

## Migration

### Flow

```
Orchestrator                     Node A (source)              Node B (target)
     │                               │                            │
     ├── POST /vms/bold-fox/export ──>│                            │
     │                               ├── Deregister from vmid     │
     │                               ├── Stop Firecracker         │
     │                               ├── Tear down TAP + fwmark   │
     │                               ├── Flush COW to disk        │
     │                               ├── rsync cow.img ──────────>│ (direct or via TrueNAS)
     │<── { export_path, size } ─────┤                            │
     │                               │                            │
     ├── POST /vms/bold-fox/import ──────────────────────────────>│
     │                               │                            ├── Allocate mark
     │                               │                            ├── Create TAP + fwmark routing
     │                               │                            ├── Set up dm-snapshot (with received COW)
     │                               │                            ├── Start Firecracker (same boot args)
     │                               │                            ├── Tailscale rejoins (identity in COW)
     │                               │                            ├── Register with local vmid (new mark)
     │<──────────────────────────────────── { tailscale_ip } ─────┤
     │                               │                            │
     ├── Update VM record ───────────────────────────────────────>│
     ├── DELETE /vms/bold-fox ───────>│                            │
     │                               ├── Cleanup storage          │
     │                               │                            │
```

### Why Same-IP Makes Migration Simpler

With the old bridge model, migration required:
- Allocating a new internal IP on the destination node's bridge
- Rewriting kernel boot args with the new IP
- The VM would see a different gateway, different netmask

With per-TAP same-IP:
- Every VM is always 10.0.0.2 with gateway 10.0.0.1 on a /30
- Kernel boot args are identical across nodes — no rewriting needed
- The only per-node state is the fwmark, which is allocated on arrival

### Tailscale Identity Preservation

The Tailscale node state lives in `/var/lib/tailscale/` on the VM's rootfs. Since we migrate the COW image (which contains the full rootfs delta from golden), the Tailscale identity comes with it. When the VM boots on the new node:
- Tailscale daemon finds existing state in `/var/lib/tailscale/`
- Rejoins the tailnet with the same node key
- Gets the same Tailscale IP and hostname
- Users see zero disruption

### Transfer Methods

**Direct node-to-node (default):**
```bash
rsync --sparse -e "ssh" /var/lib/boxcutter/vms/bold-fox/cow.img \
  bc-yyyy:/var/lib/boxcutter/vms/bold-fox/cow.img
```
- Uses Tailscale for transport (encrypted, authenticated)
- `--sparse` preserves sparse files (COW images are mostly empty)

**Via TrueNAS staging (for slow links or multi-hop):**
```bash
# Source pushes to TrueNAS staging NFS mount
rsync --sparse cow.img /mnt/truenas-staging/bold-fox/cow.img
# Target pulls from TrueNAS staging
rsync --sparse /mnt/truenas-staging/bold-fox/cow.img /var/lib/boxcutter/vms/bold-fox/cow.img
```

### Migration Duration

COW images are sparse. A VM with 50GB allocated disk but 2GB actual writes only transfers ~2GB.

| Actual data written | rsync over 1GbE | rsync over 10GbE |
|---------------------|-----------------|------------------|
| 1GB | ~10s | ~1s |
| 5GB | ~45s | ~5s |
| 20GB | ~3min | ~20s |

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
2. Orchestrator creates new node VM with latest software + bootstrap bundle
3. New node boots, syncs golden from TrueNAS, registers
4. Orchestrator drains old node(s) one at a time
5. Migrated VMs now run on new node(s)
6. Old nodes destroyed
```

---

## Implementation Plan

### Phase 1: Bootstrap Bundle + Config Standardization

Standardize all secrets and configuration into `boxcutter.yaml` + `/etc/boxcutter/secrets/`.

**Changes:**
- Define `boxcutter.yaml` schema (Go struct)
- Write `boxcutter-setup` tool that validates the bundle and provisions the local system
- Migrate `boxcutter-ctl` to read secret paths from `boxcutter.yaml` instead of hardcoded locations
- Migrate vmid to read GitHub/JWT config from `boxcutter.yaml` (currently reads `/etc/vmid/config.yaml`)
- Update `boxcutter.yaml.template` to include TLS and mode defaults
- Update cloud-init to deliver the bundle
- Include CA cert in bootstrap bundle (shared across nodes so VMs trust any node's proxy)

**Deliverable:** Single config file drives all provisioning. New deployment = fill in template + run setup.

### Phase 2: Node Agent (Go)

Extract VM lifecycle management from bash into a Go HTTP service that wraps the existing networking and storage patterns.

**New binary:** `boxcutter-node` (Go)
- HTTP API for VM CRUD
- Manages per-TAP fwmark routing (same as boxcutter-net + boxcutter-ctl: TAP creation, iptables mangle, ip rule/route, CONNMARK)
- Manages mark allocation (CRC32 with collision detection)
- Manages dm-snapshots, Firecracker lifecycle
- Coordinates with vmid via admin socket (register with mark + mode)
- Manages paranoid mode (iptables FORWARD rules, proxy env injection)
- Manages CA cert injection at create time
- Reports capacity and health
- Reads all config from `boxcutter.yaml`

**vmid integration:** Node agent co-starts vmid (or embeds it). The proxy could also be embedded.

**Deliverable:** Node is controllable via HTTP API. Bash scripts become optional/deprecated.

### Phase 3: Orchestrator

The central brain.

**New binary:** `boxcutter-orchestrator` (Go)
- SSH control interface (extends current `boxcutter-ssh` for multi-node)
- Node registry + heartbeat
- VM registry (SQLite) — includes mark and mode
- Scheduling: pick best node for `new`
- Command routing: `list` aggregates across nodes, `destroy` routes to correct node
- Packages bootstrap bundle into cloud-init for new nodes

**Deliverable:** Users SSH to orchestrator. VMs created across nodes.

### Phase 4: Migration

**Node agent additions:**
- `POST /api/vms/{name}/export` — deregister from vmid, stop VM, tear down TAP + fwmark, prepare COW for transfer
- `POST /api/vms/{name}/import` — receive COW, allocate mark, create TAP + fwmark routing, start VM, register with vmid

**Orchestrator additions:**
- `POST /api/migrate` — orchestrate export → transfer → import
- `ssh boxcutter drain <node>` — migrate all VMs off a node
- `ssh boxcutter migrate <vm> [--to <node>]`
- Direct node-to-node rsync, with TrueNAS staging fallback

**Deliverable:** VMs move between nodes. Tailscale identity preserved. Nodes can be drained and retired.

### Phase 5: TrueNAS Integration

**Golden image management:**
- NFS mount from TrueNAS for golden images
- Node agent syncs golden from NFS on boot
- Orchestrator tracks golden image versions
- `ssh boxcutter golden build` → build on a node → push to TrueNAS NFS

**Migration staging:**
- NFS mount for migration staging area
- Fallback for cross-host transfers where direct rsync is slow

**Deliverable:** Golden images centralized on TrueNAS. Migration has staging fallback.

---

## What Changes vs. What Stays

| Aspect | Current (single-node) | Multi-host design | Changes? |
|--------|----------------------|-------------------|----------|
| VM networking | Per-TAP point-to-point (10.0.0.1 ↔ 10.0.0.2) | Same per-node | **No change** |
| Mark allocation | CRC32 of name, 1–65535 | Same per-node | **No change** |
| VM isolation | No shared L2 (isolated TAPs) | Same | **No change** |
| fwmark routing | CONNMARK save/restore + per-mark policy routes | Same | **No change** |
| Tailscale overlay | Per-VM ephemeral join | Same | **No change** |
| vmid | Go service, fwmark-based identity, port 80 | Same, per-node | **No change** |
| Sentinel tokens | In-memory store, one-time swap via proxy | Same, per-node | **No change** |
| Forward proxy | MITM HTTPS, sentinel swapping, port 8080 | Same, per-node | **No change** |
| Internal TLS | CA + leaf cert, per-VM CA injection | CA shared across nodes via bundle | **Minor** |
| Device-mapper COW | Local snapshots | Same | **No change** |
| Kernel ip= boot | `10.0.0.2::10.0.0.1:...:eth0:off:8.8.8.8` | Same (no rewrite on migration) | **No change** |
| Normal/paranoid modes | Per-VM iptables + proxy env | Same | **No change** |
| Control interface | `boxcutter-ssh` on node | Orchestrator SSH (multi-node aware) | **Extended** |
| VM lifecycle | `boxcutter-ctl` (bash) | Node agent (Go HTTP API) | **Rewritten** |
| Network setup | `boxcutter-net` (bash) | Node agent (Go) | **Subsumed** |
| Secrets | Scattered files | `boxcutter.yaml` + `secrets/` | **Standardized** |
| Golden images | Local only | TrueNAS NFS (centralized) | **New** |
| Migration | N/A | rsync + TrueNAS staging | **New** |
| Multi-node scheduling | N/A | Orchestrator | **New** |

---

## Open Questions

### Q1: Orchestrator placement

The orchestrator needs to be highly available (it's the SSH entry point). Options:
- (a) Small VM on one physical host (simple, SPOF)
- (b) Container on any host (easy to restart)
- (c) Redundant pair with shared SQLite (complex)

Recommendation: (a) for now, with SQLite backed up to TrueNAS NFS. If the orchestrator host dies, restore from backup on another host.

### Q2: Golden image builds

Where do golden image builds happen?
- On a dedicated build node
- On any node (orchestrator picks one)
- On the orchestrator itself

Recommendation: On any node. The orchestrator picks a node, tells it to build, then the result is pushed to TrueNAS NFS. This is the same pattern as current `boxcutter-ctl golden build/provision`, just triggered remotely.

### Q3: Node auto-scaling

Should the orchestrator automatically create new nodes when capacity is exhausted?
- This requires knowing the physical host's remaining resources
- Multi-host: orchestrator needs to know which physical hosts exist and their capacity

For v1: manual node creation, orchestrator just tracks what exists.

### Q4: Bootstrap bundle delivery for multi-host

On a single physical host, the orchestrator can inject the bundle into cloud-init ISOs directly. Across multiple hosts, the operator needs to place the bundle on each host before the orchestrator can create nodes there.

Options:
- (a) Operator manually copies bundle to each physical host
- (b) Bundle lives on TrueNAS NFS (all hosts can access it)
- (c) Orchestrator SSHes to physical hosts to deliver bundle

Recommendation: (b) — TrueNAS NFS is already available to all hosts. The bundle (minus the actual secret files, which are small) can live on a restricted NFS share.

### Q5: Shared CA across nodes

For paranoid mode to work across migration (VM trusts proxy on any node), all nodes must share the same internal CA. The CA keypair should be part of the bootstrap bundle. Leaf certs (with IP SAN 10.0.0.1) can be generated per-node since all nodes use the same gateway IP.
