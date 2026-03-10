# Host Control Plane

Infrastructure lifecycle manager. Runs on bare metal as a systemd service. Owns everything outside the VMs: QEMU processes, bridge networking, NAT, OCI image distribution, auto-scaling, health monitoring.

## Entry Point

`cmd/host/main.go` — single-file CLI with all commands. ~2500 lines.

Commands: `run`, `bootstrap`, `status`, `pull`, `upgrade`, `recover`, `version`, `build-image`, `push-golden`, `self-update`

## Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/bridge/` | Linux bridge + TAP device creation, NAT rules, IP forwarding (idempotent) |
| `internal/cluster/` | Persistent state in `/var/lib/boxcutter/cluster.json` (VM entries: PIDs, disks, IPs, MACs) |
| `internal/qemu/` | QEMU VM lifecycle — launch, stop, health check, COW disk creation |
| `internal/oci/` | OCI registry operations (pull/push QCOW2 images) + GitHub App auth for ghcr.io |

## Key Files

| File | Purpose |
|------|---------|
| `boxcutter.env` | VM sizing, network params (sourced by shell scripts) |
| `boxcutter-host.service` | systemd unit |
| `mosquitto.conf` | MQTT broker config |
| `provision.sh` | Generates cloud-init ISOs for VMs |
| `build-image.sh` | Boots VM, installs everything, flattens to QCOW2 |
| `publish-image.sh` | Build + push images to ghcr.io |
| `launch.sh` / `stop.sh` / `ssh.sh` | VM lifecycle helpers |
| `build-deb.sh` | Creates .deb package |

## Interfaces

- **Unix socket API** at `/run/boxcutter-host.sock`: `GET /status`, `POST /drain/{nodeID}`
- **CLI**: all commands in `cmd/host/main.go`
- **MQTT broker**: Mosquitto on `192.168.50.1:1883` (other components are clients)

## State

`/var/lib/boxcutter/cluster.json` — tracks all VMs (orchestrator + nodes) with PIDs, disk paths, IPs, MACs, image versions/digests.

## Runtime Behavior (daemon mode)

`boxcutter-host run` starts the long-running daemon:
1. Start Mosquitto broker
2. Set up bridge networking (idempotent)
3. Load cluster state, relaunch VMs from state
4. Start Unix socket API
5. Start health monitor (10s polling, auto-restart crashed VMs)
6. Start auto-scaler (30s polling, launch new nodes when >80% utilized)

## Build & Deploy

```bash
cd host && go build -o boxcutter-host ./cmd/host/
make install-host    # build + copy to /usr/local/bin + install systemd unit
```

## Domain Boundary

This component **owns**:
- QEMU VM processes (create, destroy, health check, restart)
- Bridge network and NAT rules
- OCI image pull/push
- Auto-scaling decisions
- MQTT broker process
- Cluster state file

This component **does not own**:
- Anything inside the VMs (that's orchestrator + node domains)
- Firecracker VM lifecycle (that's the node domain)
- User-facing SSH interface (that's the orchestrator domain)
- VM scheduling decisions (that's the orchestrator domain)
