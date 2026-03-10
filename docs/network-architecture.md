# Boxcutter Network Architecture

This document has been split into domain-specific network docs:

- **Host bridge network** (Layer 1: physical host to QEMU VMs) → [host/docs/network.md](../host/docs/network.md)
- **Node network** (Layer 2: node VM to Firecracker VMs, fwmark routing, vmid, proxy, TLS, packet flows) → [node/docs/network.md](../node/docs/network.md)

## Quick Reference

| Subnet/Address | Purpose |
|----------------|---------|
| `192.168.50.0/24` | Host bridge — orchestrator + all node VMs |
| `10.0.0.0/30` (per TAP) | Node to each Firecracker VM (point-to-point) |
| `100.x.x.x` | Tailscale overlay (external access) |

| Port | Service | Listener |
|------|---------|----------|
| 22 | SSH control interface | Orchestrator |
| 80 | vmid (metadata) | 169.254.169.254 on nodes |
| 443 | DERP relay | Node (`10.0.0.1`) |
| 1883 | Mosquitto MQTT broker | Host (`192.168.50.1`) |
| 8080 | Forward proxy | Nodes |
| 8800 | Node agent API | Nodes |
| 8801 | Orchestrator API | Orchestrator |
