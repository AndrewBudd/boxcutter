# Development Guide

How to work on Boxcutter itself — building from source, deploying changes, and publishing images.

## Prerequisites

- A running Boxcutter cluster (see [README](README.md) for bootstrap)
- Go 1.24+ at `/usr/local/go`
- `qemu-system-x86_64`, `qemu-utils`, `genisoimage`, `zstd`
- `gh` CLI (for pushing OCI images)

## Code Layout

Boxcutter is five independent Go modules plus shell scripts:

```
boxcutter/
├── host/                        # Control plane (runs on bare metal)
│   ├── cmd/host/main.go         # boxcutter-host binary
│   ├── internal/                # bridge, cluster, qemu, oci packages
│   ├── docs/                    # Host-specific architecture, network, dev docs
│   └── go.mod
│
├── orchestrator/                # Scheduling, SSH interface, key management
│   ├── cmd/orchestrator/        # HTTP API server (:8801)
│   ├── cmd/ssh/                 # SSH ForceCommand binary
│   ├── internal/                # api, config, db, mqtt, scheduler, ssh
│   ├── docs/                    # Orchestrator-specific architecture, dev docs
│   └── go.mod
│
├── node/
│   ├── agent/                   # Node agent (Firecracker VM lifecycle)
│   │   ├── cmd/node/main.go     # boxcutter-node binary (:8800)
│   │   ├── internal/            # api, vm, golden, network, mqtt, vmid, config
│   │   └── go.mod
│   │
│   ├── vmid/                    # VM identity & token broker
│   │   ├── cmd/vmid/main.go     # vmid binary (:80)
│   │   ├── internal/            # api, config, middleware, registry, sentinel, token
│   │   └── go.mod
│   │
│   ├── proxy/                   # MITM forward proxy
│   │   ├── cmd/proxy/main.go    # boxcutter-proxy binary (:8080)
│   │   └── go.mod
│   │
│   ├── golden/                  # Firecracker rootfs builder
│   ├── scripts/                 # Shell scripts installed on nodes
│   ├── systemd/                 # Service unit files
│   └── docs/                    # Node-specific architecture, network, dev docs
│
├── docs/                        # Top-level documentation
├── Makefile                     # Top-level build targets
└── .github/workflows/           # CI/CD
```

Each Go module is independent — you can `cd` into any module directory and run `go build`, `go test`, etc. without affecting others.

## Building

### Make targets

```bash
make build-host          # Build boxcutter-host
make install-host        # Build + install to /usr/local/bin + systemd
make help                # Show all targets
```

### Individual binaries

```bash
cd host && go build -o boxcutter-host ./cmd/host/
cd orchestrator && go build -o boxcutter-orchestrator ./cmd/orchestrator/
cd orchestrator && go build -o boxcutter-ssh-orchestrator ./cmd/ssh/
cd node/agent && go build -o boxcutter-node ./cmd/node/
cd node/vmid && go build -o vmid ./cmd/vmid/
cd node/proxy && go build -o boxcutter-proxy ./cmd/proxy/
```

All binaries target `linux/amd64`.

### Tests

```bash
cd node/vmid && go test ./...
```

Tests exist for the vmid registry (mark allocation) and sentinel store (token management). Other modules are validated through integration testing on real VMs.

## Development Workflow

The fastest way to iterate depends on which component you're changing. See the domain-specific development guides:

- **Host control plane** → [host/docs/development.md](../host/docs/development.md)
- **Orchestrator** → [orchestrator/docs/development.md](../orchestrator/docs/development.md)
- **Node services** → [node/docs/development.md](../node/docs/development.md)

## SSH Access to VMs

```bash
make ssh-node              # SSH to node-1
make ssh-orchestrator      # SSH to orchestrator
bash host/ssh.sh node 1    # node-1
bash host/ssh.sh node 2    # node-2

# Direct
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2   # orchestrator
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.3   # node-1
```

## CI/CD

GitHub Actions workflow at `.github/workflows/build-image.yml`:

- **Triggers**: git tags (`v*`) or manual `workflow_dispatch`
- **Runs on**: self-hosted runner with KVM support
- **Matrix**: builds `node` and `orchestrator` images in parallel
- **Steps**: checkout → Go setup → install deps → build image → push to ghcr.io → upload artifact

The workflow requires a self-hosted runner because image builds need KVM access to boot QEMU VMs.

## Domain-Specific Guides

| Domain | Development Guide | Architecture |
|--------|------------------|-------------|
| Host | [host/docs/development.md](../host/docs/development.md) | [host/docs/architecture.md](../host/docs/architecture.md) |
| Orchestrator | [orchestrator/docs/development.md](../orchestrator/docs/development.md) | [orchestrator/docs/architecture.md](../orchestrator/docs/architecture.md) |
| Node | [node/docs/development.md](../node/docs/development.md) | [node/docs/architecture.md](../node/docs/architecture.md) |
