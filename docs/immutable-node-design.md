# Multi-Host Architecture Design

## Status

This document builds on the existing single-node boxcutter implementation. The networking layer (internal bridge + Tailscale overlay), vmid (VM Identity & Token Broker), device-mapper COW snapshots, and the SSH control interface are all implemented and working. This design extends the system to support multiple physical hosts, VM migration, centralized storage, and standardized configuration.

---

## Core Design Principles

1. **Multi-host from day one.** The orchestrator, nodes, and storage are designed to span physical hosts.
2. **VM state preservation matters.** Migration means moving a running VM's disk state to another node with zero data loss. Tailscale identity (and thus IP) survives migration.
3. **Nodes are immutable and replaceable.** To upgrade, spin up a new node, migrate VMs off the old, destroy the old.
4. **Tailscale is the trusted network.** All inter-component communication (orchestrator ↔ nodes) happens over Tailscale. Policies ensure nodes can talk to each other but VMs cannot reach nodes (except the metadata IP on their own node).
5. **Go for all new services.** Orchestrator and node agent are Go.
6. **Secrets are bootstrapped, not distributed.** All credentials come from a single, standardized config bundle provided at setup time. The orchestrator does not distribute secrets at runtime.
7. **Build on what works.** The existing bridge networking, vmid, COW snapshots, and SSH interface are proven. Extend, don't replace.

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
| vmid | Go | Each node (per-node) | — (internal only) | **Implemented** |
| boxcutter-ctl | Bash | Each node | — | **Implemented** (to be subsumed by node agent) |
| boxcutter-net | Bash | Each node | — | **Implemented** |
| TrueNAS | — | Dedicated NAS box or VM | `truenas` (or similar) | **Planned** |

---

## Current Networking Architecture (Implemented)

The networking layer is implemented and working. Each node VM runs an internal bridge with per-VM TAP devices and Tailscale overlay for external access.

```
┌─────────────────────────────────────────────────────┐
│  Physical Host                                       │
│                                                     │
│  tap-node0 (10.0.0.1/30) ──→ Node VM               │
│                                                     │
│  ┌───────────────────────────────────────────────┐  │
│  │  Node VM (QEMU/KVM) — 10.0.0.2               │  │
│  │  Tailscale: 100.x.x.x                        │  │
│  │                                               │  │
│  │  brvm0 bridge (10.0.1.1/24)                   │  │
│  │  ├── tap-bold-fox   ──→  10.0.1.200           │  │
│  │  ├── tap-calm-otter ──→  10.0.1.201           │  │
│  │  └── tap-wild-heron ──→  10.0.1.202           │  │
│  │  (ebtables FORWARD DROP: VM isolation)        │  │
│  │                                               │  │
│  │  vmid (port 8775) ← 169.254.169.254 redirect │  │
│  │  Identifies VMs by source IP (10.0.1.x)       │  │
│  └───────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────┘
```

### How It Works

- **Internal bridge** (`brvm0`, 10.0.1.0/24): Each VM gets a unique internal IP (10.0.1.200+) via TAP device on the bridge
- **VM isolation**: ebtables `FORWARD DROP` prevents VM-to-VM communication at Layer 2. Only the Node VM can reach VMs directly
- **Internet access**: NAT from bridge subnet to Node VM's uplink interface
- **Tailscale overlay**: Each VM joins Tailscale at boot (ephemeral key, never stored on disk), getting a stable 100.x.x.x IP reachable from anywhere
- **Instant networking**: Kernel `ip=` boot parameter — no DHCP, no network manager
- **Metadata service**: iptables PREROUTING redirects 169.254.169.254:80 → vmid:8775. vmid identifies VMs by source IP (10.0.1.x)

### What This Architecture Already Provides

