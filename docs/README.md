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

```bash
make install-host
sudo env BOXCUTTER_REPO=$PWD boxcutter-host bootstrap
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

### Upgrading a running cluster

```bash
# Pull latest images from OCI and rolling-upgrade all VMs
sudo boxcutter-host upgrade all

# Upgrade just nodes (rolls out new node, drains old, zero VM downtime)
sudo boxcutter-host upgrade node

# Upgrade just the orchestrator (migrates state to new instance)
sudo boxcutter-host upgrade orchestrator

# Update the golden image (nodes pull via MQTT)
sudo boxcutter-host upgrade golden
```

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
