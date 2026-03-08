# Development Guide

How to work on Boxcutter itself — building from source, deploying changes, and publishing images.

## Prerequisites

- A running Boxcutter cluster (see [README](README.md) for bootstrap)
- Go 1.22+ at `/usr/local/go`
- `qemu-system-x86_64`, `qemu-utils`, `genisoimage`, `zstd`
- `gh` CLI (for pushing OCI images)

## Code Layout

Boxcutter is five independent Go modules plus shell scripts:

```
boxcutter/
├── host/                        # Control plane (runs on bare metal)
│   ├── cmd/host/main.go         # boxcutter-host binary
│   ├── internal/                # bridge, cluster, qemu, oci packages
│   ├── go.mod
│   ├── boxcutter.env            # VM sizing and network config
│   ├── provision.sh             # Cloud-init ISO generator
│   ├── build-image.sh           # Builds base QCOW2 images
│   └── publish-image.sh         # Build + push to ghcr.io
│
├── orchestrator/                # Orchestrator VM (scheduling, SSH interface)
│   ├── cmd/orchestrator/        # HTTP API server (:8801)
│   ├── cmd/ssh/                 # SSH ForceCommand binary
│   ├── internal/                # api, config, db, mqtt packages
│   └── go.mod
│
├── node/
│   ├── agent/                   # Node agent (Firecracker VM lifecycle)
│   │   ├── cmd/node/main.go     # boxcutter-node binary (:8800)
│   │   ├── internal/            # vm, fcapi, networking, mqtt, golden
│   │   └── go.mod
│   │
│   ├── vmid/                    # VM identity & token broker
│   │   ├── cmd/vmid/main.go     # vmid binary (:80)
│   │   ├── internal/            # registry, sentinel, token, api
│   │   └── go.mod
│   │
│   ├── proxy/                   # MITM forward proxy
│   │   ├── cmd/proxy/main.go    # boxcutter-proxy binary (:8080)
│   │   └── go.mod
│   │
│   ├── golden/                  # Firecracker rootfs builder
│   │   ├── build.sh             # Phase 1: debootstrap
│   │   ├── provision.sh         # Phase 2: dev tools
│   │   ├── nss_catchall.c       # NSS module (any-username SSH)
│   │   └── vsock_listen.c       # Migration nudge listener
│   │
│   ├── scripts/                 # Shell scripts installed on nodes
│   │   ├── boxcutter-ctl        # Firecracker VM manager
│   │   ├── boxcutter-setup      # Bundle validation + secret generation
│   │   ├── boxcutter-net        # Per-VM TAP + fwmark networking
│   │   ├── boxcutter-tls        # CA + leaf cert generation
│   │   └── boxcutter-ssh        # SSH identity wrapper
│   │
│   └── systemd/                 # Service unit files
│
├── Makefile                     # Top-level build targets
└── .github/workflows/           # CI/CD
```

Each Go module is independent — you can `cd` into any module directory and run `go build`, `go test`, etc. without affecting others.

## Building

### Individual binaries

```bash
# Host control plane
cd host && go build -o boxcutter-host ./cmd/host/

# Orchestrator
cd orchestrator && go build -o boxcutter-orchestrator ./cmd/orchestrator/
cd orchestrator && go build -o boxcutter-ssh-orchestrator ./cmd/ssh/

# Node agent
cd node/agent && go build -o boxcutter-node ./cmd/node/

# VM identity broker
cd node/vmid && go build -o vmid ./cmd/vmid/

# MITM proxy
cd node/proxy && go build -o boxcutter-proxy ./cmd/proxy/
```

All binaries target `linux/amd64`. Cross-compilation works with `GOOS=linux GOARCH=amd64`.

### Make targets

```bash
make build-host          # Build boxcutter-host
make install-host        # Build + install to /usr/local/bin + systemd
make help                # Show all targets
```

### Tests

```bash
cd node/vmid && go test ./...
```

Tests exist for the vmid registry (mark allocation) and sentinel store (token management). Other modules don't have tests yet — they're validated through integration testing on real VMs.

## Development Workflow

The fastest way to iterate depends on which component you're changing.

### Host control plane (`boxcutter-host`)

The host binary runs directly on bare metal. Build and install:

```bash
make install-host
sudo systemctl restart boxcutter-host
```

Or run it directly without the service:

```bash
cd host && go build -o boxcutter-host ./cmd/host/
sudo ./boxcutter-host status
```

### Node services (agent, vmid, proxy)

These run inside Node VMs. The fastest iteration loop is to cross-compile, `scp` the binary in, and restart the service:

```bash
# Build for linux/amd64
cd node/agent && GOOS=linux GOARCH=amd64 go build -o boxcutter-node ./cmd/node/

# Copy to the running node VM
scp -o StrictHostKeyChecking=no boxcutter-node ubuntu@192.168.50.3:/tmp/

# SSH in, replace binary, restart
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.3 \
  "sudo mv /tmp/boxcutter-node /usr/local/bin/ && sudo systemctl restart boxcutter-node"
```

