# Boxcutter

Ephemeral dev environments on bare metal. Spin up a VM in under a second.

Boxcutter runs [Firecracker](https://firecracker-microvm.github.io/) microVMs inside QEMU/KVM node VMs on a physical Linux machine. Each microVM joins [Tailscale](https://tailscale.com/) automatically, making it accessible from anywhere on your tailnet.

```
$ ssh boxcutter new

  -> Trying boxcutter-node-1...
  Creating VM: bold-fox (2 vCPU, 2GB RAM, 50G disk, mode: normal)

  Name:    bold-fox
  Node:    boxcutter-node-1
  vCPU:    2
  RAM:     2G
  IP:      100.64.1.42
  Mode:    normal
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

Control plane (`boxcutter-host`) runs on bare metal and owns all infrastructure: bridge, TAPs, NAT, QEMU VM lifecycle, auto-scaling, and drain/migration coordination. The data plane (orchestrator + nodes) runs inside QEMU VMs and handles user-facing operations.

## Requirements

### Hardware

- **CPU:** x86_64 with hardware virtualization (Intel VT-x / AMD-V) and nested virtualization
- **RAM:** 32GB minimum (orchestrator 4G + node 12G + headroom). 64GB recommended.
- **Disk:** 50GB minimum. 150GB+ recommended.
- **Network:** Internet connectivity for Tailscale and OCI image pulls

### Software

- Ubuntu 24.04 on the physical host
- sudo access

### Verify KVM + nested virtualization

```bash
ls /dev/kvm

# AMD:
cat /sys/module/kvm_amd/parameters/nested    # Should say 1 or Y
# Intel:
cat /sys/module/kvm_intel/parameters/nested
```

## Quick Start

### Step 1: Install host dependencies

```bash
sudo apt install qemu-system-x86 qemu-utils genisoimage zstd mosquitto mosquitto-clients

# Go 1.22+
curl -sL https://go.dev/dl/go1.22.5.linux-amd64.tar.gz | sudo tar xz -C /usr/local
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# GitHub CLI (needed for pushing images to ghcr.io, not required for bootstrap)
# https://cli.github.com/
```

### Step 2: Clone and configure

```bash
git clone <repo-url> ~/boxcutter
cd ~/boxcutter
```

Edit `host/boxcutter.env` to match your host:

```bash
HOST_INTERFACE=enp34s0    # Your physical NIC (ip route | grep default)
NODE_VCPU=6               # vCPUs per node VM (adjust for your hardware)
NODE_RAM=12G              # RAM per node VM
NODE_DISK=150G            # Disk per node VM
```

### Step 3: Set up secrets

```bash
mkdir -p ~/.boxcutter/secrets
```

Create the following files:

| File | What it is | How to get it |
|------|-----------|---------------|
| `~/.boxcutter/secrets/tailscale-node-authkey` | Tailscale auth key for orchestrator + node VMs | [Tailscale admin](https://login.tailscale.com/admin/settings/keys) -- reusable key |
| `~/.boxcutter/secrets/tailscale-vm-authkey` | Tailscale auth key for Firecracker microVMs | Same -- reusable + ephemeral key |
| `~/.boxcutter/secrets/cluster-ssh.key` | SSH key for inter-node communication | `ssh-keygen -t ed25519 -f ~/.boxcutter/secrets/cluster-ssh.key -N ""` |
| `~/.boxcutter/secrets/authorized-keys` | SSH public keys for the boxcutter control interface | Your `~/.ssh/id_rsa.pub` or `id_ed25519.pub` |
| `~/.boxcutter/secrets/github-app.pem` | GitHub App private key (for repo cloning in VMs) | [GitHub App settings](https://github.com/settings/apps) |

You also need a config file at `~/.boxcutter/boxcutter.yaml`:

```yaml
node:
  hostname: HOSTNAME_PLACEHOLDER
  bridge_ip: BRIDGE_IP_PLACEHOLDER

orchestrator:
  url: ORCHESTRATOR_URL_PLACEHOLDER

github:
  enabled: true
  app_id: <your-app-id>
  installation_ids:
    - <your-installation-id>
  private_key_file: /etc/boxcutter/secrets/github-app.pem

tailscale:
  node_authkey_file: /etc/boxcutter/secrets/tailscale-node-authkey
  vm_authkey_file: /etc/boxcutter/secrets/tailscale-vm-authkey
```

The `PLACEHOLDER` values are templated automatically per-VM during provisioning.

### Step 4: Build, install, and bootstrap

**Option A: Install from deb (recommended for production)**

```bash
# Download latest stable release from GitHub Releases
sudo boxcutter-host self-update --version v0.2.0
# Or manually: wget + dpkg -i (see "Installation via Deb Package" above)

sudo boxcutter-host bootstrap --version v0.2.0
```

**Option B: Build from source (for development)**

```bash
make install-host
sudo env BOXCUTTER_REPO=$PWD boxcutter-host bootstrap --from-source
```

That's it. Bootstrap handles everything:

1. Sets up the bridge network (`br-boxcutter`) and NAT rules
2. Pulls pre-built VM images from `ghcr.io/andrewbudd/boxcutter` (with retry on failure)
3. Creates COW disks from the base images
4. Generates cloud-init ISOs (injects your secrets from `~/.boxcutter/`)
5. Launches the orchestrator and node-1 QEMU VMs
6. Waits for both to become healthy
7. Sets up the golden image (Firecracker rootfs) on the node

Bootstrap is fully idempotent -- if it fails partway through (e.g. network timeout during image pull), re-run it and it picks up where it left off.

### Step 5: Start the control plane service

```bash
sudo systemctl enable --now boxcutter-host
```

The service will:
- Recreate bridge/TAPs on every boot (idempotent)
- Launch all VMs from `cluster.json`
- Monitor VM health (restart crashed VMs)
- Auto-scale nodes when capacity exceeds 80%

### Step 6: Verify and start using

```bash
# Check cluster status
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 status
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 nodes

# Add your SSH keys
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 adduser <github-username>

# Create a VM
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 new

# Or via Tailscale (once orchestrator has joined your tailnet)
ssh boxcutter new
ssh boxcutter list
```

## Usage

All user commands go through the orchestrator SSH interface:

```bash
ssh boxcutter new [options]        # Create a new VM
  --clone <repo>                   #   Clone repo on creation
  --vcpu <N>                       #   CPU cores (default: 2)
  --ram <MiB>                      #   RAM in MiB (default: 2048)
  --disk <size>                    #   Disk size (default: 50G)
  --mode normal|paranoid           #   Network mode (default: normal)
  --node <node-id>                 #   Pin to specific node
ssh boxcutter list                 # List all VMs
ssh boxcutter destroy <name>       # Destroy a VM
ssh boxcutter stop <name>          # Stop a running VM
ssh boxcutter start <name>         # Start a stopped VM
ssh boxcutter cp <name> [new-name] # Clone a VM's disk
ssh boxcutter status               # Cluster capacity summary
ssh boxcutter nodes                # List all nodes with health
ssh boxcutter images               # List golden images on nodes
ssh boxcutter adduser <github>     # Add SSH keys from GitHub
ssh boxcutter removeuser <github>  # Remove SSH keys
ssh boxcutter keys                 # List configured SSH keys
```

## After a Host Reboot

The `boxcutter-host` systemd service handles reboots automatically:

1. Recreates the bridge and TAP devices
2. Relaunches all QEMU VMs from `cluster.json`
3. Monitors health and restarts any that fail to come up

No manual intervention needed.

## Installation via Deb Package

Instead of building from source, you can install the pre-built deb package from GitHub Releases:

```bash
# Download the latest release
wget https://github.com/AndrewBudd/boxcutter/releases/download/v0.2.0/boxcutter-host_0.2.0_amd64.deb

# Install
sudo dpkg -i boxcutter-host_0.2.0_amd64.deb

# Bootstrap the cluster
sudo boxcutter-host bootstrap --version v0.2.0
```

The deb package installs:
- `/usr/local/bin/boxcutter-host` -- control plane binary
- `/etc/systemd/system/boxcutter-host.service` -- systemd unit
- `/usr/share/boxcutter/` -- provisioning scripts, cloud-init templates, config
- `/var/lib/boxcutter/.images/` -- VM image storage

## Releases and Upgrades

### Release Model

Every tagged push (`v*`) triggers CI which:

1. Builds node and orchestrator VM images (QCOW2) and pushes them to `ghcr.io`
2. Builds the `boxcutter-host` binary, tarball, and deb package
3. Creates a GitHub Release marked as **pre-release**

Releases start as pre-release so you can test before promoting to stable. OCI images are tagged only with their version (e.g., `v0.3.0`) -- there is no auto-updated `latest` tag.

### Promoting a Release

After testing a pre-release, mark it as stable:

```bash
gh release edit v0.3.0 --prerelease=false
```

Only stable (non-prerelease) releases are picked up by `self-update`.

### Upgrading boxcutter-host

The `self-update` command downloads and installs the latest stable deb from GitHub Releases:

```bash
# Update to the latest stable release
sudo boxcutter-host self-update

# Update to a specific version (even pre-release)
sudo boxcutter-host self-update --version v0.3.0
```

This downloads the deb, installs it via `dpkg -i`, and restarts the systemd service.

### Upgrading VM Images

After updating the binary, upgrade the running VMs:

```bash
# Rolling upgrade of all VMs (pulls new images from ghcr.io)
sudo boxcutter-host upgrade all --tag v0.3.0

# Or upgrade individually
sudo boxcutter-host upgrade node --tag v0.3.0
sudo boxcutter-host upgrade orchestrator --tag v0.3.0

# Update the golden image (Firecracker rootfs, nodes pull via MQTT)
sudo boxcutter-host upgrade golden --tag v0.3.0
```

If `--tag` is omitted, the binary's own version is used (set at build time).

### Full Upgrade Workflow

```bash
# 1. Update the host binary
sudo boxcutter-host self-update

# 2. Upgrade the VMs to match
sudo boxcutter-host upgrade all

# 3. Verify
sudo boxcutter-host version
```

## Image Build and Publish

VM base images are distributed as OCI artifacts via `ghcr.io`. The images are public -- anyone can pull without authentication.

### Building and publishing images

```bash
# Build + push both node and orchestrator images
make publish-all

# Build + push a single image type
make publish TYPE=node
make publish TYPE=orchestrator

# Build only (no push)
make build-image TYPE=node

# Push a previously built image
./host/publish-image.sh node --push-only

# Custom tag (default: git short hash)
./host/publish-image.sh node --tag v1.0
```

Pushing requires `gh auth login` (GitHub CLI authentication).

## State Recovery

If `cluster.json` is lost or corrupted (e.g., after replacing the `boxcutter-host` binary or reinstalling), the `recover` command scans `/proc` for running QEMU processes and reconstructs the cluster state:

```bash
sudo boxcutter-host recover
```

This finds all running QEMU VMs, extracts their identity (disk path, TAP device, MAC address, CPU/RAM) from the process command line, and rebuilds `cluster.json`. Existing VMs are preserved -- nothing is stopped or restarted.

After recovery, start the daemon as usual:

```bash
sudo systemctl restart boxcutter-host
```

The daemon also runs recovery automatically on startup before launching any VMs, so already-running VMs from a previous session are adopted rather than duplicated.

## Migration

Firecracker VMs can be live-migrated between nodes using snapshot/restore. The VM pauses (not stops), its memory and state are snapshotted, transferred to the target node, and resumed. Processes, memory, and Tailscale connections survive. Downtime is ~10s for a 2GB VM.

Migration is coordinated by the control plane during drain operations. Users cannot directly trigger migrations.

After migration, a vsock-based nudge triggers `tailscale netcheck` inside the VM to re-establish network paths through the new node.

## File Structure

```
boxcutter/
+-- host/                        # Physical host control plane
|   +-- boxcutter.env            # Network and resource configuration
|   +-- boxcutter-host.service   # systemd unit
|   +-- cmd/host/main.go         # Control plane binary
|   +-- internal/                # bridge, cluster state, qemu, OCI
|   +-- provision.sh             # Cloud-init ISO generation
|   +-- build-image.sh           # Build VM base images (QCOW2)
|   +-- publish-image.sh         # Build + push images to ghcr.io
|   +-- mosquitto.conf           # MQTT broker config (golden image distribution)
+-- orchestrator/                # Orchestrator VM (Go)
|   +-- cmd/orchestrator/        # HTTP API server (:8801)
|   +-- cmd/ssh/                 # SSH ForceCommand binary
|   +-- internal/                # api, db, mqtt, scheduler
+-- node/                        # Node VM
|   +-- agent/                   # Node agent (Go) - VM lifecycle, migration
|   |   +-- cmd/node/            # HTTP API server (:8800)
|   |   +-- internal/            # vm manager, fcapi, networking, mqtt, golden
|   +-- golden/                  # Golden image (Firecracker rootfs)
|   |   +-- Dockerfile           # Golden image definition
|   |   +-- docker-to-ext4.sh    # Build Dockerfile -> ext4 rootfs
|   |   +-- nss_catchall.c       # NSS module for any-username SSH
|   |   +-- vsock_listen.c       # vsock listener for migration nudge
|   +-- proxy/                   # MITM forward proxy (Go)
|   +-- vmid/                    # VM identity & token broker (Go)
+-- docs/                        # Documentation
+-- Makefile                     # Build and management targets
```
