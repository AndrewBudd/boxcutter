# Boxcutter

Ephemeral dev environments on bare metal. Spin up a VM in under a second.

Boxcutter runs [Firecracker](https://firecracker-microvm.github.io/) microVMs inside a QEMU/KVM Node VM on a physical Linux machine. Each microVM gets a real LAN IP, an mDNS hostname, and is directly SSH-accessible with any username.

```
$ ssh 192.168.2.100 new

Creating VM: bold-fox (4 vCPU, 8GB RAM, 50G disk)
  Creating copy-on-write snapshot...
  VM created: bold-fox (IP: 192.168.2.200)
Starting VM: bold-fox (192.168.2.200)
  VM started (PID 12847)

VM ready: bold-fox
Connect: ssh 192.168.2.200

$ ssh bold-fox.local
dev@bold-fox:~$
```

## Architecture

```
┌─────────────────────────────────────────────────────┐
│  Physical Host (Ubuntu 24.04)                       │
│                                                     │
│  ┌───────────────────────────────────────────────┐  │
│  │  br0 bridge (192.168.2.124)                   │  │
│  │  ├── enp34s0 (physical NIC)                   │  │
│  │  └── tap-node0                                │  │
│  └───────────────────────────────────────────────┘  │
│           │                                         │
│  ┌────────┴──────────────────────────────────────┐  │
│  │  Node VM (QEMU/KVM) — 192.168.2.100           │  │
│  │                                               │  │
│  │  ┌─────────────────────────────────────────┐  │  │
│  │  │  brvm0 bridge                           │  │  │
│  │  │  ├── ens3 (virtio NIC → tap-node0)      │  │  │
│  │  │  ├── tap-bold-fox   ──→  192.168.2.200  │  │  │
│  │  │  ├── tap-calm-otter ──→  192.168.2.201  │  │  │
│  │  │  └── tap-wild-heron ──→  192.168.2.202  │  │  │
│  │  └─────────────────────────────────────────┘  │  │
│  │                                               │  │
│  │  ┌──────────┐ ┌──────────┐ ┌──────────┐      │  │
│  │  │bold-fox  │ │calm-otter│ │wild-heron│      │  │
│  │  │Firecrackr│ │Firecrackr│ │Firecrackr│      │  │
│  │  │4cpu/8GB  │ │4cpu/8GB  │ │4cpu/8GB  │      │  │
│  │  └──────────┘ └──────────┘ └──────────┘      │  │
│  └───────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────┘
          │
     LAN / Router (192.168.2.1)
```

There are three layers:

1. **Physical host** — runs QEMU, hosts the bridge to LAN
2. **Node VM** — Ubuntu VM that runs Firecracker, manages microVMs
3. **Firecracker microVMs** — lightweight VMs where developers work

The Node VM exists because Firecracker requires KVM and a Linux host with specific kernel features. Running it in a QEMU VM with nested virtualization isolates all Firecracker state from the physical host.

## How it works

### Networking: LAN bridging with kernel ip=

Every VM gets a real IP on your LAN. No NAT, no port forwarding, no DHCP.

The physical host creates a bridge (`br0`) that includes the physical NIC and a TAP device for the Node VM. Inside the Node VM, a second bridge (`brvm0`) connects the Node VM's NIC with TAP devices for each Firecracker microVM.

Firecracker VMs get their IP configuration via the kernel `ip=` boot parameter:

```
ip=192.168.2.200::192.168.2.1:255.255.255.0:bold-fox:eth0:off:8.8.8.8
```

This provides networking before init even starts — zero DHCP wait, instant connectivity.

### Storage: device-mapper COW snapshots

VM creation takes ~0.25 seconds regardless of golden image size.

Instead of copying the full golden rootfs (several GB), Boxcutter uses Linux device-mapper snapshots. Each VM gets a sparse COW overlay file that only stores blocks that differ from the golden image:

```
Golden rootfs (read-only)  ←──  dm-snapshot  ──→  COW overlay (per-VM)
    /dev/loop0                  /dev/mapper/bc-bold-fox    /dev/loop1
```

A new VM starts with a near-zero-size COW file that grows only as the VM writes to disk.

### SSH: accept any username

You don't need to remember usernames. SSH to any Boxcutter host or VM with whatever username your SSH client defaults to.

This is implemented with two components:

1. **NSS catchall module** (`libnss_catchall.so.2`) — a small C library that plugs into Linux's Name Service Switch. Any username not in a hardcoded system-user list resolves to the target user (uid 1000 `dev` in VMs, uid 995 `boxcutter` on the Node VM).

2. **AuthorizedKeysCommand** — sshd is configured to serve the same `authorized_keys` file for every user, so key-based auth works regardless of the username.

The NSS module provides both `passwd` and `shadow` entries so PAM authentication succeeds for synthetic users.

### Name resolution: mDNS via Avahi

Each VM runs Avahi and sets its hostname from the kernel `ip=` parameter at boot via a systemd oneshot service. VMs are discoverable as `<name>.local` on the LAN (if your network passes multicast).

### Reverse proxy: Caddy with service discovery

VMs can declare services in `~/.services`:

```
myapp=3000
api=8080
```

A polling service (`boxcutter-proxy-sync`) SSHes into running VMs, reads their `.services` files, and generates Caddy reverse proxy configs. Services are exposed as `https://<service>.<vm-name>.vm.lan` with auto-provisioned TLS certificates.

### VM names

VMs get randomly generated adjective-animal names: `bold-fox`, `calm-otter`, `wild-heron`. Names are guaranteed unique across active VMs.

## Requirements

### Hardware

- **CPU:** x86_64 processor with hardware virtualization (Intel VT-x or AMD-V). Nested virtualization must be supported and enabled — Firecracker runs inside a QEMU VM and needs KVM access.
- **RAM:** 16 GB minimum. The Node VM and its microVMs share this pool. Each microVM defaults to 8GB, so more RAM = more simultaneous VMs. A 64GB machine comfortably runs 5-6 VMs with room to spare.
- **Disk:** 50 GB minimum. The golden image is ~4GB, and each VM's COW overlay starts small but grows with use. 150GB+ recommended if you expect many VMs with large workloads.
- **Network:** Ethernet connection to a LAN. Boxcutter bridges VMs onto your network — they need real LAN IPs. Wi-Fi won't work for the bridge (Linux can't bridge Wi-Fi interfaces in managed mode).