| Concern | How it's handled |
|---------|-----------------|
| VM isolation | ebtables FORWARD DROP (kernel-enforced L2 isolation) |
| Internet access | NAT to uplink |
| External reachability | Tailscale overlay (100.x.x.x) |
| VM identity | vmid source-IP lookup (unique 10.0.1.x per VM) |
| Name resolution | Tailscale MagicDNS (ssh bold-fox) |
| Fast boot | Kernel ip= parameter, no DHCP |
| Auth key security | Key stays on Node VM, provisioned via SSH, never on VM disk |

### Networking Implications for Multi-Host

The bridge model works well for single-node operation. For multi-host, the key consideration is **migration**: when a VM moves between nodes, it needs a new internal IP on the destination node's bridge. This is acceptable because:

1. The internal IP is ephemeral — it's only used for Node→VM communication and vmid identity
2. The Tailscale IP (100.x.x.x) is the VM's stable identity and survives migration (state lives in `/var/lib/tailscale/` on the VM's rootfs)
3. The destination node just allocates the next free IP from its local pool
4. vmid on the destination node registers the VM with its new internal IP

No networking changes are required to support multi-host. Each node runs its own bridge independently.

---

## vmid (Implemented)

vmid is the VM Identity & Token Broker service, already implemented in Go and running per-node.

### What It Does

- **Identity**: Identifies VMs by source IP on the internal bridge (10.0.1.x → VM name)
- **JWT tokens**: Mints audience-scoped ES256 tokens signed with a per-deployment ECDSA key
- **GitHub tokens**: Policy-based GitHub App installation tokens for repo access
- **JWKS endpoint**: Public key endpoint at `/.well-known/jwks.json` for external token verification

### Interfaces

**VM-facing API** (via 169.254.169.254 redirect):
- `GET /identity` — VM record (vm_id, ip, labels)
- `GET /token?audience=<aud>` — Mint JWT token
- `GET /token/github` — Mint GitHub App token
- `GET /.well-known/jwks.json` — Public JWKS (no auth required)

**Admin API** (Unix socket at `/run/vmid/admin.sock`, node-local only):
- `POST /internal/vms` — Register VM (id, ip, github_repo)
- `DELETE /internal/vms/{id}` — Deregister VM
- `GET /internal/vms` — List registered VMs
- `POST /internal/vms/{id}/github-token` — Mint token on behalf of VM (used by boxcutter-ctl during clone)

### Multi-Host Consideration

vmid runs per-node with no changes needed. Each node's vmid has its own registry of local VMs. For consistent JWT verification across nodes, all nodes share the same ECDSA signing key (distributed via the bootstrap bundle).

---

## Secrets & Bootstrap Configuration

### The Problem

Today, secrets are scattered across multiple locations with no standard structure:

| Secret | Current location | How it gets there |
|--------|-----------------|-------------------|
| Tailscale auth key (VMs) | `/etc/boxcutter/tailscale-authkey` | Manual placement |
| SSH keypair (node→VM) | `/var/lib/boxcutter/ssh/id_ed25519` | Auto-generated by install.sh |
| User SSH public keys | `/etc/boxcutter/authorized_keys` | Seeded from cloud-init, then manual |
| GitHub App private key | `/etc/vmid/github-app.pem` | Manual placement |
| GitHub App ID + Install ID | `/etc/vmid/config.yaml` | Manual edit |
| JWT signing key | `/etc/vmid/jwt-key.pem` | Auto-generated or manual |
| TrueNAS API key | nowhere yet | — |

This means spinning up a new node requires manually placing files in multiple locations, knowing which paths each component expects, and hoping nothing was forgotten.

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

# Storage
storage:
  # Golden image source
  golden_nfs_path: "truenas:/mnt/pool/boxcutter/golden"
  # Local cache of golden image on each node
  golden_local_path: /var/lib/boxcutter/golden/rootfs.ext4
  # Migration staging area on TrueNAS
  staging_nfs_path: "truenas:/mnt/pool/boxcutter/staging"

# TrueNAS (optional — for NFS golden images and migration staging)
truenas:
  enabled: false
  host: truenas
  api_key_path: /etc/boxcutter/secrets/truenas-api-key

