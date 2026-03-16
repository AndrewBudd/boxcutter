# Boxcutter

Ephemeral dev environments on bare metal. Spin up a VM in under a second.

Boxcutter runs [Firecracker](https://firecracker-microvm.github.io/) microVMs inside QEMU/KVM node VMs on a physical Linux machine. Each microVM joins [Tailscale](https://tailscale.com/) automatically, making it accessible from anywhere on your tailnet.

```
$ ssh boxcutter new

  Name:    bold-fox
  Node:    boxcutter-node-1
  vCPU:    2
  RAM:     2G
  Disk:    50G
  IP:      100.64.1.42
  Status:  running

  Connect: ssh bold-fox

$ ssh bold-fox
dev@bold-fox:~$
```

## Architecture

```
+--------------------------------------------------------------+
| Physical Host (Ubuntu 24.04)                                  |
|                                                               |
|  boxcutter-host (systemd service)                             |
|    - bridge/TAP/NAT management                                |
|    - boot recovery, health monitoring                         |
|    - auto-scaling, drain/migration coordination               |
|    - OCI image pull, VM provisioning                          |
|                                                               |
|  br-boxcutter (192.168.50.1/24)                               |
|    |                                                          |
|    +-- tap-orch ---- Orchestrator VM (192.168.50.2)           |
|    |                   SSH control interface (:22)             |
|    |                   Scheduling, key management (:8801)      |
|    |                                                          |
|    +-- tap-node1 --- Node VM 1 (192.168.50.3)                 |
|    |                   Firecracker agent (:8800)               |
|    |                   +------+ +------+ +------+             |
|    |                   |fox   | |otter | |heron |  microVMs   |
|    |                   |FC 2G | |FC 2G | |FC 2G |             |
|    |                   +------+ +------+ +------+             |
|    |                                                          |
|    +-- tap-node2 --- Node VM 2 (192.168.50.4)                 |
|                        (auto-scaled when capacity > 80%)      |
+--------------------------------------------------------------+
```

Three layers:

1. **Physical host** -- runs `boxcutter-host` control plane, QEMU VMs, bridge/NAT
2. **Orchestrator VM** -- SSH control interface, scheduling, SSH key management
3. **Node VMs** -- run Firecracker microVMs, handle migration, golden image builds

## Requirements

- **OS:** Ubuntu 24.04 on a physical machine (not a VM -- nested KVM required)
- **CPU:** x86_64 with hardware virtualization (Intel VT-x / AMD-V)
- **RAM:** 32GB minimum (64GB recommended)
- **Disk:** 50GB minimum (150GB+ recommended)
- **Network:** Internet access for Tailscale + image pulls

Verify KVM + nested virtualization:

```bash
ls /dev/kvm                                     # Should exist
cat /sys/module/kvm_amd/parameters/nested       # AMD: should say 1 or Y
cat /sys/module/kvm_intel/parameters/nested      # Intel: should say 1 or Y
```

## Install

### 1. Install system dependencies

```bash
sudo apt install -y qemu-system-x86 qemu-utils genisoimage zstd mosquitto mosquitto-clients
```

### 2. Install boxcutter-host

```bash
wget https://github.com/AndrewBudd/boxcutter/releases/latest/download/boxcutter-host_amd64.deb
sudo dpkg -i boxcutter-host_amd64.deb
```

Verify: `boxcutter-host version`

### 3. Create your secrets bundle

```bash
mkdir -p ~/.boxcutter/secrets
```

You need to create these files:

| File | Description |
|------|-------------|
| `tailscale-orch-authkey` | Tailscale reusable auth key for the orchestrator |
| `tailscale-node-authkey` | Tailscale reusable auth key for node VMs |
| `tailscale-vm-authkey` | Tailscale reusable+ephemeral auth key for Firecracker VMs |
| `cluster-ssh.key` | SSH key for inter-node migration (see below) |
| `authorized-keys` | Your SSH public key(s) for the control interface |
| `github-app.pem` | *(Optional)* GitHub App private key for repo cloning in VMs |

Get Tailscale auth keys from [https://login.tailscale.com/admin/settings/keys](https://login.tailscale.com/admin/settings/keys). Create reusable keys. For the VM key, also check "Ephemeral".

Generate the cluster SSH key:

```bash
ssh-keygen -t ed25519 -f ~/.boxcutter/secrets/cluster-ssh.key -N ""
```

Copy your SSH public key:

```bash
cp ~/.ssh/id_ed25519.pub ~/.boxcutter/secrets/authorized-keys
# or: cp ~/.ssh/id_rsa.pub ~/.boxcutter/secrets/authorized-keys
```

You also need `~/.boxcutter/boxcutter.yaml`. Copy the template:

```bash
cp config/boxcutter.yaml.template ~/.boxcutter/boxcutter.yaml
```

Edit it to fill in your GitHub App ID and installation ID (or set `github.enabled: false` if you don't need repo cloning).

### 4. Bootstrap the cluster

```bash
sudo boxcutter-host bootstrap
```

This takes about 5 minutes. It will:

1. Pull pre-built VM images from `ghcr.io/andrewbudd/boxcutter/`
2. Create the bridge network and NAT rules
3. Launch the orchestrator and first node VM
4. Build the golden image (Firecracker rootfs)

Bootstrap is idempotent -- if it fails partway through (e.g. network timeout), just re-run it.

### 5. Start the service

```bash
sudo systemctl enable --now boxcutter-host
```

### 6. Verify

```bash
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 status
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 new
```

Once the orchestrator joins Tailscale, you can use:

```bash
ssh boxcutter status
ssh boxcutter new
```

## Usage

All commands go through the orchestrator SSH interface:

```bash
ssh boxcutter new [options]        # Create a new VM
  --type <type>                    #   firecracker (default) or qemu
  --clone <repo>                   #   Clone repo on creation (repeatable)
  --vcpu <N>                       #   CPU cores (default: 2)
  --ram <MiB>                      #   RAM in MiB (default: 2048)
  --disk <size>                    #   Disk size (default: 50G)
  --mode normal|paranoid           #   Network mode (default: normal)
  --node <node-id>                 #   Pin to specific node
ssh boxcutter list                 # List all VMs (shows type: fc/qemu)
ssh boxcutter destroy <name>       # Destroy a VM
ssh boxcutter stop <name>          # Stop a running VM
ssh boxcutter start <name>         # Start a stopped VM
ssh boxcutter cp <name> [new-name] # Clone a VM's disk
ssh boxcutter status               # Cluster capacity summary
ssh boxcutter nodes                # List all nodes with health
ssh boxcutter adduser <github>     # Add SSH keys from GitHub
ssh boxcutter repos add <vm> <repo>  # Grant GitHub repo access to a VM
ssh boxcutter repos list <vm>        # List repos a VM can access
```

## VM Types: Firecracker vs QEMU

Boxcutter supports two VM backends:

**Firecracker** (default) -- lightweight microVMs that boot in ~200ms. Best for most dev workloads. Limitations: no Docker support (minimal kernel lacks netfilter modules).

**QEMU** (`--type qemu`) -- full VMs with a complete Linux kernel. Docker and docker-compose work out of the box. Boot time is ~5-10 seconds. Use this when your project needs Docker, docker-compose, or other workloads that require full kernel support.

```bash
# Firecracker VM (default, fast, lightweight)
ssh boxcutter new

# QEMU VM (Docker support, full kernel)
ssh boxcutter new --type qemu --ram 4096

# QEMU VM with a repo cloned
ssh boxcutter new --type qemu --ram 4096 --clone github.com/org/repo
```

VMs can identify their type by reading `/etc/boxcutter/vm-type` (contains `firecracker` or `qemu`).

The `list` command shows the type for each VM:

```
NAME          TAILSCALE IP    NODE              TYPE   VCPU  RAM   MODE    STATUS
my-vm         100.64.1.42     boxcutter-node-1  fc     2     2G    normal  running
docker-dev    100.64.1.43     boxcutter-node-1  qemu   2     4G    normal  running
```

### GitHub Repo Access

When cloning private repos or using git submodules, grant access via `repos add`:

```bash
ssh boxcutter repos add my-vm org/main-repo
ssh boxcutter repos add my-vm org/submodule-repo
# Then inside the VM, git clone / submodule update will work
```

## After a Host Reboot

The `boxcutter-host` systemd service handles reboots automatically. No manual intervention needed. It recreates network devices, relaunches VMs from saved state, and monitors health.

## Upgrading

### Update the host binary

```bash
sudo boxcutter-host self-update
# or: sudo boxcutter-host self-update --version v0.4.0
```

### Rolling upgrade of VMs

```bash
sudo boxcutter-host upgrade all
# or individually:
sudo boxcutter-host upgrade node
sudo boxcutter-host upgrade orchestrator
```

### Full upgrade workflow

```bash
sudo boxcutter-host self-update           # 1. Update binary
sudo boxcutter-host upgrade all           # 2. Roll VMs to match
sudo boxcutter-host version               # 3. Verify
```

## State Recovery

If `cluster.json` is lost or corrupted, recover running VMs:

```bash
sudo boxcutter-host recover
sudo systemctl restart boxcutter-host
```

This scans `/proc` for running QEMU processes and rebuilds the state file. Nothing is stopped.

## Migration

**Firecracker VMs** are live-migrated between nodes during drain operations using snapshot/restore. The VM pauses, its memory is snapshotted, transferred to the target node, and resumed. Processes, memory, and Tailscale connections survive. Downtime is typically 1-10 seconds.

**QEMU VMs** do not support live migration. They can be relocated when stopped (stop → transfer files → start on new node). During node drains, running QEMU VMs are skipped.

## Troubleshooting

**Bootstrap fails pulling images:** Re-run `sudo boxcutter-host bootstrap` -- it picks up where it left off.

**VMs not starting after reboot:** Check `sudo journalctl -u boxcutter-host -f`. The daemon auto-restarts crashed VMs.

**Can't SSH to orchestrator:** Use the bridge IP directly: `ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2`

**Need to start over:** `sudo systemctl stop boxcutter-host && sudo pkill qemu-system-x86 && sudo rm /var/lib/boxcutter/cluster.json`, then re-run bootstrap.

## Development

For building from source, running tests, building images, and contributing, see [docs/development.md](docs/development.md).

## File Structure

```
boxcutter/
+-- host/                        # Physical host control plane
|   +-- cmd/host/main.go         # Control plane binary
|   +-- internal/                # bridge, cluster state, qemu, OCI
|   +-- provision.sh             # Cloud-init ISO generation
|   +-- build-image.sh           # Build VM base images (QCOW2)
+-- orchestrator/                # Orchestrator VM (Go)
|   +-- cmd/orchestrator/        # HTTP API server (:8801)
|   +-- cmd/ssh/                 # SSH ForceCommand binary
+-- node/                        # Node VM
|   +-- agent/                   # Node agent (Go) - VM lifecycle, migration
|   +-- golden/                  # Golden image (Firecracker rootfs)
|   +-- proxy/                   # MITM forward proxy (Go)
|   +-- vmid/                    # VM identity & token broker (Go)
+-- docs/                        # Documentation
+-- Makefile                     # Build targets
```
