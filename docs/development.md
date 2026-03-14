# Development Guide

Building and running boxcutter from source. For production installation, see the [README](README.md).

## Prerequisites

Everything from the README, plus:

```bash
# Go 1.22+
curl -sL https://go.dev/dl/go1.22.5.linux-amd64.tar.gz | sudo tar xz -C /usr/local
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# GitHub CLI (for pushing images to ghcr.io)
# https://cli.github.com/
```

## Clone and Configure

```bash
git clone https://github.com/AndrewBudd/boxcutter.git ~/boxcutter
cd ~/boxcutter
```

Edit `host/boxcutter.env` to match your host:

```bash
HOST_INTERFACE=enp34s0    # Your physical NIC (ip route | grep default)
NODE_VCPU=6               # vCPUs per node VM
NODE_RAM=12G              # RAM per node VM
NODE_DISK=150G            # Disk per node VM
```

Set up your secrets bundle in `~/.boxcutter/` as described in the README.

## Build and Install from Source

```bash
# Build + install the host binary + systemd unit
make install-host

# Bootstrap (builds all Go binaries, packages them into VMs)
sudo env BOXCUTTER_REPO=$PWD boxcutter-host bootstrap --from-source
```

The `--from-source` flag compiles all Go binaries (node agent, vmid, proxy, orchestrator) and packages them into the cloud-init payload instead of pulling pre-built images.

After bootstrap:

```bash
sudo systemctl enable --now boxcutter-host
```

## Go Modules

Five independent modules -- each can be built and tested independently:

| Module | Path | Binary |
|--------|------|--------|
| Host control plane | `host/` | `boxcutter-host` |
| Orchestrator | `orchestrator/` | `boxcutter-orchestrator`, `boxcutter-ssh-orchestrator` |
| Node agent | `node/agent/` | `boxcutter-node` |
| VM identity | `node/vmid/` | `vmid` |
| MITM proxy | `node/proxy/` | `boxcutter-proxy` |

Build individually:

```bash
cd host && go build -o boxcutter-host ./cmd/host/
cd orchestrator && go build -o boxcutter-orchestrator ./cmd/orchestrator/
cd node/agent && CGO_ENABLED=0 go build -o boxcutter-node ./cmd/node/
cd node/vmid && go build -o vmid ./cmd/vmid/
cd node/proxy && go build -o boxcutter-proxy ./cmd/proxy/
```

## Deploying Code Changes

After modifying Go code, deploy to running VMs without a full rebuild:

```bash
# Node agent
cd node/agent && CGO_ENABLED=0 go build -o /tmp/boxcutter-node ./cmd/node/
scp -i ~/.ssh/id_rsa /tmp/boxcutter-node ubuntu@192.168.50.3:/tmp/
ssh -i ~/.ssh/id_rsa ubuntu@192.168.50.3 "sudo systemctl stop boxcutter-node && sudo cp /tmp/boxcutter-node /usr/local/bin/ && sudo systemctl start boxcutter-node"

# Orchestrator
cd orchestrator && go build -o /tmp/boxcutter-orchestrator ./cmd/orchestrator/
scp -i ~/.ssh/id_rsa /tmp/boxcutter-orchestrator ubuntu@192.168.50.2:/tmp/
ssh -i ~/.ssh/id_rsa ubuntu@192.168.50.2 "sudo systemctl stop boxcutter-orchestrator && sudo cp /tmp/boxcutter-orchestrator /usr/local/bin/ && sudo systemctl start boxcutter-orchestrator"

# Host control plane
cd host && go build -o /tmp/boxcutter-host ./cmd/host/
sudo systemctl stop boxcutter-host && sudo cp /tmp/boxcutter-host /usr/local/bin/ && sudo systemctl start boxcutter-host
```

## Testing

```bash
cd node/vmid && go test ./...   # Unit tests (only module with tests currently)
```

Other modules are validated through integration testing on real VMs.

## Make Targets

```bash
make build-host              # Build boxcutter-host binary
make install-host            # Build + install + systemd setup
make deb-host                # Create .deb package
make provision-node          # Build node VM from source (cloud-init + QEMU)
make provision-orchestrator  # Build orchestrator VM from source
make build-image TYPE=node   # Build OCI-distributable QCOW2 image
make publish TYPE=node       # Build + push image to ghcr.io
make publish-all             # Build + push node + orchestrator images
make help                    # Show all targets
```

## Building and Publishing OCI Images

VM base images are distributed as OCI artifacts via `ghcr.io`. Images are public (anonymous pull).

```bash
# Build + push both images
make publish-all

# Build + push one type
make publish TYPE=node
make publish TYPE=orchestrator

# Build only (no push)
make build-image TYPE=node

# Custom tag
./host/publish-image.sh node --tag v1.0

# Push a previously built image
./host/publish-image.sh node --push-only
```

Pushing requires `gh auth login`.

The image build process boots a QEMU VM, installs all packages and binaries via cloud-init, then flattens the overlay to a standalone QCOW2 and compresses with zstd. Takes 20-30 minutes per image.

## Building the Deb Package

```bash
bash host/build-deb.sh 0.3.0   # explicit version
bash host/build-deb.sh          # uses git describe
```

Output: `.release/boxcutter-host_<version>_amd64.deb`

The deb includes:
- `/usr/local/bin/boxcutter-host`
- `/etc/systemd/system/boxcutter-host.service`
- `/usr/share/boxcutter/` (provision.sh, boxcutter.env, cloud-init templates, mosquitto.conf)

## Key Environment Variables

| Variable | Used by | Purpose |
|----------|---------|---------|
| `BOXCUTTER_REPO` | `boxcutter-host` | Path to repo checkout (dev mode). Without this, uses `/usr/share/boxcutter/` |
| `BOXCUTTER_BUNDLE` | `provision.sh` | Override path to secrets bundle (default: `~/.boxcutter/`) |
| `CLOUD_INIT_OUTPUT` | `provision.sh` | Override ISO output path |
| `CLOUD_INIT_IP` | `provision.sh` | Override node bridge IP |
| `CLOUD_INIT_MAC` | `provision.sh` | Override node MAC address |

## Architecture Documentation

- [docs/architecture.md](architecture.md) -- System overview, domain boundaries
- [host/docs/](../host/docs/) -- Host control plane internals
- [orchestrator/docs/](../orchestrator/docs/) -- Orchestrator internals
- [node/docs/](../node/docs/) -- Node agent, fwmark routing, vmid
