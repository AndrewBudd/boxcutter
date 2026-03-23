# Host Control Plane Development

## Building

```bash
cd host && go build -o boxcutter-host ./cmd/host/
```

Or via Make:

```bash
make build-host          # Build boxcutter-host
make install-host        # Build + install to /usr/local/bin + systemd
```

## Deploying

The host binary runs directly on bare metal:

```bash
make install-host
sudo systemctl restart boxcutter-host
```

Or run directly without the service:

```bash
cd host && go build -o boxcutter-host ./cmd/host/
sudo ./boxcutter-host status
```

## Debugging

```bash
# Logs
sudo journalctl -u boxcutter-host -f

# Status via Unix socket
sudo curl --unix-socket /run/boxcutter-host.sock http://localhost/status
```

## Configuration

`host/boxcutter.env` controls VM sizing and network layout:

```bash
HOST_INTERFACE=enp34s0      # Physical NIC for NAT
NODE_VCPU=6                 # vCPUs per node VM
NODE_RAM=12G                # RAM per node VM
NODE_DISK=150G              # Disk per node VM
ORCH_VCPU=2                 # Orchestrator vCPUs
ORCH_RAM=4G                 # Orchestrator RAM
```

## Building and Publishing OCI Images

```bash
# Build a node image (boots a VM, installs everything, flattens to QCOW2)
make build-image TYPE=node

# Build + push
make publish TYPE=node
make publish TYPE=orchestrator
make publish-all

# Push a previously built image without rebuilding
./host/publish-image.sh node --push-only

# Custom tag (default: git short hash)
./host/publish-image.sh node --tag v1.0
```

Images are pushed to `ghcr.io/andrewbudd/boxcutter/{node,orchestrator}` with both a specific tag and `latest`.

Image builds take ~15-20 minutes. They require KVM access (nested virtualization).

## Upgrading a Running Cluster

```bash
# Update the host binary
sudo boxcutter-host self-update

# Rolling upgrade of all VMs
sudo boxcutter-host upgrade all

# Just nodes (launches new node, drains old, zero VM downtime)
sudo boxcutter-host upgrade node

# Just orchestrator
sudo boxcutter-host upgrade orchestrator

# Just the golden image (nodes pull via MQTT)
sudo boxcutter-host upgrade golden

# Verify
sudo boxcutter-host version
```

Node upgrades use drain + migration: a new node is launched from the latest image, all VMs (Firecracker and QEMU) are migrated off the old node — Firecracker via snapshot/restore, QEMU via QMP state save/restore — then the old node is stopped.

## State Recovery

If `cluster.json` is lost or corrupted:

```bash
sudo boxcutter-host recover
```

Scans `/proc` for running QEMU processes and reconstructs `cluster.json`. Existing VMs are preserved.

## Packaging

```bash
make deb-host              # Build .deb package
make release-host          # Create release tarball
```

## Systemd

| Service | Binary | Port | Description |
|---------|--------|------|-------------|
| `boxcutter-host` | `boxcutter-host` | `/run/boxcutter-host.sock` | Control plane daemon |
