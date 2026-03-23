# VM Backend Architecture

The node agent supports two hypervisor backends — Firecracker and QEMU — through a common `VMBackend` interface. This document describes how the abstraction works and how the backends differ.

## VMBackend Interface

Defined in `node/agent/internal/vm/backend.go`:

```go
type VMBackend interface {
    Launch(vmDir string, st *VMState) (pid int, err error)
    LaunchIncoming(vmDir string, st *VMState) (pid int, err error)
    Pause(vmDir string) error
    Resume(vmDir string) error
    GracefulShutdown(vmDir string, pid int, timeout time.Duration) error
    CaptureState(vmDir string) (statePath string, err error)
    RestoreState(vmDir, statePath string) error
    CreateDisk(vmDir, goldenPath, diskSize string) error
    PrepareDisk(mgr *Manager, st *VMState)
    WriteConfig(vmDir string, st *VMState) error
    DiskName(vmDir string) string
    SyncBeforeCopy(st *VMState, sshKey string) (paused bool, err error)
}
```

`BackendFor(vmType string)` returns the appropriate backend based on the VM's type field ("firecracker" or "qemu"). This eliminates type-switching throughout the manager — only one branch remains (in `MigrateVM`, which dispatches to type-specific migration flows).

## Firecracker Backend (`fcbackend.go`)

| Method | Implementation |
|--------|---------------|
| `Launch` | Starts `firecracker` process with `--api-sock` and `--config-file` |
| `LaunchIncoming` | Not used (FC uses `ImportSnapshot` for migration) |
| `Pause` | `PATCH /vm {"state":"Paused"}` via Firecracker API socket |
| `Resume` | `PATCH /vm {"state":"Resumed"}` via Firecracker API socket |
| `GracefulShutdown` | `SendCtrlAltDel` via API, then SIGKILL fallback |
| `CaptureState` | `PUT /snapshot/create` → `vm.snap` + `vm.mem` files |
| `RestoreState` | Not used (FC uses `ImportSnapshot`) |
| `CreateDisk` | `CreateRootfs()` — sparse copy of golden ext4, truncate, resize2fs |
| `PrepareDisk` | Writes `fc-config.json` |
| `WriteConfig` | Regenerates `fc-config.json` (called on every start) |
| `DiskName` | Returns `"rootfs.ext4"` |
| `SyncBeforeCopy` | Pauses VM via `fcPause()` (returns `paused=true`) |

**Disk format:** Raw ext4 (`rootfs.ext4`), sparse-copied from the golden image.

**Migration:** Firecracker uses its native snapshot/restore API. The VM is paused, a snapshot is created (producing `vm.snap` for device state and `vm.mem` for RAM), files are transferred to the target, and a fresh Firecracker process loads the snapshot with `resume_vm: true`.

## QEMU Backend (`qemubackend.go`)

| Method | Implementation |
|--------|---------------|
| `Launch` | Starts `qemu-system-x86_64` with direct kernel boot, `-daemonize` |
| `LaunchIncoming` | Starts QEMU with `-incoming defer` (paused, waiting for state) |
| `Pause` | `{"execute":"stop"}` via QMP socket |
| `Resume` | `{"execute":"cont"}` via QMP socket |
| `GracefulShutdown` | SIGTERM (triggers ACPI shutdown), then SIGKILL fallback |
| `CaptureState` | `{"execute":"migrate"}` via QMP → saves full state to file |
| `RestoreState` | `{"execute":"migrate-incoming"}` via QMP → loads state from file |
| `CreateDisk` | `CreateQCOW2Rootfs()` — `qemu-img create -b <golden> -F qcow2` (instant) |
| `PrepareDisk` | Mounts rootfs, copies kernel modules, configures Docker/iptables/systemd |
| `WriteConfig` | No-op (QEMU config is embedded in launch args) |
| `DiskName` | Returns `"rootfs.qcow2"` (or `"rootfs.ext4"` for legacy) |
| `SyncBeforeCopy` | Runs `sudo sync` via SSH (returns `paused=false`) |

