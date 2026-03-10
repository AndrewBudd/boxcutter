# Node Architecture

The node is the fundamental system that manages Firecracker VMs as a resource. It turns bare compute into isolated, networked, identity-aware dev environments. A node contains multiple cooperating services that together provide VM lifecycle, identity, networking, and credential brokering.

## Services

| Service | Binary | Port | Go Module |
|---------|--------|------|-----------|
| `boxcutter-node` | Node agent | `:8800` | `node/agent/` |
| `vmid` | VM identity | `169.254.169.254:80` + `/run/vmid/admin.sock` | `node/vmid/` |
| `boxcutter-proxy` | MITM proxy | `:8080` | `node/proxy/` |
| `boxcutter-net` | Network setup | — (oneshot) | Shell script |
| `boxcutter-derper` | DERP relay | `:443` | External binary |

## Per-VM State

Each VM gets a directory at `/var/lib/boxcutter/vms/{name}/`:

```
/var/lib/boxcutter/vms/my-dev-vm/
├── vm.json              — VM config + state
├── rootfs.ext4          — Sparse ext4 disk (~50GB allocated on-demand)
├── firecracker.json     — Firecracker VM config
├── metadata.json        — Cloud-init style metadata
├── firecracker.pid      — PID of running Firecracker process
├── firecracker.sock     — Firecracker API Unix socket
└── migrating            — Marker file (present only during migration)
```

**vm.json** contains all VM config:
```json
{
  "name": "my-dev-vm", "vcpu": 2, "ram_mib": 2048,
  "mark": 101, "mode": "normal", "mac": "AA:FC:00:00:00:65",
  "disk": "50G", "tap": "tap-my-dev-vm",
  "created": "2026-03-10T12:00:00Z",
  "clone_urls": ["https://github.com/user/repo"],
  "tailscale_ip": "100.x.x.x", "golden_version": "v0.1.0-abc123"
}
```

**Storage models:** New VMs use file-based rootfs (standalone sparse ext4 copied from golden image via `CreateRootfs()`). Legacy VMs may use dm-snapshot COW overlays (`cow.img` on shared golden). `IsFileRootfs(vmDir)` distinguishes between the two.

## Node Agent

The node agent manages Firecracker VM lifecycle via an HTTP API on `:8800`.

**API endpoints:**

| Endpoint | Description |
|----------|-------------|
| `POST /api/vms` | Create + start a Firecracker VM |
| `DELETE /api/vms/{name}` | Stop + destroy a VM |
| `GET /api/vms` | List VMs on this node |
| `GET /api/vms/{name}` | VM details |
| `POST /api/vms/{name}/stop` | Stop a VM |
| `POST /api/vms/{name}/start` | Start a stopped VM |
| `POST /api/vms/{name}/copy` | Clone a VM's disk |
| `POST /api/vms/{name}/migrate` | Migrate VM to another node |
| `POST /api/vms/{name}/import-snapshot` | Import a migrated VM snapshot |
| `POST /api/vms/{name}/export` | Export VM state |
| `POST /api/vms/{name}/import` | Import VM state |
| `POST /api/vms/{name}/repos` | Add repos to a VM |
| `GET /api/vms/{name}/repos` | List repos on a VM |
| `DELETE /api/vms/{name}/repos/{repo...}` | Remove a repo |
| `GET /api/golden/versions` | List golden image versions |
| `GET /api/golden/{version}` | Golden image details |
| `POST /api/golden/build` | Build golden image |
| `GET /api/health` | Health check |

**Internal packages:**

| Package | Responsibility |
|---------|---------------|
| `internal/api/` | HTTP handlers |
| `internal/vm/` | Firecracker process management — create, destroy, start, stop, snapshot, restore, per-TAP networking |
| `internal/golden/` | Golden image versioning — tracks available versions, active head |
| `internal/network/` | TAP device and fwmark setup |
| `internal/mqtt/` | MQTT client — subscribes to golden image updates |
| `internal/vmid/` | HTTP client to the vmid service |
| `internal/config/` | Agent configuration |

## VM Identity (vmid)

vmid maps Firecracker VMs to identities using Linux fwmark-based routing. It serves the cloud metadata API at `169.254.169.254:80` and an admin API on a Unix socket.

**How it identifies VMs:** All Firecracker VMs are `10.0.0.2`, so vmid can't use source IP. Instead, it reads the fwmark from each TCP connection via `getsockopt(SOL_SOCKET, SO_MARK)`. The sysctl `net.ipv4.tcp_fwmark_accept=1` causes accepted sockets to inherit the packet's fwmark.

**VM-facing endpoints** (169.254.169.254:80):

