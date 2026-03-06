---
name: managing-dev-vms
description: >
  Manages ephemeral dev environment VMs via SSH. Creates, lists, starts, stops, and
  destroys Firecracker microVMs that boot in ~5 seconds with Tailscale networking,
  pre-installed dev tools (Node.js, Ruby, git, Claude Code), and passwordless sudo.
  VMs use per-TAP fwmark routing (every VM is 10.0.0.2 on isolated TAP links).
  Supports normal and paranoid modes. Triggers on: spinning up a VM, creating a dev
  environment, testing in a clean machine, running something in isolation, setting up
  a dev box, ephemeral compute, throwaway environments, sandboxed execution, "try this
  somewhere safe", "fresh environment", "clean VM", or any mention of Boxcutter. Also
  triggers when the user needs to SSH into a remote dev machine, check VM status, or
  manage VM lifecycle.
---

# Boxcutter

Boxcutter provides ephemeral dev environment VMs powered by Firecracker microVMs. VMs boot in about five seconds (including Tailscale join), get a routable Tailscale IP, and come pre-loaded with common dev tools. They're disposable — create one, use it, destroy it.

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
| `ssh HOST create <name> --mode paranoid` | Create a paranoid-mode VM |
| `ssh HOST list` | List all VMs with marks, modes, Tailscale IPs, and status |
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
Creating VM: bold-fox (4 vCPU, 8GB RAM, 50G disk, mode: normal)
  Creating copy-on-write snapshot...
  Injecting CA cert...
  VM created: bold-fox (mark: 41022, mode: normal)
Starting VM: bold-fox (mark: 41022, mode: normal)
  VM started (PID 12847)
  Waiting for VM to boot...
ready
  Joining Tailscale...
  Tailscale IP: 100.64.1.42
  SSH: ssh 100.64.1.42

VM ready: bold-fox
Connect: ssh 100.64.1.42
```

Parse the **name** and **Tailscale IP** from the output. The name appears after "VM ready:" and the IP appears after "Connect: ssh". You need the name for lifecycle commands (`stop`, `destroy`) and for SSH access — the VM name works as a hostname via MagicDNS (e.g., `ssh bold-fox`). The Tailscale IP also works (`ssh 100.64.1.42`).

Prefer using the VM name as the hostname — it's shorter and more readable. Give the user both the name and the Tailscale IP.

## Connecting to a VM

SSH directly to the VM by its name (via MagicDNS) or Tailscale IP. No username needed — all users map to `dev` (uid 1000) with passwordless sudo. You have full root access via `sudo` on every VM — use it freely to install packages, configure services, edit system files, or anything else. These are disposable VMs; you cannot break anything that matters.

```bash
# By hostname (preferred — MagicDNS resolves the VM name)
ssh bold-fox

# By Tailscale IP (always works)
ssh 100.64.1.42

# Run a command without an interactive session
ssh bold-fox "command here"
```

### Waiting for SSH

The `ssh HOST new` command waits for the VM to be fully booted and Tailscale-connected before returning. By the time you see the "VM ready" line, SSH should work immediately. If it doesn't connect on the first try (rare), retry once or twice:

```bash
ssh -o ConnectTimeout=5 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null bold-fox echo ready
```

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
ssh bold-fox "mise use -g python@3.12"
ssh bold-fox "mise use -g go@1.22"
```

Use apt for system packages:

```bash
ssh bold-fox "sudo apt-get update && sudo apt-get install -y postgresql redis-server"
```

## Copying files to/from a VM

Always use `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null` with scp too, since VM host keys are ephemeral.

```bash
# Copy a file to the VM
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null file.txt bold-fox:/home/dev/

# Copy a directory
scp -r -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null myproject/ bold-fox:/home/dev/

# Copy from the VM
scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null bold-fox:/home/dev/output.txt ./
```

## Accessing services running in a VM

Services running in a VM are directly reachable from any device on the tailnet using the VM's hostname or Tailscale IP.

```bash
curl http://bold-fox:3000
curl http://100.64.1.42:3000
```

For HTTPS, see the TLS certificates section below.

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
3. Save the VM name to memory so you can reconnect later (the name is the hostname)
4. Use `ssh HOST stop <name>` when not in use (preserves disk state)
5. Use `ssh HOST start <name>` to resume

### Running untrusted or experimental code

VMs are isolated Firecracker microVMs with their own kernel. They're a good place to run code you don't fully trust, test destructive operations, or experiment with system-level changes. VMs are also isolated from each other on separate TAP devices (no shared network).

## TLS certificates for VM services

Tailscale can provision real Let's Encrypt TLS certificates for any VM on the tailnet. This requires HTTPS to be enabled in the Tailscale admin console (one-time setup at https://login.tailscale.com/admin/dns).

### Using `tailscale cert`

After a VM is running and joined to Tailscale, get a cert for its MagicDNS name:

```bash
ssh bold-fox "sudo tailscale cert bold-fox.<tailnet-name>.ts.net"
```

This writes `.crt` and `.key` files to the current directory. The private key never leaves the VM. Certs last 90 days and must be manually renewed when obtained this way.

### Automatic certs with Caddy

Caddy 2.5+ natively fetches TLS certs from the local Tailscale daemon for `*.ts.net` domains — zero configuration needed. A Caddyfile like this just works:

```
bold-fox.tail038cc3.ts.net {
    reverse_proxy localhost:3000
}
```

Caddy handles cert provisioning and renewal automatically. It needs access to the Tailscale socket (run as root or grant the caddy user access).

## Important things to know

- **VMs are ephemeral.** Destroying a VM permanently deletes all its data. There is no undo. Stopping a VM preserves its disk state — you can start it again later.
- **Each VM defaults to 4 vCPU, 8GB RAM, 50GB disk.** This is enough for most dev workloads.
- **Boot time is ~5 seconds** total (VM boot + Tailscale join). The `new` command waits for everything to be ready before returning.
- **All SSH is key-based.** The keys of whoever ran the initial setup are automatically trusted. Additional users can be added via `adduser <github-user>`.
- **VMs are accessible via Tailscale.** They're reachable from any device on your tailnet, not just the local network.
- **Any SSH username works.** Both on the control host and on VMs — no need to specify a user.
- **Check capacity before creating many VMs.** Run `ssh HOST status` to see available RAM. Each VM uses 8GB by default.
- **Destroying a VM auto-removes it from Tailscale.** The ephemeral auth key means disconnected nodes are automatically cleaned up.
- **Normal vs paranoid mode.** Normal VMs have full internet. Paranoid VMs route through a MITM proxy with sentinel token swapping — real credentials never touch the VM.
