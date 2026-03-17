# Boxcutter Workflows

How to use ephemeral VMs as disposable, isolated development environments — and why you'd want to.

## The Core Idea

Boxcutter gives you VMs that behave like scratch paper. Spin one up in seconds, install whatever you want, break things, throw it away. Each VM is fully isolated, joins your Tailscale network automatically, and can be accessed from any device on your tailnet. You never SSH into a shared server. You never worry about conflicting dependencies. You never pollute your main machine.

The workflow is: stamp out a VM, do your work, tear it down when you're done. If you want to keep it, keep it. If you want to fork it, copy it. If you want to walk away and come back tomorrow, it's still there.

## Stamping Out a VM

Creating a VM takes about 15 seconds for Firecracker, about 30 seconds for QEMU:

```bash
# Lightweight VM (Firecracker) — boots in ~200ms, good for most work
ssh boxcutter new

# Full VM (QEMU) — boots in ~5s, has Docker support
ssh boxcutter new --type qemu --ram 4096

# With a repo already cloned
ssh boxcutter new --type qemu --ram 4096 --clone github.com/myorg/myproject

# With specific resources
ssh boxcutter new --vcpu 4 --ram 8192 --disk 100G
```

The VM gets a random name (like `spicy-fox` or `sunny-lynx`), a Tailscale IP, and SSH access. You can connect immediately:

```bash
ssh spicy-fox
```

No key exchange, no IP address to remember, no VPN to connect to. Tailscale handles it. You can be on your laptop at a coffee shop, on your phone, on a different computer entirely — if it's on your tailnet, you can reach your VM.

## Installing Tools

Each VM starts minimal — Ubuntu base with SSH, git, curl, and a few essentials. You install what you need:

```bash
# Inside the VM
sudo apt install -y python3 nodejs build-essential
pip install torch transformers
npm install -g typescript
```

For QEMU VMs, Docker is automatically installed on first boot:

```bash
docker compose up -d
```

The point is that you never need to ask anyone for permission, wait for a provisioning ticket, or worry about breaking a shared environment. It's your VM. Install Rust nightly, pin a weird version of gcc, run three different Python versions — nobody else is affected.

## Private Repos and GitHub Access

Boxcutter integrates with GitHub Apps to provide scoped repository access. When you clone a private repo or need submodule access:

```bash
# Grant access to specific repos
ssh boxcutter repos add spicy-fox myorg/main-repo
ssh boxcutter repos add spicy-fox myorg/shared-lib
ssh boxcutter repos add spicy-fox myorg/config

# Inside the VM, git operations just work
git clone https://github.com/myorg/main-repo
cd main-repo
git submodule update --init --recursive
```

The token is scoped to only the repos you've granted. It refreshes automatically. If you add more repos later, the token updates without restarting anything.

For repos that use SSH URLs in `.gitmodules`, add this inside the VM:

```bash
git config --global url."https://github.com/".insteadOf "git@github.com:"
```

## Running AI Agents

This is where boxcutter really shines. You can run Claude Code (or any AI coding agent) inside a VM with full permissions, zero risk to your real environment:

```bash
# Inside the VM
claude --dangerously-skip-permissions
```

The `--dangerously-skip-permissions` flag lets the agent read, write, and execute anything without prompting. This sounds terrifying on your real machine. Inside a boxcutter VM, it's fine — the VM is disposable. If the agent breaks something, destroy the VM and stamp out a new one. Your real machine, your data, your other projects are completely untouched.

### Detach and Reattach

Use tmux to keep agents running after you disconnect:

```bash
# Start Claude Code in a tmux session
tmux new-session -s claude
claude --dangerously-skip-permissions

# Detach: Ctrl+B then D
# Disconnect from SSH entirely — the agent keeps running

# Come back later, from any device
ssh spicy-fox
tmux attach -t claude
# Pick up right where you left off
```

This means you can:
1. Start an agent working on a task
2. Close your laptop and go to lunch
3. Open your phone and check progress via Tailscale
4. Get home, open your desktop, reattach to the session
5. The agent has been working the whole time

### Remote Control

Claude Code supports remote control, letting you interact with a running agent from a web browser:

```bash
# Inside the VM's tmux session
/remote-control

# Gives you a URL like:
# https://claude.ai/code/session_01F3b95SRxdnKVKX451TuMoY
```

Now you can send commands to the agent from any browser, without even needing SSH access. Monitor progress from your phone. Send follow-up instructions. Review what the agent has done.

### Multiple Agents, Multiple Projects

Nothing stops you from running multiple VMs with different agents working on different things simultaneously:

```bash
ssh boxcutter new --type qemu --ram 4096 --clone github.com/myorg/frontend
ssh boxcutter new --type qemu --ram 4096 --clone github.com/myorg/backend
ssh boxcutter new --clone github.com/myorg/infra
```

Three VMs, three agents, three independent workstreams. Each one is isolated. They can't interfere with each other. You can check on any of them from any device.

## Copying and Forking VMs

The `cp` command clones a VM's disk, creating an exact copy:

```bash
ssh boxcutter cp spicy-fox spicy-fox-backup
```

This is useful for:

**Checkpointing before risky changes.** About to let an agent refactor your entire codebase? Copy the VM first. If the refactor goes sideways, destroy the new one and go back to the copy.

