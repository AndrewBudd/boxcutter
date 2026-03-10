# stereOS vs Boxcutter: Architecture Comparison

## What stereOS Is

[stereOS](https://stereos.ai/) is a hardened NixOS-based operating system purpose-built for running AI agents in isolated VMs. Developed by [Paper Compute Co](https://github.com/papercomputeco/stereos), it focuses on a single-agent-per-VM model where disposable VMs are booted, injected with credentials, and torn down after use.

## What Boxcutter Is

Boxcutter is an ephemeral Firecracker dev environment platform running on a single physical host. It manages a multi-tier VM hierarchy (QEMU nodes → Firecracker microVMs) with orchestration, live migration, auto-scaling, and golden image distribution.

## Head-to-Head Comparison

| Dimension | **stereOS** | **Boxcutter** |
|---|---|---|
| **Purpose** | Run AI coding agents in isolated VMs | Run ephemeral dev environments on a single host |
| **Target user** | AI agent operators (Claude Code, OpenCode, etc.) | Developers needing disposable dev VMs |
| **VM Technology** | Full QEMU/KVM VMs (deliberate choice over microVMs) | Two-tier: QEMU VMs for nodes + Firecracker microVMs for workloads |
| **Why that VM tech** | Needs secure boot, FIPS, GPU passthrough — Firecracker can't do these | Needs sub-second boot, minimal overhead, high density per host |
| **OS Foundation** | NixOS (declarative, reproducible) | Ubuntu-based (golden image via Dockerfile) |
| **Image format** | "Mixtapes" — raw EFI, QCOW2, kernel artifacts, zstd-compressed with `mixtape.toml` manifest | QCOW2 for nodes + sparse ext4 rootfs for Firecracker VMs, OCI-distributed from ghcr.io |
| **Build system** | Nix flakes + Dagger CI | Makefiles + Docker-to-ext4 conversion + OCI registry |
| **Configuration** | `jcard.toml` — declares which mixtape to boot and which agent to run | Cloud-init ISOs with injected secrets from `~/.boxcutter/` |
| **CLI** | `masterblaster` (`mb`) — single CLI managing local VMs | SSH-based commands through orchestrator (`ssh boxcutter new/list/destroy`) |
| **Orchestration** | None — single VM per invocation, local only | Full orchestrator with SQLite state, scheduler (most-free-RAM), node registration, health monitoring |
| **Scaling** | Single VM at a time | Auto-scaling: RAM >80%, vCPU >80%, or disk >85% triggers new nodes |
| **Networking** | Standard VM networking (host bridge/NAT) | fwmark-based per-TAP routing (all VMs are 10.0.0.2, distinguished by packet marks) |
| **Migration** | Not supported (VMs are disposable) | Snapshot-based live migration (~10s for 2GB RAM) |
| **Security model** | NixOS hardening, gVisor sandboxing, namespace isolation, separate admin/agent users | Sentinel token brokering (VMs never see real API keys), MITM proxy with allowlists, per-VM network isolation |
| **Credential handling** | Environment variables injected into VM via `jcard.toml` | Paranoid mode: random sentinel tokens swapped for real credentials by MITM proxy — real keys never touch VM disk |
| **Agent lifecycle** | `agentd` daemon manages agent processes inside the VM | No agent concept — VMs are general-purpose dev environments |
| **GPU support** | Yes — GPU passthrough for local model inference (ollama, vLLM) | No — Firecracker doesn't support GPU passthrough |
| **Multi-tenancy** | Single agent per VM (agents can nest sub-agents) | Multiple VMs per node, multiple nodes per host |
| **State persistence** | Disposable by design | Thin orchestrator state in SQLite; VM truth queried from nodes |
| **Language** | Nix (80%), Shell (10%), Go (5%) | Go (nearly 100%) |
| **Update mechanism** | Rebuild mixtape image via Nix | Kubernetes-style reconciliation loop: pull new OCI image → rolling replace → migrate VMs |

## Architectural Philosophy Differences

### 1. Full VMs vs MicroVMs — the core divergence

stereOS explicitly rejects Firecracker/microVMs because they strip virtual hardware, preventing secure boot, FIPS compliance, and GPU passthrough. For AI agents that may need to run local models, this matters.

Boxcutter embraces Firecracker because it needs density and speed — sub-second boot, <5MB memory overhead, 150+ VMs per host. The tradeoff is no GPU passthrough, no secure boot, and a more complex networking layer to compensate for Firecracker's lack of bridge support.

### 2. Single-host complexity vs distributed simplicity

stereOS keeps things simple: one CLI, one VM, one agent. The complexity lives in the NixOS build system (reproducible images, declarative config).

Boxcutter builds a full platform on a single host: orchestrator, scheduler, auto-scaler, health monitor, MQTT pub/sub, live migration, OCI image distribution, and a sophisticated fwmark routing system. It's closer to a mini-Kubernetes for Firecracker VMs.

### 3. Credential security

stereOS injects API keys as environment variables — straightforward but the keys exist in VM memory.

Boxcutter's "paranoid mode" is more sophisticated: VMs receive sentinel tokens (random hex), and a MITM proxy on the node intercepts outbound requests and swaps sentinels for real credentials. Real keys never enter the VM. This is a meaningfully stronger security boundary.

### 4. Image distribution

stereOS uses Nix for reproducibility — rebuild from source, get the same image. Distribution is via compressed artifacts with a `mixtape.toml` manifest.

Boxcutter uses OCI registries (ghcr.io) with MQTT-based version propagation — closer to how container platforms distribute images, with reconciliation loops for rolling upgrades.

### 5. What each does better

- **stereOS excels at**: GPU workloads, reproducible builds (Nix), secure boot requirements, AI-agent-specific ergonomics (jcard config, agent harnesses), and simplicity of the single-VM model
- **Boxcutter excels at**: High-density multi-tenant dev environments, live migration, auto-scaling, credential isolation (sentinel tokens), and managing many concurrent VMs on one host

## Summary

They solve related but distinct problems. stereOS is an **OS image** optimized for running a single AI agent with maximum isolation and hardware access. Boxcutter is a **platform** for running many ephemeral dev environments on a single host with orchestration, scaling, and migration. stereOS prioritizes security hardening and GPU support; Boxcutter prioritizes density, speed, and operational sophistication.

The projects could theoretically complement each other — a stereOS-style hardened image could run as a Boxcutter golden image if Firecracker's limitations weren't a blocker.