Same pattern works for `vmid` and `boxcutter-proxy`. The service names match the binary names.

You can also use Tailscale hostnames instead of bridge IPs if MagicDNS is set up:

```bash
scp boxcutter-node ubuntu@boxcutter-node-1:/tmp/
```

### Orchestrator services

Same approach — build, copy, restart:

```bash
cd orchestrator && GOOS=linux GOARCH=amd64 go build -o boxcutter-orchestrator ./cmd/orchestrator/
scp -o StrictHostKeyChecking=no boxcutter-orchestrator ubuntu@192.168.50.2:/tmp/
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2 \
  "sudo mv /tmp/boxcutter-orchestrator /usr/local/bin/ && sudo systemctl restart boxcutter-orchestrator"
```

For the SSH ForceCommand binary:

```bash
cd orchestrator && GOOS=linux GOARCH=amd64 go build -o boxcutter-ssh-orchestrator ./cmd/ssh/
scp -o StrictHostKeyChecking=no boxcutter-ssh-orchestrator ubuntu@192.168.50.2:/tmp/
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2 \
  "sudo mv /tmp/boxcutter-ssh-orchestrator /usr/local/bin/"
```

No restart needed for the SSH binary — it's invoked fresh on each SSH connection.

### Shell scripts

Scripts on the node live at `/usr/local/bin/`. The repo is also available inside the Node VM via 9p mount at `/mnt/boxcutter/`. During development you can symlink or just copy:

```bash
scp node/scripts/boxcutter-ctl ubuntu@192.168.50.3:/tmp/
ssh ubuntu@192.168.50.3 "sudo mv /tmp/boxcutter-ctl /usr/local/bin/ && sudo chmod +x /usr/local/bin/boxcutter-ctl"
```

### Golden image

The golden image is the Firecracker microVM rootfs. Rebuilding it is slower (boots a VM, installs packages):

```bash
# SSH into a node
ssh ubuntu@192.168.50.3

# Phase 1: build base rootfs (debootstrap, ~5 min)
sudo boxcutter-ctl golden build

# Phase 2: provision dev tools (boots rootfs, installs packages, ~10 min)
sudo boxcutter-ctl golden provision
```

After rebuilding, new VMs use the updated image. Existing VMs are unaffected.

To distribute the updated golden image to all nodes, publish it to OCI and set the head version:

```bash
# On the host
sudo boxcutter-host push-golden

# Via SSH to orchestrator
ssh boxcutter golden set-head <version>
```

Nodes pull the new version automatically via MQTT notification.

## SSH Access to VMs

```bash
# Via Makefile
make ssh-node              # SSH to node-1
make ssh-orchestrator      # SSH to orchestrator

# Via script (supports multiple nodes)
bash host/ssh.sh node 1    # node-1
bash host/ssh.sh node 2    # node-2

# Direct
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2   # orchestrator
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.3   # node-1
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.4   # node-2
```

## Configuration

`host/boxcutter.env` controls VM sizing and network layout:

```bash
HOST_INTERFACE=enp34s0      # Physical NIC for NAT
NODE_VCPU=6                 # vCPUs per node VM
NODE_RAM=12G                # RAM per node VM
NODE_DISK=150G              # Disk per node VM
ORCH_VCPU=2                 # Orchestrator vCPUs
ORCH_RAM=4G                 # Orchestrator RAM
```

Firecracker microVM defaults (2 vCPU, 2GB RAM, 50G disk) are set in the orchestrator API handlers and the node agent's config.

## Building and Publishing OCI Images

When your changes are ready to ship, bake them into OCI images. These are what `boxcutter-host bootstrap` pulls on a fresh machine.

### Build locally

```bash
# Build a node image (boots a VM, installs everything, flattens to QCOW2)
make build-image TYPE=node

# Build an orchestrator image
make build-image TYPE=orchestrator
```

This takes ~15-20 minutes per image. It boots a fresh Ubuntu cloud image as a QEMU VM, runs cloud-init to install all binaries and packages, cleans instance-specific state, then flattens the disk to a standalone compressed QCOW2.

Output goes to `.images/<type>-image.qcow2.zst`.

### Push to ghcr.io

```bash
# Authenticate first
gh auth login

# Build + push
make publish TYPE=node
make publish TYPE=orchestrator

# Or both at once
make publish-all

# Push a previously built image without rebuilding
./host/publish-image.sh node --push-only

# Custom tag (default: git short hash)
./host/publish-image.sh node --tag v1.0
```

Images are pushed to `ghcr.io/andrewbudd/boxcutter/{node,orchestrator}` with both a specific tag and `latest`.

