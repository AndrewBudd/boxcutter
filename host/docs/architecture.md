# Host Control Plane Architecture

The host control plane is a Go binary (`boxcutter-host`) running as a systemd service on bare metal. It owns all infrastructure outside VMs: QEMU processes, bridge networking, NAT, OCI image distribution, auto-scaling, and health monitoring.

## Bootstrap

`boxcutter-host bootstrap` sets up a cluster from scratch:

1. Sets up the bridge network (`br-boxcutter`) and NAT rules
2. Pulls pre-built VM images from `ghcr.io/andrewbudd/boxcutter` (with retry on failure)
3. Creates COW disks from the base images
4. Generates cloud-init ISOs (injects secrets from `~/.boxcutter/`)
5. Launches the orchestrator and node-1 QEMU VMs
6. Waits for both to become healthy
7. Sets up the golden image (Firecracker rootfs) on the node

Bootstrap is fully idempotent — if it fails partway through, re-run it and it picks up where it left off.

## Boot Recovery

On reboot, the daemon recreates bridge/NAT (idempotent) and relaunches all VMs from `cluster.json`. If `cluster.json` is lost, the `recover` command scans `/proc` for running QEMU processes and reconstructs state from their command-line arguments.

## Health Monitoring

10-second polling loop. Checks QEMU process liveness and auto-restarts crashed VMs. If a VM fails to restart after multiple attempts, it logs the failure but doesn't escalate.

## Auto-Scaling

30-second polling loop. Queries each node's `GET /api/health` for capacity metrics.

- **Scale up** when any node exceeds: RAM >80%, vCPU >80%, or disk >85%
- **Scale down** when a node is idle AND removing it won't push remaining nodes above thresholds
- Before scaling up: checks host has enough disk (>20GB free) and memory

Default thresholds (in `defaultConfig()`):
- RAM: 80%, CPU: 80%, Disk: 85%
- Min free disk on host: 20GB
- Scale cooldown: 10 minutes between any scale events

## Drain and Migration Coordination

When draining a node (for upgrade or removal):

1. Host calls the source node's migrate API for each VM
2. Node agent handles the actual snapshot/restore transfer to the target
3. Once all VMs are migrated off, host stops the QEMU process

The host coordinates *when* to drain; the node agent handles *how* to migrate.

## OCI Image Distribution

VM base images are OCI artifacts stored at `ghcr.io/andrewbudd/boxcutter/`:

```
├── node:latest          (~1.1GB zstd-compressed QCOW2)
├── orchestrator:latest  (~1.1GB zstd-compressed QCOW2)
└── golden:latest        (~450MB zstd-compressed ext4)
```

Pull is anonymous (public packages). Push requires `gh auth login`.

**Deploy flow** (bootstrap or upgrade):
1. `oci.Pull()` downloads `.zst` from ghcr.io
2. `decompressZstd()` produces base QCOW2
3. `createCOWDisk()` creates thin overlay from base
4. `provision.sh --from-image` generates cloud-init ISO (injects secrets from `~/.boxcutter/`)
5. QEMU launches with COW disk + cloud-init ISO
6. Cloud-init runs a script that configures secrets, joins Tailscale, starts services

**Image build pipeline** (`host/build-image.sh`):
1. Downloads Ubuntu 24.04 cloud image (if not cached)
2. Creates a COW overlay disk
3. Generates a cloud-init ISO with all binaries, scripts, configs, and systemd units
4. Boots QEMU with the overlay + ISO
5. Cloud-init runs: installs packages, places binaries, enables services
6. SSHs in to clean instance-specific state (Tailscale, cloud-init, host keys)
7. Powers off the VM
8. Flattens the COW overlay to a standalone QCOW2
9. Compresses with zstd

The cleanup step may return a non-zero exit code (SSH disconnects when the VM powers off). The script checks for the output file rather than relying on the exit code.

## MQTT Broker

Runs Mosquitto on the host bridge (`192.168.50.1:1883`) for golden image version distribution. The orchestrator publishes golden head versions; nodes subscribe and pull new images.

## Secrets and Provisioning

All secrets live in `~/.boxcutter/secrets/` on the host (gitignored). They are injected into VMs via cloud-init ISOs at provisioning time.

| Secret | Purpose |
|--------|---------|
| `tailscale-node-authkey` | Tailscale auth key for orchestrator + node VMs (reusable, not ephemeral) |
| `tailscale-vm-authkey` | Tailscale auth key for Firecracker microVMs (reusable, ephemeral) |
| `cluster-ssh.key` / `.pub` | SSH key for inter-node communication (migration, deploy) |
| `authorized-keys` | SSH public keys for the boxcutter control interface |
| `github-app.pem` | GitHub App private key (for repo cloning in VMs) |

Config template at `~/.boxcutter/boxcutter.yaml` defines GitHub App IDs, Tailscale key paths, and per-node placeholders that are templated during provisioning.

## CLI Commands

```
boxcutter-host run          # Run the daemon (systemd service)
boxcutter-host bootstrap    # Pull OCI images and create VMs from scratch
boxcutter-host status       # Show cluster status
boxcutter-host pull <type>  # Pull VM image from OCI registry
boxcutter-host upgrade <t>  # Rolling upgrade of VMs
boxcutter-host recover      # Scan /proc for running VMs and rebuild cluster.json
boxcutter-host version      # Show image versions
boxcutter-host build-image  # Build VM image locally
boxcutter-host push-golden  # Push golden image to OCI
boxcutter-host self-update  # Update boxcutter-host binary from GitHub Releases
```

## Daemon Subsystems

When `boxcutter-host run` starts:

```
runDaemon()
  ├─ startMosquitto()          — Start MQTT broker
  ├─ bridge.Setup()            — Create/verify bridge + NAT (idempotent)
  ├─ cluster.Load()            — Load cluster.json
  ├─ bootRecover()             — Relaunch dead VMs from state
  ├─ go startAPI()             — Unix socket API server
  ├─ go healthLoop()           — 10s polling, auto-restart crashed QEMU
  ├─ go autoScaleLoop()        — 30s polling, scale up/down based on capacity
  └─ signal.Wait(SIGINT/TERM)  — Graceful shutdown
```

## State

`/var/lib/boxcutter/cluster.json` — tracks all QEMU VMs (orchestrator + nodes) with PIDs, disk paths, IPs, MACs, image versions/digests.

```json
{
  "orchestrator": {
    "id": "orchestrator", "type": "orchestrator",
    "bridge_ip": "192.168.50.2", "pid": 12345,
    "disk": "/var/lib/boxcutter/orchestrator.qcow2",
    "iso": "/var/lib/boxcutter/orchestrator-cloud-init.iso",
    "vcpu": 2, "ram": "4G",
    "tap": "tap-orch", "mac": "52:54:00:00:00:02"
  },
  "nodes": [
    {
      "id": "boxcutter-node-1", "type": "node",
      "bridge_ip": "192.168.50.3", "pid": 12346,
      "vcpu": 6, "ram": "12G", ...
    }
  ]
}
```

## Unix Socket API

`/run/boxcutter-host.sock` — local-only, unreachable from inside VMs.

| Endpoint | Description |
|----------|-------------|
| `GET /status` | Cluster status |
| `POST /drain/{nodeID}` | Drain a node (migrate all VMs off, then stop) |
