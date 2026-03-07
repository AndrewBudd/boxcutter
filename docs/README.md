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
- **Network:** Internet connectivity for Tailscale

### Software

- Ubuntu 24.04 on the physical host
- sudo access
- SSH key (`~/.ssh/id_rsa` or `~/.ssh/id_ed25519`)
- Tailscale account with reusable+ephemeral auth key
- Go 1.22+ (for building Go services)

### Verify KVM + nested virtualization

```bash
ls /dev/kvm

# AMD:
cat /sys/module/kvm_amd/parameters/nested    # Should say 1 or Y
# Intel:
cat /sys/module/kvm_intel/parameters/nested
```

## Installation

### Step 1: Clone and configure

```bash
git clone <repo-url> ~/boxcutter
cd ~/boxcutter
```

Edit `host/boxcutter.env`:

```bash
HOST_INTERFACE=enp34s0    # Your physical NIC (ip route | grep default)
NODE_VCPU=6               # vCPUs per node VM
NODE_RAM=12G              # RAM per node VM
NODE_DISK=150G            # Disk per node VM
```

### Step 2: Set up secrets

```bash
mkdir -p ~/.boxcutter/secrets

# Tailscale auth keys (reusable + ephemeral)
echo 'tskey-auth-XXXXX' > ~/.boxcutter/secrets/tailscale-node-authkey
echo 'tskey-auth-YYYYY' > ~/.boxcutter/secrets/tailscale-vm-authkey

# Generate cluster SSH key (shared across all nodes)
ssh-keygen -t ed25519 -f ~/.boxcutter/secrets/cluster-ssh.key -N ""
```

### Step 3: Build and install the control plane

```bash
make build-host
make install-host
# -> Installs /usr/local/bin/boxcutter-host + systemd service
```

### Step 4: Provision VMs

```bash
# Provision the orchestrator VM (builds binaries, creates disk + cloud-init ISO)
make provision-orchestrator

# Provision a node VM
make provision-node
```

### Step 5: Bootstrap the cluster

```bash
sudo boxcutter-host bootstrap
```

This will:
- Create the bridge device (`br-boxcutter`) and NAT rules
- Create TAP devices for each VM
- Launch the orchestrator and node-1 QEMU VMs
- Save state to `/var/lib/boxcutter/cluster.json`

### Step 6: Start the control plane service

```bash
sudo systemctl enable --now boxcutter-host
```

The service will:
- Recreate bridge/TAPs on every boot (idempotent)
- Launch all VMs from `cluster.json`
- Monitor VM health (restart crashed VMs)
- Auto-scale nodes when capacity exceeds 80%
- Never scale if host has less than 8GB free memory

### Step 7: Wait for VMs to boot and verify

```bash
# Check control plane status
sudo boxcutter-host status

# Or via the unix socket API
sudo curl -s --unix-socket /run/boxcutter-host.sock http://localhost/status | python3 -m json.tool

# Wait for node to register with orchestrator, then test SSH interface
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 status
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 nodes
```

### Step 8: Add users and create VMs

```bash
# Add SSH keys from GitHub
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 adduser <github-username>

# Create a VM
ssh -i ~/.ssh/id_rsa boxcutter@192.168.50.2 new

# Or via Tailscale once the orchestrator has joined
ssh boxcutter new
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
ssh boxcutter status               # Cluster capacity summary
ssh boxcutter nodes                # List all nodes with health
ssh boxcutter adduser <github>     # Add SSH keys from GitHub
ssh boxcutter removeuser <github>  # Remove SSH keys
ssh boxcutter keys                 # List configured SSH keys
```

## After a host reboot

The `boxcutter-host` systemd service handles reboots automatically:

1. Recreates the bridge and TAP devices
2. Relaunches all QEMU VMs from `cluster.json`
3. Monitors health and restarts any that fail to come up

No manual intervention needed.

## Migration

Firecracker VMs can be live-migrated between nodes using snapshot/restore. The VM pauses (not stops), its memory and state are snapshotted, transferred to the target node, and resumed. Processes, memory, and Tailscale connections survive. Downtime is ~10s for a 2GB VM.

Migration is coordinated by the control plane during drain operations. Users cannot directly trigger migrations.

After migration, a vsock-based nudge triggers `tailscale netcheck` inside the VM to re-establish network paths through the new node.

## File structure

```
boxcutter/
+-- host/                        # Physical host
|   +-- boxcutter.env            # Network and resource configuration
|   +-- boxcutter-host.service   # systemd unit for control plane
|   +-- cmd/host/main.go         # Control plane binary
|   +-- internal/                # bridge, cluster state, qemu management
|   +-- setup.sh                 # Manual bridge/TAP setup (superseded by boxcutter-host)
|   +-- launch.sh                # Manual VM launch (superseded by boxcutter-host)
|   +-- provision.sh             # Provision orchestrator/node VMs
+-- orchestrator/                # Orchestrator VM (Go)
|   +-- cmd/orchestrator/        # HTTP API server
|   +-- cmd/ssh/                 # SSH ForceCommand binary
|   +-- internal/                # api, db, scheduler, node client
+-- node/                        # Node VM
|   +-- agent/                   # Node agent (Go) - VM lifecycle, migration
|   |   +-- cmd/node/            # HTTP API server (:8800)
|   |   +-- internal/            # vm manager, fcapi, networking
|   +-- golden/                  # Golden image build
|   |   +-- build.sh             # Phase 1: debootstrap rootfs
|   |   +-- provision.sh         # Phase 2: install dev tools
|   |   +-- nss_catchall.c       # NSS module for any-username SSH
|   |   +-- vsock_listen.c       # vsock listener for migration nudge
|   +-- proxy/                   # MITM forward proxy (Go)
|   +-- vmid/                    # VM identity & token broker (Go)
+-- docs/                        # Documentation
+-- Makefile                     # Build and management targets
```
