# Boxcutter Improvement Proposal

Based on a comprehensive architecture review (March 2026). The system architecture is fundamentally sound — three-tier separation with appropriate responsibilities. The improvements below target operational safety, observability, and code maintainability.

## Assessment Summary

**Strengths:**
- Clean three-tier architecture with appropriate boundaries
- Thin state at orchestrator (queries nodes for truth, avoids divergence)
- Idempotent infrastructure setup (bridge/NAT survives reboots)
- Snapshot-based migration preserving full VM state
- Loose coupling via MQTT for golden image distribution
- Boot recovery from cluster.json + /proc scanning

**Critical gaps:**
- No protection against multiple control planes running simultaneously
- No composite health checks (silent degradation)
- Single global mutex in node agent serializes all VM operations
- Monolithic code files (1866-2621 lines) in critical paths

## Phase 1: Safety (Priority: MUST)

### 1.1 Control Plane Lock

**Problem:** Two `boxcutter-host` instances can run simultaneously — both modify cluster.json, both manage VMs, both auto-scale. Results in data corruption and contradictory actions.

**Fix:** Use `flock` on a lock file at daemon startup.

```go
// host/cmd/host/main.go — at start of runDaemon()
lockFile, err := os.OpenFile("/run/boxcutter-host.lock", os.O_CREATE|os.O_RDWR, 0600)
if err != nil {
    log.Fatalf("Cannot create lock file: %v", err)
}
if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
    log.Fatalf("Another boxcutter-host is already running")
}
defer lockFile.Close()
```

**Effort:** 30 minutes.

### 1.2 Composite Health Checks

**Problem:** `/healthz` returns "ok" even when critical subsystems have failed. SQLite corruption, MQTT disconnection, dead Firecracker binary — all invisible.

**Fix:** Replace simple "ok" with real checks:

**Node agent** (`/api/health`):
- Verify Firecracker binary exists and is executable
- Verify golden image symlink resolves
- Verify vmid admin socket is reachable
- Report subsystem health in response

**Orchestrator** (`/api/health`):
- Verify SQLite DB is writable (`PRAGMA integrity_check` is too slow; use a test write)
- Verify MQTT connection is alive
- Report number of reachable nodes

**Host** (`boxcutter-host status`):
- Verify bridge exists with correct IP
- Verify NAT rules are in place
- Verify Mosquitto is running
- Verify at least one node is reachable

**Effort:** 3-4 hours.

### 1.3 Migration Timeout + Rollback

**Problem:** If migration fails halfway (network error mid-transfer), the VM is stuck in "migrating" state forever. No timeout, no rollback, no cleanup.

**Fix:**
- Add 60-second timeout to migration
- On timeout: resume source VM, remove marker file, clean up target
- Log migration phases with timestamps for debugging

**Effort:** 2-3 hours.

## Phase 2: Observability (Priority: SHOULD)

### 2.1 Structured Logging

**Problem:** All components use `log.Printf()` with unstructured strings. Can't query "how many VM creations failed in the last hour" or trace a request across components.

**Fix:** Migrate to Go's `log/slog` (stdlib since Go 1.21):

```go
slog.Info("vm created",
    "vm", req.Name,
    "node", nodeID,
    "duration_ms", elapsed.Milliseconds(),
    "ram_mib", req.RAMMIB,
)
```

Add request IDs to HTTP handlers (propagated via `X-Request-ID` header).

**Effort:** 4-6 hours across all components.

### 2.2 Better Health Reporting

**Problem:** Host health monitor only checks if QEMU PIDs are alive. Doesn't verify VMs are actually functional (SSH reachable, node agent responding).