**Disk format:** QCOW2 with a golden backing file. Creation is instant (just metadata, no data copy). The golden QCOW2 is auto-converted from the golden ext4 image on first use.

**Migration:** Uses QMP (QEMU Monitor Protocol) over a Unix socket (`qmp.sock`). The VM is paused, full CPU/device/RAM state is saved to a file via the `migrate` command, the state file and disk are transferred to the target, a new QEMU is launched in incoming mode, and the state is loaded via `migrate-incoming`.

## Key Differences

| Aspect | Firecracker | QEMU |
|--------|-------------|------|
| Boot time | ~200ms | ~5-10s |
| Kernel | Minimal (no netfilter, no tun) | Full host kernel (all modules) |
| Docker support | No | Yes (auto-installed on first boot) |
| Disk format | Raw ext4 (sparse copy) | QCOW2 (COW backing file, instant) |
| Disk creation time | ~200ms (sparse copy + resize) | Instant (metadata only) |
| Migration mechanism | Snapshot/restore API | QMP state save/restore |
| Migration downtime (2GB) | ~2-3 seconds | ~6-8 seconds |
| Migration downtime (4GB) | ~5-7 seconds | ~10-12 seconds |
| Pause mechanism | Firecracker API (`/vm` PATCH) | QMP (`stop` command) |
| Control socket | `api.sock` (REST API) | `qmp.sock` (JSON-RPC) |
| Tailscale networking | Userspace (no CONFIG_TUN) | Kernel mode |
| Copy strategy | Pause VM, copy disk | `sync` filesystem, copy disk (VM stays running) |
| vsock support | Yes (migration nudge) | No |

## QMP (QEMU Monitor Protocol)

QMP is a JSON-based protocol for controlling QEMU, accessed via Unix socket at `<vmDir>/qmp.sock`.

Implementation in `node/agent/internal/vm/qmpapi.go`:

```go
// Connection: qmpDial(vmDir) → net.Conn to qmp.sock
// Commands: qmpCommand(conn, command, args) → sends JSON, reads response
// Key operations:
//   qmpStop(vmDir)     — pause vCPUs
//   qmpCont(vmDir)     — resume vCPUs
//   qmpSaveState(vmDir) — migrate to file (saves full VM state)
//   qmpLoadState(vmDir, path) — migrate-incoming from file
```

The QMP socket is added to QEMU launch args: `-qmp unix:<vmDir>/qmp.sock,server,nowait`

State save uses the QMP `migrate` command with `exec:cat > <file>` URI, which pipes the VM state through cat to a file. State load uses `migrate-incoming` with `exec:cat < <file>`.

## QEMU Direct Kernel Boot

QEMU VMs use direct kernel boot (no BIOS/bootloader):

```
-kernel /boot/vmlinuz-$(uname -r)
-initrd /boot/initrd.img-$(uname -r)
-append "console=ttyS0 root=/dev/vda rw init=/sbin/init ..."
```

The kernel and initrd are taken from the running node VM's `/boot/` directory. This ensures the guest kernel matches the kernel modules copied into the rootfs during `PrepareDisk`.

A dedicated QEMU kernel at `/var/lib/boxcutter/kernel/vmlinuz-qemu` takes priority if it exists.

## Files

| File | Responsibility |
|------|---------------|
| `backend.go` | `VMBackend` interface + `BackendFor()` factory |
| `fcbackend.go` | `FirecrackerBackend` implementation |
| `qemubackend.go` | `QEMUBackend` implementation |
| `qmpapi.go` | QMP client: dial, command, stop, cont, save, load, incoming launch |
| `qemu.go` | QEMU launch, kernel discovery, rootfs preparation |
| `storage.go` | Disk creation: `CreateRootfs()`, `CreateQCOW2Rootfs()`, dm-snapshot |
| `state.go` | `VMState`, `RootfsPath()`, `DiskFormat()`, `IsFileRootfs()` |
| `manager.go` | VM lifecycle manager (uses VMBackend for dispatch) |