| Endpoint | Description |
|----------|-------------|
| `GET /` | Metadata root (VM ID, available endpoints) |
| `GET /identity` | VM record (ID, mark, mode) |
| `GET /token` | ES256 JWT for the requesting VM |
| `GET /token/github` | GitHub App installation token |
| `GET /metadata/ssh-keys` | SSH public keys (no auth) |
| `GET /metadata/ca-cert` | Internal CA certificate (no auth) |
| `GET /.well-known/jwks.json` | JWKS public key (no auth) |

**Admin endpoints** (`/run/vmid/admin.sock`):

| Endpoint | Description |
|----------|-------------|
| `POST /internal/vms` | Register a VM (with mark + mode) |
| `DELETE /internal/vms/{id}` | Deregister (purges sentinels) |
| `GET /internal/vms` | List registered VMs |
| `GET /internal/vms/{id}` | Get VM details |
| `GET /internal/sentinel/{sentinel}` | Swap sentinel for real token |
| `POST /internal/vms/{id}/github-token` | Set GitHub token for VM |
| `POST /internal/vms/{id}/repos` | Add repos for VM |
| `POST /internal/ghcr-token` | Set GHCR token |

**This is the only module with unit tests**: `cd node/vmid && go test ./...`

## Forward Proxy

MITM forward proxy on `:8080` using `elazarl/goproxy`. Used in paranoid mode where VMs never see real API keys.

Capabilities:
1. **MITM HTTPS** — uses the internal CA cert to intercept HTTPS traffic
2. **Sentinel token swapping** — scans Authorization headers, swaps sentinel tokens for real credentials via vmid's admin socket
3. **Egress allowlist** — in paranoid mode, restricts which domains VMs can reach (`/etc/boxcutter/proxy-allowlist.conf`)

## Golden Image

The golden image defines the Firecracker guest environment. Built from `golden/Dockerfile`:

| File | Purpose |
|------|---------|
| `Dockerfile` | Guest rootfs definition (Ubuntu base + tools) |
| `docker-to-ext4.sh` | Converts Docker image → sparse ext4 filesystem |
| `nss_catchall.c` | NSS module enabling any-username SSH login |
| `vsock_listen.c` | Listens for migration nudge over vsock |
| `config/` | systemd units, SSH config, metadata fetch scripts installed in guest |

The image is SHA256-versioned. Building: `sudo boxcutter-ctl golden build` on a node.

## Shell Scripts

| Script | Role |
|--------|------|
| `boxcutter-ctl` | Firecracker VM manager — create, destroy, list, shell, logs, golden build |
| `boxcutter-setup` | Bundle validation + secret generation on first boot |
| `boxcutter-net` | One-time network infrastructure — shared fwmark/NAT rules |
| `boxcutter-tls` | CA + leaf certificate generation (idempotent) |
| `boxcutter-ssh` | SSH identity wrapper |

## Migration

Firecracker VMs migrate using snapshot/restore:

```
Source Node                              Target Node
    │                                        │
    ├─ Pre-stage golden image ──────────────>│  (while VM runs)
    │                                        │
    ├─ PATCH /vm {"state":"Paused"}          │  (sub-millisecond)
    ├─ PUT /snapshot/create                  │  (vm.snap + vm.mem)
    │                                        │
    ├─ tar --sparse COW+snap+mem ──SSH──────>│  (~10s for 2GB RAM)
    │                                        │
    │                                        ├─ fresh firecracker --api-sock
    │                                        ├─ PUT /snapshot/load {resume: true}
    │                                        ├─ vsock nudge -> tailscale netcheck
    │                                        │
    ├─ Stop source, cleanup                  │
```

What survives: all processes and memory, Tailscale identity and IP, network connections (after DERP re-establishment).

Downtime: ~10 seconds for a 2GB RAM VM (dominated by memory file transfer over bridge network).

## Normal vs Paranoid Mode

**Normal mode:** Full direct internet access via NAT. Real credentials from vmid token endpoints. No proxy required.

**Paranoid mode:** All outbound traffic must go through the forward proxy. iptables rules block direct internet access but allow traffic to the proxy. VMs receive sentinel tokens instead of real credentials. The proxy swaps sentinels for real tokens before forwarding.

## Health Endpoint

`GET /api/health` returns capacity and status:

```json
{
  "hostname": "boxcutter-node-1",
  "vcpu_total": 6, "vcpu_allocated": 4,
  "ram_total_mib": 12288, "ram_allocated_mib": 4096, "ram_free_mib": 8192,
  "disk_total_mb": 40960, "disk_used_mb": 12000,
  "vms_total": 3, "vms_running": 2,
  "golden_ready": true, "status": "active"
}
```

## Concurrency

The VM manager uses a single global `sync.Mutex` — all VM operations (create, destroy, start, stop, migrate) are serialized. Creating VM #1 (60s) blocks all other operations. This is the biggest performance bottleneck. See the [improvement proposal](../../docs/improvement-proposal.md) for the per-VM lock plan.