### Software

- **Ubuntu 24.04** on the physical host (tested; other Debian-based distros likely work)
- **sudo access** on the host machine
- **SSH key** on the host (setup will generate one if you don't have one)

### Verify KVM support

```bash
# Check that KVM is available
ls /dev/kvm

# Check nested virtualization is enabled
# For Intel:
cat /sys/module/kvm_intel/parameters/nested    # Should say Y or 1
# For AMD:
cat /sys/module/kvm_amd/parameters/nested      # Should say Y or 1
```

If nested virtualization is not enabled:

```bash
# For Intel:
sudo modprobe -r kvm_intel
sudo modprobe kvm_intel nested=1
echo "options kvm_intel nested=1" | sudo tee /etc/modprobe.d/kvm-nested.conf

# For AMD:
sudo modprobe -r kvm_amd
sudo modprobe kvm_amd nested=1
echo "options kvm_amd nested=1" | sudo tee /etc/modprobe.d/kvm-nested.conf
```

### Network planning

You need to reserve a block of IP addresses on your LAN for VMs. These should be outside your router's DHCP range to avoid conflicts.

For example, if your LAN is `192.168.2.0/24`:
- Router: `192.168.2.1`
- Your host: `192.168.2.124` (whatever it currently is)
- Node VM: `192.168.2.100` (pick a free static IP)
- VM pool: `192.168.2.200` - `192.168.2.250` (51 VMs max)

Make sure your router's DHCP range doesn't overlap with the Node VM IP or the VM pool.

## Installation

### Step 1: Clone and configure

```bash
git clone <repo-url> ~/boxcutter
cd ~/boxcutter
```

Edit `host/boxcutter.env` for your environment:

```bash
# Resources allocated to the Node VM (adjust to your machine)
NODE_VCPU=12          # Number of vCPUs (leave 2-4 for the host)
NODE_RAM=48G          # RAM (leave 4-8GB for the host)
NODE_DISK=150G        # Disk size for the Node VM

# Network — must match your LAN
HOST_INTERFACE=enp34s0  # Your physical NIC (run: ip link show)
NODE_IP=192.168.2.100   # Static IP for the Node VM
LAN_GW=192.168.2.1      # Your router's IP
LAN_CIDR=24             # Subnet mask (24 = 255.255.255.0)
LAN_DNS="8.8.8.8"       # DNS server

# VM IP pool (must be outside your DHCP range)
VM_IP_PREFIX=192.168.2
VM_IP_START=200          # First VM gets .200
VM_IP_END=250            # Last possible VM gets .250
```

Find your physical NIC name:
```bash
ip route | grep default    # Look for the "dev <interface>" part
```

### Step 2: Host setup

```bash
make setup
```

This will:
- Install QEMU and dependencies (`qemu-system-x86`, `qemu-utils`, `genisoimage`)
- Download the Ubuntu 24.04 cloud image (~600MB)
- Create a QCOW2 disk for the Node VM (COW on the cloud image)
- Create a network bridge (`br0`) with your physical NIC
- Create a TAP device for the Node VM

**Warning:** The bridge setup temporarily disrupts network connectivity on the host (a few seconds) as it moves the host's IP from the physical NIC to the bridge. If you're SSH'd into the host, your session will briefly freeze but should recover.

### Step 3: Launch the Node VM

```bash
make launch          # Foreground (see console output, Ctrl-A X to quit)
# or
make launch-daemon   # Background (logs to .images/node-console.log)
```

The first boot takes 3-5 minutes. Cloud-init will:
1. Update packages
2. Mount the boxcutter repo into the VM via 9p
3. Run `install.sh` which installs Firecracker, Caddy, networking, and all management scripts

Wait for the console to show the login prompt (foreground) or check the log:
```bash
tail -f .images/node-console.log    # If running as daemon
```

You can verify the Node VM is ready:
```bash
ssh ubuntu@192.168.2.100    # Should work once cloud-init finishes
```

### Step 4: Build the golden image

SSH into the Node VM and build the rootfs that all microVMs will use:

```bash
ssh ubuntu@192.168.2.100

# Phase 1: Create minimal Ubuntu rootfs with debootstrap (~3 minutes)
sudo boxcutter-ctl golden build

# Phase 2: Boot as VM, install dev tools via SSH (~5 minutes)
sudo boxcutter-ctl golden provision
```

Phase 1 creates a minimal Ubuntu Noble system with SSH, systemd, Avahi, and networking.

Phase 2 boots that image as a temporary Firecracker VM and runs the provision script inside it. This installs build-essential, mise (with Node 22 and Ruby 3.2), GitHub CLI, Claude Code, and other dev tools. The changes are then merged back into the golden image.

### Step 5: Add users

From the machine that ran setup (which already has SSH access):

```bash
ssh 192.168.2.100 adduser <github-username>
```

This fetches their public SSH keys from `github.com/<username>.keys` and adds them to:
- The Node VM's control interface (so they can create/list/destroy VMs)
- The golden image's authorized_keys (so they can SSH into VMs)

The user can then, from any machine with their SSH key:
```bash
ssh 192.168.2.100 new      # Create a VM
ssh 192.168.2.100 list     # See their VMs
```

### Step 6: Verify

```bash
# Create a test VM
ssh 192.168.2.100 new

# You should see something like:
#   VM ready: bold-fox
#   Connect: ssh 192.168.2.200

# SSH into it
ssh 192.168.2.200

# Or by mDNS name (if your network supports multicast)
ssh bold-fox.local
```

### After a host reboot

The bridge and Node VM do not persist across host reboots. To restart:

```bash
cd ~/boxcutter
make setup         # Re-create the bridge (idempotent — skips what exists)
make launch-daemon # Re-launch the Node VM
```

VMs that were running before the reboot will be in a stopped state. Start them with:
```bash
ssh 192.168.2.100 start <vm-name>
```

## Usage

All commands go through SSH to the Node VM:

```bash
ssh 192.168.2.100 new              # Create and start a new VM
ssh 192.168.2.100 list             # List all VMs
ssh 192.168.2.100 start <name>     # Start a stopped VM
ssh 192.168.2.100 stop <name>      # Stop a running VM
ssh 192.168.2.100 destroy <name>   # Destroy a VM
ssh 192.168.2.100 status           # Host capacity summary
ssh 192.168.2.100 help             # Show all commands
```

Once a VM is running, SSH directly to it:

```bash
ssh 192.168.2.200          # By IP
ssh bold-fox.local         # By mDNS hostname (if your network supports it)
```

## File structure

```
boxcutter/
├── host/                    # Physical host scripts
│   ├── boxcutter.env        # Network and resource configuration
│   ├── setup.sh             # Install QEMU, create bridge + TAP
│   ├── launch.sh            # Start the Node VM
│   ├── stop.sh              # Stop the Node VM
│   └── ssh.sh               # Quick SSH into Node VM
├── scripts/                 # Installed into the Node VM
│   ├── boxcutter-ctl        # VM lifecycle manager (create/start/stop/destroy)
│   ├── boxcutter-ssh        # SSH ForceCommand dispatch (control interface)
│   ├── boxcutter-net        # Bridge network setup inside Node VM
│   ├── boxcutter-proxy-sync # Service discovery + Caddy config generator
│   ├── boxcutter-gateway    # Multi-host gateway dispatch (future)
│   └── boxcutter-names      # Random adjective-animal name generator
├── golden/                  # Golden image build
│   ├── build.sh             # Phase 1: debootstrap minimal Ubuntu rootfs
│   ├── provision.sh         # Phase 2: install dev tools (node, ruby, gh, etc.)
│   └── nss_catchall.c       # NSS module for any-username SSH
├── cloud-init/              # Node VM cloud-init config
│   ├── user-data
│   ├── meta-data
│   └── network-config
├── config/
│   └── Caddyfile            # Caddy base config (imports per-VM site configs)
├── systemd/                 # Systemd unit files for Node VM services
│   ├── boxcutter-net.service
│   └── boxcutter-proxy-sync.service
├── install.sh               # Node VM setup (runs inside the Node VM)
└── Makefile                 # Host-side convenience targets
```

## How the golden image is built

The golden image is a single ext4 file built in two phases:

**Phase 1 (`golden build`):** Runs `debootstrap` to create a minimal Ubuntu Noble rootfs. Configures serial console, SSH, static DNS, entropy seeding, hostname-from-kernel-cmdline service, Avahi for mDNS, and the NSS catchall module. No network manager — the kernel `ip=` parameter handles everything.

**Phase 2 (`golden provision`):** Boots the golden image as a temporary Firecracker VM, SSHes in, and runs `provision.sh` to install dev tools: build-essential, mise (with Node 22 and Ruby 3.2), GitHub CLI, Claude Code, tmux, htop, etc. The COW snapshot is then merged back into the golden image.

## Security model

- The `boxcutter` user on the Node VM has `ForceCommand` — it can only run `boxcutter-ssh`, never get a shell
- The `ubuntu` user retains full admin access for maintenance
- VM users share the `dev` account (uid 1000) — isolation is at the VM level, not the user level
- SSH keys are the only authentication method (password auth is disabled for SSH)
- Each VM is a full Firecracker microVM with its own kernel — stronger isolation than containers

## Limitations

- x86_64 only (Firecracker + nested KVM)
- mDNS depends on your network passing multicast traffic
- No persistent storage across VM destruction (by design — VMs are ephemeral)
- VM-to-VM networking goes through the LAN bridge (no isolated network)
- Host bridge setup is not persistent across physical host reboots (re-run `make setup`)