# VM defaults
vm_defaults:
  vcpu: 4
  ram_mib: 8192
  disk: 50G
  dns: 8.8.8.8
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
         └──> Golden image build reads bundle for SSH key injection
              Public key from node-ssh.pub goes into golden image
              authorized-keys goes into golden image
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
│   ├── vm.json
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

### Migration and Internal IP Reassignment

When a VM migrates between nodes, the destination node:
1. Allocates the next free internal IP from its local pool (10.0.1.x)
2. Rewrites the kernel boot args with the new internal IP
3. Registers the VM with its local vmid using the new IP
4. The Tailscale IP (100.x.x.x) is preserved — state lives in `/var/lib/tailscale/` on the COW image

The internal IP is ephemeral infrastructure; the Tailscale IP is the VM's stable identity.

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

Replaces the current bash scripts (`boxcutter-ctl`, `boxcutter-net`, `boxcutter-ssh`) with a single Go binary.

- **VM lifecycle**: Create, start, stop, destroy Firecracker VMs
- **Bridge management**: Set up brvm0 bridge, TAP devices, ebtables isolation (same as current boxcutter-net)
- **IP allocation**: Allocate internal IPs from the 10.0.1.200-250 pool (same as current boxcutter-ctl)
- **Storage management**: Device-mapper snapshots on local disk (same as current)
- **Tailscale provisioning**: Join VMs to Tailscale on start (same as current)
- **vmid coordination**: Register/deregister VMs with vmid via admin socket (same as current)
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
GET    /api/vms/{name}              VM details
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
4. Sets up bridge network (brvm0, ebtables isolation, NAT — same as current boxcutter-net)
5. Syncs golden image from TrueNAS NFS (if configured)
6. Joins Tailscale as `bc-<id>` using `tailscale-node-authkey`
7. Starts vmid (metadata service)
8. Registers with orchestrator: `POST orchestrator/api/nodes/register`
9. Begins accepting API calls

### VM Create Flow (on Node)

This is the same flow as current `boxcutter-ctl create` + `start`, translated to Go:

```
1. Receive POST /api/vms { name: "bold-fox", vcpu: 4, ram_mib: 8192, ... }

2. Allocate internal IP from pool (10.0.1.200-250)
   Same logic as current allocate_ip() in boxcutter-ctl

3. Create TAP device on bridge
   ip tuntap add dev tap-bold-fox mode tap
   ip link set tap-bold-fox master brvm0
   ip link set tap-bold-fox up

4. Create storage (local device-mapper)
   truncate -s 50G cow.img
   losetup + dmsetup create bc-bold-fox (COW on cached golden)

5. Inject SSH keys into rootfs
   Read authorized_keys_path and public_key_path from boxcutter.yaml
   mount block device → copy keys → umount

6. Write fc-config.json
   kernel boot args: ip=10.0.1.200::10.0.1.1:255.255.255.0:bold-fox:eth0:off:8.8.8.8
   drive: /dev/mapper/bc-bold-fox
   network: tap-bold-fox

7. Launch Firecracker
   firecracker --config-file fc-config.json

8. Wait for SSH ready (poll internal IP)
   ssh -i <node-ssh.key> dev@10.0.1.200 echo ready

9. Join Tailscale (using vm_authkey from boxcutter.yaml)
   ssh dev@10.0.1.200 "sudo tailscale up --authkey=<vm-authkey> --hostname=bold-fox"

10. Register with vmid (admin socket)
    POST /internal/vms { vm_id: "bold-fox", ip: "10.0.1.200" }

11. Report back to orchestrator: { tailscale_ip: "100.x.x.x", status: "running" }
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
     │                               ├── rsync cow.img ──────────>│ (direct or via TrueNAS)
     │<── { export_path, size } ─────┤                            │
     │                               │                            │
     ├── POST /vms/bold-fox/import ──────────────────────────────>│
     │                               │                            ├── Allocate new internal IP
     │                               │                            ├── Create TAP on bridge
     │                               │                            ├── Set up dm-snapshot (with received COW)
     │                               │                            ├── Update kernel boot args (new IP)
     │                               │                            ├── Start Firecracker
     │                               │                            ├── Tailscale rejoins (identity in COW)
     │                               │                            ├── Register with local vmid
     │<──────────────────────────────────── { tailscale_ip } ─────┤
     │                               │                            │
     ├── Update VM record ───────────────────────────────────────>│
     ├── DELETE /vms/bold-fox ───────>│                            │
     │                               ├── Cleanup TAP + storage    │
     │                               │                            │
```