### What the image build does

`host/build-image.sh` runs through these steps:

1. Downloads Ubuntu 24.04 cloud image (if not cached)
2. Creates a COW overlay disk
3. Generates a cloud-init ISO with all binaries, scripts, configs, and systemd units
4. Boots QEMU with the overlay + ISO
5. Cloud-init runs: installs packages, places binaries, enables services
6. SSHs in to clean instance-specific state (Tailscale, cloud-init, host keys)
7. Powers off the VM
8. Flattens the COW overlay to a standalone QCOW2
9. Compresses with zstd

The cleanup step may return a non-zero exit code (SSH disconnects when the VM powers off). The script checks for the output file rather than relying on the exit code.

## Upgrading a Running Cluster

After pushing new images:

```bash
# Pull latest and rolling-upgrade everything
sudo boxcutter-host upgrade all

# Just nodes (launches new node, drains old, zero VM downtime)
sudo boxcutter-host upgrade node

# Just orchestrator
sudo boxcutter-host upgrade orchestrator

# Just the golden image (nodes pull via MQTT)
sudo boxcutter-host upgrade golden
```

Node upgrades use drain + migration: a new node is launched from the latest image, all Firecracker VMs are migrated off the old node via snapshot/restore, then the old node is stopped. User VMs experience ~10s of downtime during migration.

## CI/CD

GitHub Actions workflow at `.github/workflows/build-image.yml`:

- **Triggers**: git tags (`v*`) or manual `workflow_dispatch`
- **Runs on**: self-hosted runner with KVM support
- **Matrix**: builds `node` and `orchestrator` images in parallel
- **Steps**: checkout → Go setup → install deps → build image → push to ghcr.io → upload artifact

The workflow requires a self-hosted runner because image builds need KVM access to boot QEMU VMs.

## Systemd Services

Services on each VM and their corresponding binaries:

**Node VM:**

| Service | Binary | Port | Description |
|---------|--------|------|-------------|
| `vmid` | `vmid` | :80 + `/run/vmid/admin.sock` | VM identity, fwmark-based |
| `boxcutter-proxy` | `boxcutter-proxy` | :8080 | MITM proxy, sentinel tokens |
| `boxcutter-node` | `boxcutter-node` | :8800 | Node agent, VM lifecycle API |
| `boxcutter-net` | (shell script) | — | Network setup (oneshot) |
| `boxcutter-derper` | `derper` | :443 | Tailscale DERP relay |

**Orchestrator VM:**

| Service | Binary | Port | Description |
|---------|--------|------|-------------|
| `boxcutter-orchestrator` | `boxcutter-orchestrator` | :8801 | Scheduling, state, MQTT |

**Host:**

| Service | Binary | Port | Description |
|---------|--------|------|-------------|
| `boxcutter-host` | `boxcutter-host` | `/run/boxcutter-host.sock` | Control plane |

## Debugging

### Logs

```bash
# On the host
sudo journalctl -u boxcutter-host -f

# On a node VM
ssh ubuntu@192.168.50.3 "sudo journalctl -u boxcutter-node -f"
ssh ubuntu@192.168.50.3 "sudo journalctl -u vmid -f"
ssh ubuntu@192.168.50.3 "sudo journalctl -u boxcutter-proxy -f"

# On the orchestrator
ssh ubuntu@192.168.50.2 "sudo journalctl -u boxcutter-orchestrator -f"
```

### Node agent API

The node agent exposes an HTTP API on port 8800:

```bash
# List VMs on a node
curl http://192.168.50.3:8800/api/vms

# VM details
curl http://192.168.50.3:8800/api/vms/<name>

# Health check
curl http://192.168.50.3:8800/healthz

# Golden image versions
curl http://192.168.50.3:8800/api/golden
```

### vmid admin socket

```bash
ssh ubuntu@192.168.50.3 \
  "sudo curl --unix-socket /run/vmid/admin.sock http://localhost/healthz"
```

### Orchestrator API

```bash
curl http://192.168.50.2:8801/api/vms
curl http://192.168.50.2:8801/api/nodes
curl http://192.168.50.2:8801/healthz
```

### Host control plane socket

```bash
sudo curl --unix-socket /run/boxcutter-host.sock http://localhost/status
```

### Firecracker VM management

On a node VM, `boxcutter-ctl` manages individual Firecracker microVMs:

```bash
ssh ubuntu@192.168.50.3

sudo boxcutter-ctl list                  # List running VMs
sudo boxcutter-ctl shell <name>          # Shell into a Firecracker VM
sudo boxcutter-ctl logs <name>           # View VM serial console log
```

## Architecture Reference

- [System Architecture](architecture.md) — control/data plane, OCI distribution, MQTT, migration
- [Network Architecture](network-architecture.md) — TAP/fwmark routing, vmid, proxy, packet flows