**Fix:** Layer the health checks:
1. PID alive (current, fast)
2. Node agent HTTP reachable (add, 1s timeout)
3. SSH reachable (add, 2s timeout, only on failure of #2)

Record failure counts per node. After 3 consecutive failures, mark node as degraded and stop scheduling to it. After 5 failures, attempt restart.

**Effort:** 3-4 hours.

## Phase 3: Code Organization (Priority: COULD)

These changes improve maintainability but don't fix bugs. Do them when working in the relevant area, not as standalone refactors.

### 3.1 Split host/cmd/host/main.go (2621 lines)

Current: everything in one file.

Proposed split:
```
host/cmd/host/
├── main.go              — CLI entry point, flag parsing
├── daemon.go            — runDaemon(), signal handling
├── health.go            — healthLoop(), VM restart logic
├── scale.go             — autoScaleLoop(), nodeCapacity, canScaleUp
├── scale_test.go        — (existing)
├── bootstrap.go         — bootstrap command, first-time setup
├── upgrade.go           — upgrade command, rolling upgrade logic
├── drain.go             — drainNode(), migration coordination
├── api.go               — Unix socket API handlers
└── helpers.go           — Shared utilities (SSH, deploy binary, etc.)
```

### 3.2 Split orchestrator/internal/api/handlers.go (1120 lines)

Proposed split:
```
orchestrator/internal/api/
├── handlers.go          — Handler struct, router registration, health monitor
├── handlers_nodes.go    — Node register, heartbeat, list, get
├── handlers_vms.go      — VM CRUD, stop, start, copy, repos
├── handlers_golden.go   — Golden image queries/updates
├── handlers_keys.go     — SSH key management
└── handlers_migrate.go  — Orchestrator self-migration
```

### 3.3 Split node/agent/internal/vm/manager.go (1866 lines)

Proposed split:
```
node/agent/internal/vm/
├── manager.go           — Manager struct, Create(), Destroy(), ListVMs()
├── lifecycle.go         — Start(), Stop(), Restart(), RestartAll()
├── migration.go         — MigrateVM(), ImportSnapshot(), ExportSnapshot()
├── copy.go              — CopyVM()
├── prepare.go           — prepareRootfs(), injectCACert(), injectSSHKeys(), cloneRepos()
├── tailscale.go         — joinTailscale(), leaveTailscale()
├── firecracker.go       — writeFirecrackerConfig(), startVM()
├── health.go            — Health(), capacity checking
├── fcapi.go             — (existing) Firecracker socket API
├── network.go           — (existing) TAP + fwmark routing
├── state.go             — (existing) VMState persistence
├── storage.go           — (existing) Rootfs creation
└── ssh.go               — (existing) SSH key injection
```

### 3.4 Per-VM Locks (Performance)

**Problem:** Single `sync.Mutex` serializes all VM operations. Creating VM A (60s) blocks destroying VM B.

**Fix:** Use per-VM locks with a lock manager:

```go
type Manager struct {
    globalMu sync.Mutex            // Only for shared resources (mark allocation)
    vmLocks  map[string]*sync.Mutex // Per-VM locks
    locksMu  sync.Mutex            // Protects vmLocks map
    cfg      *config.Config
    vmid     *vmid.Client
}

func (m *Manager) lockVM(name string) func() {
    m.locksMu.Lock()
    mu, ok := m.vmLocks[name]
    if !ok {
        mu = &sync.Mutex{}
        m.vmLocks[name] = mu
    }
    m.locksMu.Unlock()
    mu.Lock()
    return mu.Unlock
}
```

Global mutex would only be held briefly for mark allocation, then released before the slow operations (rootfs copy, Tailscale join, repo cloning).

**Effort:** 4-6 hours including testing.

## Phase 4: Future Considerations (Not Planned)

These are noted for awareness but NOT recommended now:

- **Mutual TLS between components** — the bridge network is host-local, attack surface is small
- **MQTT authentication** — same reasoning, bridge is isolated
- **Prometheus metrics** — useful at scale, premature for single-host
- **Distributed tracing** — useful at scale, premature for single-host
- **Rate limiting** — current user base doesn't need it
- **PostgreSQL** — SQLite is fine for single-orchestrator

## Ownership Matrix

| Activity | Owner | Notes |
|----------|-------|-------|
| Create/destroy QEMU VMs | Host | Infrastructure layer |
| Bridge/TAP/NAT setup | Host | Bare metal only |
| Health monitor QEMU VMs | Host | Auto-restart on crash |
| Auto-scale nodes | Host | Launch/drain based on capacity |
| OCI image pull/push | Host | Infrastructure lifecycle |
| MQTT broker | Host | Runs Mosquitto subprocess |
| VM scheduling | Orchestrator | Pick node by free RAM |
| Node registry | Orchestrator | Nodes self-register |
| SSH key management | Orchestrator | Store + distribute at VM create |
| Golden image head | Orchestrator | Publish version via MQTT |
| User SSH interface | Orchestrator | ForceCommand dispatch |
| Firecracker VM lifecycle | Node | Create, start, stop, destroy |
| VM migration execution | Node | Snapshot, transfer, restore |
| Golden image storage | Node | Pull from OCI, version management |
| Health reporting | Node | RAM, vCPU, disk metrics |

## State Consistency

The system uses a **pull-based consistency model**:
- Orchestrator DB is a thin cache, rebuilt every 30s from node queries
- Host cluster.json is the persistent record of QEMU VMs
- Node vm.json files are the source of truth for Firecracker VMs

**Failure modes and recovery:**

| Failure | Detection | Recovery |
|---------|-----------|----------|
| Firecracker crash | Node detects dead PID | RestartAll() on node agent restart |
| Node VM crash | Host health monitor (10s) | Auto-restart QEMU from cluster.json |
| Orchestrator crash | Host health monitor (10s) | Auto-restart QEMU, DB rebuilt from nodes |
| Host crash | N/A (VMs keep running) | On reboot: bridge setup + VM relaunch |
| OOM kill | Host health monitor | Auto-restart, but root cause needs manual fix |
| Disk full | Scale-up check (host), df check (node health) | Scale-up blocked, admin alert needed |
