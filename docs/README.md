# Boxcutter

Ephemeral dev environments on bare metal. Spin up a VM in under a second.

Boxcutter runs [Firecracker](https://firecracker-microvm.github.io/) microVMs inside a QEMU/KVM Node VM on a physical Linux machine. Each microVM joins [Tailscale](https://tailscale.com/) automatically, making it accessible from anywhere on your tailnet — not just your LAN.

```
$ ssh boxcutter new

Creating VM: bold-fox (4 vCPU, 8GB RAM, 50G disk, mode: normal)
  Creating copy-on-write snapshot...
  Injecting CA cert...
  VM created: bold-fox (mark: 41022, mode: normal)
Starting VM: bold-fox (mark: 41022, mode: normal)
  VM started (PID 12847)
  Waiting for Tailscale...
  Tailscale IP: 100.64.1.42

VM ready: bold-fox
Connect: ssh 100.64.1.42

$ ssh bold-fox
dev@bold-fox:~$
```

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  Physical Host (Ubuntu 24.04)                                │
│                                                              │
│  enp34s0 (physical NIC) ──→ internet (NAT)                   │
│  tap-node0 (192.168.50.1/30) ──→ Node VM                    │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Node VM (QEMU/KVM) — 192.168.50.2                    │  │
│  │  Tailscale: 100.x.x.x (boxcutter)                     │  │
│  │                                                        │  │
│  │  Per-VM isolated TAP links (no shared bridge):         │  │
│  │    tap-bold-fox   10.0.0.1 ↔ 10.0.0.2  mark: 41022   │  │
│  │    tap-calm-otter 10.0.0.1 ↔ 10.0.0.2  mark: 8193    │  │
│  │    tap-wild-heron 10.0.0.1 ↔ 10.0.0.2  mark: 55471   │  │
│  │                                                        │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐             │  │
│  │  │bold-fox  │  │calm-otter│  │wild-heron│             │  │
│  │  │Firecrackr│  │Firecrackr│  │Firecrackr│             │  │
│  │  │4cpu/8GB  │  │4cpu/8GB  │  │4cpu/8GB  │             │  │
│  │  │10.0.0.2  │  │10.0.0.2  │  │10.0.0.2  │             │  │
│  │  │TS:100.x  │  │TS:100.x  │  │TS:100.x  │             │  │
│  │  └──────────┘  └──────────┘  └──────────┘             │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
         │
    Internet (via host NAT)
    Tailscale overlay network
```

There are three layers:

1. **Physical host** — runs QEMU, provides NAT to internet
2. **Node VM** — Ubuntu VM that runs Firecracker, manages microVMs, joins Tailscale
3. **Firecracker microVMs** — lightweight VMs where developers work, each joins Tailscale

The Node VM exists because Firecracker requires KVM and a Linux host with specific kernel features. Running it in a QEMU VM with nested virtualization isolates all Firecracker state from the physical host.

## How it works

### Networking: per-TAP fwmark routing

Every VM gets the same IP address (10.0.0.2) on an isolated point-to-point TAP link. There is no shared bridge — each VM has its own TAP device with 10.0.0.1 on the Node side and 10.0.0.2 on the VM side. Linux fwmark-based policy routing directs return traffic to the correct TAP.

Each VM is assigned a unique integer "mark" derived from CRC32 of its name. When packets arrive on a TAP, iptables marks them. CONNMARK saves the mark to conntrack so return traffic can be routed back to the correct TAP.

VMs are completely isolated from each other — there is no shared Layer 2 domain and no route between TAPs. VM-to-VM communication goes through Tailscale (if needed), subject to your tailnet's ACL policies.

For full networking details, see [docs/network-architecture.md](network-architecture.md).

Firecracker VMs get their network configuration via the kernel `ip=` boot parameter:

```
ip=10.0.0.2::10.0.0.1:255.255.255.252:bold-fox:eth0:off:8.8.8.8
```

This provides networking before init even starts — zero DHCP wait, instant connectivity.

### VM provisioning modes

VMs can be created in **normal** or **paranoid** mode:

- **Normal** — full direct internet access, real credentials from the token broker
- **Paranoid** — no direct internet; all traffic must go through the MITM forward proxy; credentials are wrapped in one-time sentinel tokens that the proxy swaps for real credentials on the fly

```bash
ssh boxcutter create my-vm --mode paranoid
```

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

### Name resolution: Tailscale MagicDNS

Each VM registers with Tailscale using its name as the hostname. With MagicDNS enabled on your tailnet, you can `ssh bold-fox` from any device on the tailnet.

### Accessing VM services

VMs are accessible via their Tailscale IPs, so any service running in a VM is directly reachable at `http://<tailscale-ip>:<port>` from any device on your tailnet. With MagicDNS, you can also use `http://bold-fox:<port>`.

### VM names

VMs get randomly generated adjective-animal names: `bold-fox`, `calm-otter`, `wild-heron`. Names are guaranteed unique across active VMs.

## Requirements

### Hardware

- **CPU:** x86_64 processor with hardware virtualization (Intel VT-x or AMD-V). Nested virtualization must be supported and enabled — Firecracker runs inside a QEMU VM and needs KVM access.
- **RAM:** 16 GB minimum. The Node VM and its microVMs share this pool. Each microVM defaults to 8GB, so more RAM = more simultaneous VMs. A 64GB machine comfortably runs 5-6 VMs with room to spare.
- **Disk:** 50 GB minimum. The golden image is ~4GB, and each VM's COW overlay starts small but grows with use. 150GB+ recommended if you expect many VMs with large workloads.
- **Network:** Internet connectivity (for Tailscale). Unlike the previous bridged architecture, Wi-Fi works fine since VMs connect via Tailscale overlay, not LAN bridging.

### Software

- **Ubuntu 24.04** on the physical host (tested; other Debian-based distros likely work)
- **sudo access** on the host machine
- **SSH key** on the host (setup will generate one if you don't have one)
- **Tailscale account** with a reusable auth key

### Tailscale setup

1. Create a Tailscale account at https://tailscale.com/
2. Generate an auth key at https://login.tailscale.com/admin/settings/keys
   - Check **"Reusable"** — each VM uses this key to join
   - Check **"Ephemeral"** — nodes auto-remove from the tailnet when they disconnect (perfect for disposable VMs)
   - The key expires after 90 days; you'll need to rotate it
3. Enable MagicDNS at https://login.tailscale.com/admin/dns (optional but recommended)

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

# Network — internal (Tailscale handles external access)
HOST_INTERFACE=enp34s0  # Your physical NIC (run: ip link show)
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
- Create a TAP device with NAT for the Node VM

### Step 3: Launch the Node VM

```bash
make launch          # Foreground (see console output, Ctrl-A X to quit)
# or
make launch-daemon   # Background (logs to .images/node-console.log)
```

The first boot takes 3-5 minutes. Cloud-init will:
1. Update packages
2. Mount the boxcutter repo into the VM via 9p
3. Run `install.sh` which installs Firecracker, Tailscale, Caddy, networking, TLS certificates, vmid, the forward proxy, and all management scripts

Wait for the console to show the login prompt (foreground) or check the log:
```bash
tail -f .images/node-console.log    # If running as daemon
```

### Step 4: Configure Tailscale

SSH into the Node VM and set up Tailscale:

```bash
ssh ubuntu@192.168.50.2

# Join the Node VM to Tailscale (this is a persistent node, not ephemeral)
sudo tailscale up --hostname=boxcutter

# Place the ephemeral VM auth key (used to provision disposable VMs)
echo 'tskey-auth-XXXXXXX' | sudo tee /etc/boxcutter/tailscale-authkey
```

The Node VM joins Tailscale interactively (it's a persistent node, not ephemeral). The ephemeral auth key at `/etc/boxcutter/tailscale-authkey` is only used for VMs — they auto-remove from the tailnet when destroyed. The key never touches VM disk images; the Node VM SSHes into each VM over the internal network to run `tailscale up`.

Once the Node VM is on Tailscale, you can SSH to it via its Tailscale IP or MagicDNS name (`boxcutter`) from any device on your tailnet.

### Step 5: Build the golden image

```bash
ssh ubuntu@192.168.50.2   # or ssh boxcutter (via Tailscale)

# Phase 1: Create minimal Ubuntu rootfs with debootstrap (~3 minutes)
sudo boxcutter-ctl golden build

# Phase 2: Boot as VM, install dev tools via SSH (~5 minutes)
sudo boxcutter-ctl golden provision
```

Phase 1 creates a minimal Ubuntu Noble system with SSH, systemd, Tailscale (client only — no auth key), and networking.

Phase 2 boots that image as a temporary Firecracker VM and runs the provision script inside it. This installs build-essential, mise (with Node 22 and Ruby 3.2), GitHub CLI, Claude Code, and other dev tools. The changes are then merged back into the golden image.

### Step 6: Add users

From any device on your tailnet:

```bash
ssh boxcutter adduser <github-username>
```

This fetches their public SSH keys from `github.com/<username>.keys` and adds them to:
- The Node VM's control interface (so they can create/list/destroy VMs)
- The golden image's authorized_keys (so they can SSH into VMs)

The user can then, from any device on the tailnet:
```bash
ssh boxcutter new      # Create a VM
ssh boxcutter list     # See their VMs
```

### Step 7: Verify

```bash
# Create a test VM
ssh boxcutter new

# You should see something like:
#   VM ready: bold-fox
#   Connect: ssh 100.64.1.42

# SSH into it (via Tailscale IP)
ssh 100.64.1.42

# Or by MagicDNS name (if MagicDNS is enabled)
ssh bold-fox
```

### After a host reboot

The TAP device and Node VM do not persist across host reboots. To restart:

```bash
cd ~/boxcutter
make setup         # Re-create the TAP + NAT (idempotent — skips what exists)
make launch-daemon # Re-launch the Node VM
```

VMs that were running before the reboot will be in a stopped state. Start them with:
```bash
ssh boxcutter start <vm-name>
```

## Usage

All commands go through SSH to the Node VM (via Tailscale or internal IP):

```bash
ssh boxcutter new                        # Create and start a new VM (normal mode)
ssh boxcutter create <name> --mode paranoid  # Create a paranoid-mode VM
ssh boxcutter list                       # List all VMs (with marks, modes, Tailscale IPs)
ssh boxcutter start <name>               # Start a stopped VM
ssh boxcutter stop <name>                # Stop a running VM
ssh boxcutter destroy <name>             # Destroy a VM (auto-removed from Tailscale)
ssh boxcutter status                     # Host capacity summary
ssh boxcutter help                       # Show all commands
```

Once a VM is running, SSH directly to it via Tailscale:

```bash
ssh 100.64.1.42          # By Tailscale IP
ssh bold-fox             # By MagicDNS name
```

## Node VM services

| Service | Port | Description |
|---------|------|-------------|
| vmid | :80 | VM identity & token broker (fwmark-based identification) |
| boxcutter-proxy | :8080 | MITM forward proxy (sentinel token swapping) |
| derper | :443 | Local Tailscale DERP relay |
| Caddy | :8880/:8443 | Reverse proxy |

## File structure

```
boxcutter/
├── host/                    # Physical host scripts
│   ├── boxcutter.env        # Network and resource configuration
│   ├── setup.sh             # Install QEMU, create TAP + NAT
│   ├── launch.sh            # Start the Node VM
│   ├── stop.sh              # Stop the Node VM
│   └── ssh.sh               # Quick SSH into Node VM
├── scripts/                 # Installed into the Node VM
│   ├── boxcutter-ctl        # VM lifecycle manager (create/start/stop/destroy)
│   ├── boxcutter-ssh        # SSH ForceCommand dispatch (control interface)
│   ├── boxcutter-net        # Per-TAP fwmark routing infrastructure
│   ├── boxcutter-tls        # Internal CA + leaf cert generation
│   └── boxcutter-names      # Random adjective-animal name generator
├── vmid/                    # VM identity & token broker (Go)
│   ├── cmd/vmid/main.go     # Mark-aware listener (SO_MARK)
│   └── internal/            # Registry, sentinel store, middleware, API
├── proxy/                   # Forward proxy (Go)
│   └── cmd/proxy/main.go    # MITM proxy with sentinel swapping
├── golden/                  # Golden image build
│   ├── build.sh             # Phase 1: debootstrap minimal Ubuntu rootfs
│   ├── provision.sh         # Phase 2: install dev tools (node, ruby, gh, etc.)
│   └── nss_catchall.c       # NSS module for any-username SSH
├── cloud-init/              # Node VM cloud-init config
│   ├── user-data
│   ├── meta-data
│   └── network-config
├── config/
│   └── Caddyfile            # Caddy base config (ports 8880/8443)
├── systemd/                 # Systemd unit files for Node VM services
│   ├── boxcutter-net.service
│   ├── vmid.service
│   ├── boxcutter-proxy.service
│   └── boxcutter-derper.service
├── docs/
│   ├── README.md            # This file
│   └── network-architecture.md  # Detailed networking documentation
├── install.sh               # Node VM setup (runs inside the Node VM)
└── Makefile                 # Host-side convenience targets
```

## How the golden image is built

The golden image is a single ext4 file built in two phases:

**Phase 1 (`golden build`):** Runs `debootstrap` to create a minimal Ubuntu Noble rootfs. Configures serial console, SSH, static DNS, entropy seeding, hostname-from-kernel-cmdline service, Tailscale, and the NSS catchall module. No network manager — the kernel `ip=` parameter handles everything.

**Phase 2 (`golden provision`):** Boots the golden image as a temporary Firecracker VM, SSHes in, and runs `provision.sh` to install dev tools: build-essential, mise (with Node 22 and Ruby 3.2), GitHub CLI, Claude Code, tmux, htop, etc. The COW snapshot is then merged back into the golden image.

## Security model

- All SSH users on the Node VM (except `ubuntu` and `root`) have `ForceCommand` — they can only run `boxcutter-ssh`, never get a shell
- The `ubuntu` user retains full admin access for maintenance via `ssh ubuntu@<host>`
- VM users share the `dev` account (uid 1000) — isolation is at the VM level, not the user level
- SSH keys are the only authentication method (password auth is disabled for SSH)
- Each VM is a full Firecracker microVM with its own kernel — stronger isolation than containers
- VMs are isolated on separate TAP devices — no shared L2 domain, no VM-to-VM communication
- External access is via Tailscale only, subject to your tailnet's ACL policies
- The Tailscale auth key lives only on the Node VM (`/etc/boxcutter/tailscale-authkey`) — it is never stored on VM disk images
- The auth key should be **ephemeral** — destroyed VMs auto-remove from the tailnet when they disconnect
- Internal CA provides TLS for proxy MITM; CA cert is per-VM injected, not baked into the golden image
- Paranoid mode VMs have no direct internet access — all traffic goes through the inspectable forward proxy
- Sentinel tokens ensure real credentials never touch VM disk or memory in paranoid mode

## Limitations

- x86_64 only (Firecracker + nested KVM)
- No persistent storage across VM destruction (by design — VMs are ephemeral)
- Tailscale auth key must be rotated every 90 days
- VM creation takes ~5-10 seconds longer than before due to Tailscale join time
- Host TAP setup is not persistent across physical host reboots (re-run `make setup`)