```bash
ssh boxcutter cp my-project my-project-before-refactor
# Let the agent loose on my-project
# If it breaks everything:
ssh boxcutter destroy my-project
ssh boxcutter cp my-project-before-refactor my-project
```

**Forking to explore alternatives.** Want to try two different approaches to the same problem? Copy the VM and let two agents work in parallel:

```bash
ssh boxcutter cp my-project approach-a
ssh boxcutter cp my-project approach-b
# Run agents on both, compare results
```

**Snapshotting known-good states.** Get your project to a working state with all dependencies installed, tests passing, services running? Copy it. Now you have a golden starting point you can stamp out copies from whenever you want.

## Full Isolation

Every VM is completely isolated:

- **Filesystem isolation.** Each VM has its own root filesystem. No shared volumes, no shared home directories, no accidental cross-contamination.

- **Network isolation.** Each VM gets its own network stack with a unique Tailscale identity. VMs cannot see each other's traffic. A compromised VM cannot reach other VMs on the same node.

- **Process isolation.** VMs run under separate hypervisors (Firecracker or QEMU with KVM). There is no shared kernel between VMs. A kernel panic in one VM does not affect others.

- **Resource isolation.** Each VM has dedicated CPU and RAM allocations. One VM can't starve another of resources.

This isolation is what makes `--dangerously-skip-permissions` safe. The "danger" is contained. The agent has root access to an environment that contains nothing valuable and can be destroyed in one command.

## Disconnecting Completely

When you're done for the day, you can just close your laptop. Everything keeps running:

- VMs persist across your disconnection. They're not tied to your SSH session.
- Agents in tmux sessions keep working.
- Docker containers keep running.
- Tailscale keeps the VM accessible.

When you reconnect — from any device, anywhere — everything is exactly as you left it. There's no "session expired" or "connection timed out" or "your environment was recycled."

VMs also survive host reboots. The boxcutter control plane automatically relaunches all VMs from saved state when the host comes back up.

## The Lifecycle of a Typical Project

Here's a concrete example of how this looks in practice:

```bash
# Monday morning: start a new project
ssh boxcutter new --type qemu --ram 4096 --clone github.com/myorg/api-service
# VM: bold-fox

# Grant access to internal packages
ssh boxcutter repos add bold-fox myorg/api-service
ssh boxcutter repos add bold-fox myorg/shared-proto
ssh boxcutter repos add bold-fox myorg/auth-lib

# SSH in and set up the environment
ssh bold-fox
cd project
git submodule update --init
docker compose up -d
npm install

# Start an agent to work on the feature
tmux new -s agent
claude --dangerously-skip-permissions
# "Implement the new billing endpoint per the spec in docs/billing.md"
# Ctrl+B, D to detach

# Go to meetings, check progress from your phone occasionally
# Agent is writing code, running tests, fixing issues

# Tuesday: check in on progress
ssh bold-fox
tmux attach -t agent
# Review what the agent did, give feedback, let it continue

# Wednesday: ready to test a different approach
ssh boxcutter cp bold-fox bold-fox-alt
ssh bold-fox-alt
# Try a different architecture in this copy

# Thursday: the original approach was better
ssh boxcutter destroy bold-fox-alt

# Friday: feature is done, PR is up
ssh boxcutter destroy bold-fox
# Clean. No traces. No lingering containers or packages.
```

## VM Types: When to Use What

**Firecracker** (`ssh boxcutter new`):
- Boots in ~200ms
- Minimal overhead (~128MB base)
- Best for: code editing, scripting, testing, AI agent work
- Cannot run Docker (minimal kernel)

**QEMU** (`ssh boxcutter new --type qemu`):
- Boots in ~5-10 seconds
- Full Linux kernel with all modules
- Best for: Docker, docker-compose, integration testing, full-stack development
- Docker is automatically installed on first boot

Choose Firecracker unless you specifically need Docker or other kernel-dependent features. You can always destroy a Firecracker VM and recreate as QEMU if you realize you need Docker.

## Monitoring and Debugging

Check cluster status:

```bash
ssh boxcutter status     # Capacity summary
ssh boxcutter nodes      # Node health
ssh boxcutter list       # All VMs with type, IP, status
```

View VM console logs (boot messages, kernel output, crash traces):

```bash
ssh boxcutter logs bold-fox
ssh boxcutter logs bold-fox --lines 200
```

This is especially useful for debugging VMs that crash on boot or have kernel-level issues.

## Tips

**Name your VMs intentionally** for long-running projects. The auto-generated names are cute but forgettable. You can use `--clone` with a meaningful repo URL and the VM takes the project name.

**Use QEMU VMs for anything involving Docker.** Don't fight the Firecracker kernel limitations.

**Copy before you experiment.** VM copies are cheap (copy-on-write). Make a copy before letting an agent do something drastic.

**Don't hoard VMs.** They're meant to be disposable. If you haven't touched a VM in a week, destroy it and recreate if you need it again. Your code is in git. Your environment can be recreated in 30 seconds.

**Use `repos add` before cloning.** If your project has private submodules, add all the repos to the VM's access list before running `git submodule update --init`. Otherwise the submodule clone will fail on private repos.