### Tailscale Identity Preservation

The Tailscale node state lives in `/var/lib/tailscale/` on the VM's rootfs. Since we migrate the COW image (which contains the full rootfs delta from golden), the Tailscale identity comes with it. When the VM boots on the new node:
- Tailscale daemon finds existing state in `/var/lib/tailscale/`
- Rejoins the tailnet with the same node key
- Gets the same Tailscale IP and hostname
- Users see zero disruption

The internal IP (10.0.1.x) changes — it's whatever the destination node allocates. But this is invisible to users since all access is via Tailscale IP or MagicDNS hostname.

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
- Add `boxcutter.yaml.template` to the repo (already done)
- Update cloud-init to deliver the bundle

**Deliverable:** Single config file drives all provisioning. New deployment = fill in template + run setup.

### Phase 2: Node Agent (Go)

Extract VM lifecycle management from bash into a Go HTTP service that wraps the existing networking and storage patterns.

**New binary:** `boxcutter-node` (Go)
- HTTP API for VM CRUD
- Manages bridge setup (same as boxcutter-net: brvm0, ebtables, NAT)
- Manages IP allocation (same as boxcutter-ctl: 10.0.1.200-250 pool)
- Manages TAP devices, dm-snapshots, Firecracker lifecycle
- Coordinates with vmid via admin socket
- Reports capacity and health
- Reads all config from `boxcutter.yaml`

**vmid integration:** Node agent co-starts vmid (or embeds it).

**Deliverable:** Node is controllable via HTTP API. Bash scripts become optional/deprecated.

### Phase 3: Orchestrator

The central brain.

**New binary:** `boxcutter-orchestrator` (Go)
- SSH control interface (extends current `boxcutter-ssh` for multi-node)
- Node registry + heartbeat
- VM registry (SQLite)
- Scheduling: pick best node for `new`
- Command routing: `list` aggregates across nodes, `destroy` routes to correct node
- Packages bootstrap bundle into cloud-init for new nodes

**Deliverable:** Users SSH to orchestrator. VMs created across nodes.

### Phase 4: Migration

**Node agent additions:**
- `POST /api/vms/{name}/export` — stop VM, prepare COW for transfer
- `POST /api/vms/{name}/import` — receive COW, allocate new internal IP, start VM

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
| VM networking | Bridge (brvm0, 10.0.1.x, ebtables) | Same per-node bridge | **No change** |
| IP allocation | Pool 10.0.1.200-250 per node | Same pool per node | **No change** |
| VM isolation | ebtables FORWARD DROP | Same | **No change** |
| Tailscale overlay | Per-VM ephemeral join | Same | **No change** |
| vmid | Go service, source-IP identity | Same, per-node | **No change** |
| Device-mapper COW | Local snapshots | Same | **No change** |
| Kernel ip= boot | Static per-VM | Same | **No change** |
| Metadata redirect | 169.254.169.254 → vmid | Same | **No change** |
| Control interface | `boxcutter-ssh` on node | Orchestrator SSH (multi-node aware) | **Extended** |
| VM lifecycle | `boxcutter-ctl` (bash) | Node agent (Go HTTP API) | **Rewritten** |
| Bridge setup | `boxcutter-net` (bash) | Node agent (Go) | **Subsumed** |
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
