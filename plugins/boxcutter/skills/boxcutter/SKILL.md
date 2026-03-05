---
name: boxcutter
description: >
  Manage ephemeral dev environment VMs using Boxcutter. Boxcutter runs Firecracker
  microVMs that boot in ~1 second with real LAN IPs and SSH access. Use this skill
  whenever the user needs a fresh development environment, wants to test something
  in a clean VM, needs an isolated machine for running builds or scripts, wants to
  spin up a throwaway environment, or mentions Boxcutter, VMs, or dev environments.
  Also use when the user asks to "try something in a clean environment", "set up a
  dev box", "run this somewhere safe", or needs any kind of ephemeral compute.
---

# Boxcutter

Boxcutter provides ephemeral dev environment VMs powered by Firecracker microVMs. VMs boot in about one second, get real LAN IP addresses, and come pre-loaded with common dev tools. They're disposable — create one, use it, destroy it.

## Finding your Boxcutter host

The Boxcutter control interface runs on a Node VM with a specific IP on your LAN. This IP varies per installation. Check these locations for the host address:

1. **CLAUDE.md** in the current project — look for a Boxcutter host IP
2. **Memory files** — check for previously saved Boxcutter configuration
3. **Ask the user** — if neither source has it, ask: "What's the IP of your Boxcutter host?"

Once you know the IP, save it to memory so you don't have to ask again.

All examples below use `HOST` as a placeholder for this IP.

## Control interface

All VM management happens over SSH to the Boxcutter host. No username is required — any username works, so just use the host IP directly.

Always use `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null` when SSHing to Boxcutter hosts and VMs, since VMs are ephemeral and their host keys change on every creation.

```bash
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null HOST <command>
```

### Commands

| Command | Description |
|---------|-------------|
| `ssh HOST new` | Create and start a new VM (returns name + IP) |
| `ssh HOST list` | List all VMs with status |
| `ssh HOST start <name>` | Start a stopped VM |
| `ssh HOST stop <name>` | Gracefully stop a VM |
| `ssh HOST destroy <name>` | Permanently delete a VM and its data |
| `ssh HOST status` | Show host capacity (RAM headroom, VM count) |
| `ssh HOST adduser <github-user>` | Import SSH keys from GitHub for a new user |
| `ssh HOST help` | Show available commands |

### Creating a VM

```bash
ssh HOST new
```

Output looks like:
```
Creating VM: bold-fox (4 vCPU, 8GB RAM, 50G disk)
  Creating copy-on-write snapshot...
  VM created: bold-fox (IP: 192.168.2.200)
Starting VM: bold-fox (192.168.2.200)

VM ready: bold-fox
Connect: ssh 192.168.2.200
```

Parse both the **name** and **IP** from the output. The name appears after "VM ready:" and the IP appears after "Connect: ssh". You need the name for lifecycle commands (`stop`, `destroy`) and the IP for direct SSH access into the VM.

Always give the user the VM's IP address. mDNS (`<name>.local`) is unreliable across networks, so always provide the IP as the primary way to connect.

## Connecting to a VM

SSH directly to the VM's IP. No username needed — all users map to `dev` (uid 1000) with passwordless sudo.

```bash
# By IP
ssh <VM_IP>

# By mDNS hostname (if the network supports multicast DNS)
ssh <name>.local    # e.g., ssh bold-fox.local

# Run a command without an interactive session
ssh <VM_IP> "command here"
```

### Waiting for SSH

After `ssh HOST new`, the VM needs 2-3 seconds before SSH is ready. Test with:

```bash
ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no <VM_IP> echo ready
```

Retry a few times if it doesn't connect on the first attempt.

## Pre-installed tools

VMs come with:

- **Languages:** Node.js 22, Ruby 3.2 (via mise)
- **Version manager:** mise (supports Node, Ruby, Python, Go, Rust, and more)
- **Build tools:** build-essential, gcc, make
- **CLI tools:** git, curl, wget, jq, tmux, htop
- **Dev tools:** GitHub CLI (gh), Claude Code
- **System:** Ubuntu Noble, systemd, SSH, sudo

The `dev` user has passwordless sudo, so `sudo apt-get install ...` works without prompts.

## Installing additional tools

Use mise for language runtimes:

```bash
ssh <VM_IP> "mise use -g python@3.12"
ssh <VM_IP> "mise use -g go@1.22"
```

Use apt for system packages:

```bash
ssh <VM_IP> "sudo apt-get update && sudo apt-get install -y postgresql redis-server"
```

## Copying files to/from a VM

```bash
# Copy a file to the VM
scp file.txt <VM_IP>:/home/dev/

# Copy a directory
scp -r myproject/ <VM_IP>:/home/dev/

# Copy from the VM
scp <VM_IP>:/home/dev/output.txt ./
```

## Accessing services running in a VM

VMs have real LAN IPs, so services are directly reachable from any machine on the same network. If a VM at 192.168.2.200 runs a server on port 3000, access it at `http://192.168.2.200:3000` from any machine on the LAN.

From the user's local machine:
```bash
curl http://<VM_IP>:3000
```

From inside the VM (after SSHing in):
```bash
curl http://localhost:3000
```

The mDNS hostname also works: `http://<name>.local:3000` (if the network supports multicast).

## Workflow patterns

### Quick test in a clean environment

When the user wants to test something without affecting their local machine:

1. `ssh HOST new` — create a VM
2. Copy files or clone a repo into the VM
3. Run the test
4. Capture results
5. `ssh HOST destroy <name>` — clean up

### Persistent dev environment

When the user needs an environment that persists across sessions:

1. `ssh HOST new` — create the VM
2. Set up tools and clone repos
3. Save the VM name to memory so you can reconnect later
4. Use `ssh HOST stop <name>` when not in use (preserves disk state)
5. Use `ssh HOST start <name>` to resume

### Running untrusted or experimental code

VMs are isolated Firecracker microVMs with their own kernel. They're a good place to run code you don't fully trust, test destructive operations, or experiment with system-level changes.

## Important things to know

- **VMs are ephemeral.** Destroying a VM permanently deletes all its data. There is no undo.
- **Each VM defaults to 4 vCPU, 8GB RAM, 50GB disk.** This is enough for most dev workloads.
- **Boot time is ~1 second.** VM creation uses copy-on-write snapshots of a golden image, so it's nearly instant.
- **All SSH is key-based.** Your SSH keys must be registered with the Boxcutter host (via `adduser` or during initial setup).
- **VMs get real LAN IPs.** They're directly reachable from any machine on the same network, not hidden behind NAT.
- **Any SSH username works.** Both on the control host and on VMs — no need to specify a user.
- **Check capacity before creating many VMs.** Run `ssh HOST status` to see available RAM. Each VM uses 8GB by default.
