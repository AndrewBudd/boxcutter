---
name: boxcutter
description: >
  Manage ephemeral dev environment VMs using Boxcutter. Boxcutter runs Firecracker
  microVMs that boot in ~1 second with Tailscale IPs and SSH access. Use this skill
  whenever the user needs a fresh development environment, wants to test something
  in a clean VM, needs an isolated machine for running builds or scripts, wants to
  spin up a throwaway environment, or mentions Boxcutter, VMs, or dev environments.
  Also use when the user asks to "try something in a clean environment", "set up a
  dev box", "run this somewhere safe", or needs any kind of ephemeral compute.
---

# Boxcutter

Boxcutter provides ephemeral dev environment VMs powered by Firecracker microVMs. VMs boot in about one second, join Tailscale automatically for network access, and come pre-loaded with common dev tools. They're disposable — create one, use it, destroy it.

## Finding your Boxcutter host

The Boxcutter control interface runs on a Node VM that joins Tailscale as `boxcutter`. Check these locations for the host address:

1. **CLAUDE.md** in the current project — look for a Boxcutter host IP or hostname
2. **Memory files** — check for previously saved Boxcutter configuration
3. **Try `boxcutter`** — if MagicDNS is enabled, this hostname should work
4. **Ask the user** — if nothing works, ask: "What's the Tailscale IP of your Boxcutter host?"

Once you know the host, save it to memory so you don't have to ask again.

All examples below use `HOST` as a placeholder for the Tailscale IP or MagicDNS name.

## Control interface

All VM management happens over SSH to the Boxcutter host. No username is required — any username works, so just use the host directly.

Always use `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null` when SSHing to Boxcutter hosts and VMs, since VMs are ephemeral and their host keys change on every creation.

```bash
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null HOST <command>
```

### Commands

| Command | Description |
|---------|-------------|
| `ssh HOST new` | Create and start a new VM (returns name + Tailscale IP) |
| `ssh HOST list` | List all VMs with Tailscale IPs and status |
| `ssh HOST start <name>` | Start a stopped VM |
| `ssh HOST stop <name>` | Gracefully stop a VM |
| `ssh HOST destroy <name>` | Permanently delete a VM (auto-removed from Tailscale) |
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
  VM created: bold-fox (internal: 10.0.1.200)
Starting VM: bold-fox (internal: 10.0.1.200)
  Waiting for Tailscale...
  Tailscale IP: 100.64.1.42

VM ready: bold-fox
Connect: ssh 100.64.1.42
```

Parse both the **name** and **Tailscale IP** from the output. The name appears after "VM ready:" and the IP appears after "Connect: ssh". You need the name for lifecycle commands (`stop`, `destroy`) and the Tailscale IP for direct SSH access into the VM.

Always give the user the VM's Tailscale IP address.

## Connecting to a VM

SSH directly to the VM's Tailscale IP. No username needed — all users map to `dev` (uid 1000) with passwordless sudo. You have full root access via `sudo` on every VM — use it freely to install packages, configure services, edit system files, or anything else. These are disposable VMs; you cannot break anything that matters.

```bash
# By Tailscale IP
ssh <TAILSCALE_IP>

# By MagicDNS hostname (if MagicDNS is enabled on the tailnet)
ssh <name>    # e.g., ssh bold-fox

# Run a command without an interactive session
ssh <TAILSCALE_IP> "command here"
```

### Waiting for SSH

After `ssh HOST new`, the VM needs a few seconds before SSH is ready (Tailscale handshake adds ~3-5s). Test with:

```bash
ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no <TAILSCALE_IP> echo ready
```

Retry a few times if it doesn't connect on the first attempt.

## Pre-installed tools

VMs come with:

- **Languages:** Node.js 22, Ruby 3.2 (via mise)
- **Version manager:** mise (supports Node, Ruby, Python, Go, Rust, and more)
- **Build tools:** build-essential, gcc, make
- **CLI tools:** git, curl, wget, jq, tmux, htop
- **Dev tools:** GitHub CLI (gh), Claude Code
- **Networking:** Tailscale (auto-joined)
- **System:** Ubuntu Noble, systemd, SSH, sudo

The `dev` user has passwordless sudo, so `sudo apt-get install ...` works without prompts.

## Installing additional tools

Use mise for language runtimes:

```bash
ssh <TAILSCALE_IP> "mise use -g python@3.12"
ssh <TAILSCALE_IP> "mise use -g go@1.22"
```

Use apt for system packages:

```bash
ssh <TAILSCALE_IP> "sudo apt-get update && sudo apt-get install -y postgresql redis-server"
```

## Copying files to/from a VM

```bash
# Copy a file to the VM
scp file.txt <TAILSCALE_IP>:/home/dev/

# Copy a directory
scp -r myproject/ <TAILSCALE_IP>:/home/dev/

# Copy from the VM
scp <TAILSCALE_IP>:/home/dev/output.txt ./
```

## Accessing services running in a VM

VMs have Tailscale IPs, so services are directly reachable from any device on the tailnet. If a VM at 100.64.1.42 runs a server on port 3000, access it at `http://100.64.1.42:3000` from any device on the tailnet.

From the user's machine (on the same tailnet):
```bash
curl http://<TAILSCALE_IP>:3000
```

With MagicDNS:
```bash
curl http://bold-fox:3000
```

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
3. Save the VM name and Tailscale IP to memory so you can reconnect later
4. Use `ssh HOST stop <name>` when not in use (preserves disk state)
5. Use `ssh HOST start <name>` to resume

### Running untrusted or experimental code

VMs are isolated Firecracker microVMs with their own kernel. They're a good place to run code you don't fully trust, test destructive operations, or experiment with system-level changes. VMs are also isolated from each other on the internal network.

## Important things to know

- **VMs are ephemeral.** Destroying a VM permanently deletes all its data. There is no undo.
- **Each VM defaults to 4 vCPU, 8GB RAM, 50GB disk.** This is enough for most dev workloads.
- **Boot time is ~1 second** (internal network), plus ~3-5 seconds for Tailscale to connect.
- **All SSH is key-based.** Your SSH keys must be registered with the Boxcutter host (via `adduser` or during initial setup).
- **VMs are accessible via Tailscale.** They're reachable from any device on your tailnet, not just the local network.
- **Any SSH username works.** Both on the control host and on VMs — no need to specify a user.
- **Check capacity before creating many VMs.** Run `ssh HOST status` to see available RAM. Each VM uses 8GB by default.
- **Destroying a VM auto-removes it from Tailscale.** The ephemeral auth key means disconnected nodes are automatically cleaned up.
