# Rolling Upgrade & Migration

This document describes how boxcutter performs zero-downtime rolling upgrades of node VMs and the orchestrator, including VM migration during node drain.

## Overview

Rolling upgrades use a **reconciliation loop** (Kubernetes controller pattern). An `UpgradeGoal` in `cluster.json` declares the desired image version. The reconciler observes reality and takes one convergence step per iteration until all VMs match the goal.

**Crash recovery is free**: on daemon restart, if `UpgradeGoal` exists, the reconciler resumes from wherever it left off by re-observing cluster state.

## Node Lifecycle

```mermaid
stateDiagram-v2
    [*] --> active: Launched
    active --> upgrading: Reconciler marks for drain
    upgrading --> draining: Drain starts
    draining --> [*]: Drain complete (removed)
    
    draining --> active: Drain failed (retried next cycle)
    draining --> [*]: Unreachable 3x (force-removed)
    
    note right of active: Health monitor restarts if QEMU dies
    note right of upgrading: Health monitor ignores (won't restart)
    note right of draining: VMs being migrated off
```

## Upgrade State Machine

```mermaid
flowchart TD
    A[UpgradeGoal set] --> B{Node image pulled?}
    B -->|No| C[Pull node image from OCI]
    C --> B
    B -->|Yes| D{Orch image pulled?}
    D -->|No| E[Pull orchestrator image from OCI]
    E --> D
    D -->|Yes| F{All nodes match goal?}
    
    F -->|No| G{Replacement node exists?}
    G -->|No| H[Launch replacement node]
    H --> G
    G -->|Yes| I{Replacement healthy?}
    I -->|No| J[Deploy binary + wait]
    J --> I
    I -->|Yes| K{Golden image ready?}
    K -->|No| L[Wait up to 10 min]
    L --> K
    K -->|Yes| M[Mark old node 'upgrading']
    M --> N[Drain old node]
    N --> F
    
    F -->|Yes| O{Orchestrator needs upgrade?}
    O -->|No| P[Clear goal - DONE]
    O -->|Yes| Q[Launch new orchestrator]
    Q --> R[Migrate state from old]
    R --> S[Stop old orchestrator]
    S --> P
```

## Drain Process

Drain migrates all VMs off a node, then stops and removes it.

```mermaid
flowchart TD
    A[Start drain] --> B[Get VM list from node agent]
    B -->|Unreachable| C{Failed 3+ times?}
    C -->|Yes| D[Force kill + remove]
    C -->|No| E[Increment fail count, retry next cycle]
    
    B -->|0 VMs| F[Remove from state + stop QEMU]
    
    B -->|Has VMs| G[Pick target node - most free RAM]
    G --> H[Sort VMs by size - smallest first]
    H --> I[Fire batch of 3 migrations]
    
    I --> J[Poll source every 3s per VM]
    J --> K{Source status?}
    K -->|migrating| J
    K -->|migrated| L[Verify on target]
    K -->|404| L
    K -->|running| M[Migration failed]
    K -->|unreachable 2min| L
    
    M --> N[Retry failed VMs once]
    N -->|All passed| O[Stop node + cleanup]
    N -->|Still failing| E
    
    L --> O
    O --> P[Remove disk, TAP, ISO, logs]
```

## VM Migration

### Firecracker (snapshot/restore)

```mermaid
sequenceDiagram
    participant H as Host Daemon
    participant S as Source Node Agent
    participant T as Target Node Agent
    
    H->>S: POST /api/vms/{name}/migrate
    S-->>H: 202 Accepted (async)
    
    Note over S: Phase 1: Pre-sync (VM running)
    S->>T: tar --sparse rootfs over SSH
    
    Note over S: Phase 2: Snapshot (VM paused)
    S->>S: Pause VM (~1ms)
    S->>S: Snapshot to /dev/shm
    S->>T: Transfer vm.snap + vm.mem
    S->>T: POST /api/vms/{name}/import-snapshot
    T->>T: Restore VM from snapshot
    
    Note over S: Phase 3: Verify + commit
    S->>T: GET /api/vms/{name} (check running)
    S->>S: Set 'migrated' marker
    S->>S: Stop source VM
    S->>S: Remove vmDir
    
    H->>S: GET /api/vms/{name} (poll)
    S-->>H: 404 (removed) or status: migrated
```

### QEMU (state save/restore)

```mermaid
sequenceDiagram
    participant H as Host Daemon
    participant S as Source Node Agent
    participant T as Target Node Agent
    
    H->>S: POST /api/vms/{name}/migrate
    S-->>H: 202 Accepted (async)
    
    Note over S: Phase 1: Pre-sync disk (VM running)
    S->>T: rsync/tar rootfs over SSH (~60s for 10GB)
    
    Note over S: Phase 2: Save state (VM paused)
    S->>S: QMP stop (pause)
    S->>S: QMP savevm (state to disk, ~8s for 4GB)
    S->>T: Transfer state file over SSH (~3s)
    S->>T: POST /api/vms/{name}/import-snapshot
    T->>T: Launch QEMU + load state
    
    Note over S: Phase 3: Verify + commit
    S->>T: GET /api/vms/{name} (check running, up to 60s)
    S->>S: Set 'migrated' marker
    S->>S: Stop source QEMU
    S->>S: Remove vmDir
    
    H->>S: GET /api/vms/{name} (poll)
    S-->>H: 404 (removed) or status: migrated
```

## Drain Poll Status Machine

The host daemon polls the source node every 3 seconds during drain:

```mermaid
stateDiagram-v2
    [*] --> Polling
    Polling --> Polling: status = migrating
    Polling --> Polling: status = stopped (ambiguous, keep waiting)
    Polling --> Verified: status = migrated
    Polling --> Verified: HTTP 404 (VM removed)
    Polling --> Failed: status = running (migration didn't start)
    Polling --> Verified: unreachable for 2 minutes
    
    Verified --> [*]: Check target has running copy
    Failed --> Retry: Single retry per VM
    Retry --> [*]: Success or give up
```

## Orchestrator Upgrade

Blue-green deployment with live state migration:

```mermaid
flowchart TD
    A[Assign temp IP for new orch] --> B[Generate cloud-init ISO]
    B --> C[Create COW disk from base image]
    C --> D[Launch new orch QEMU at temp IP]
    D --> E{New orch healthy?}
    E -->|No| F[Wait for boot]
    F --> E
    E -->|Yes| G[POST /api/migrate to new orch]
    G --> H[New orch pulls state from old]
    H --> I[Stop old orch QEMU]
    I --> J[Update cluster.json with new orch]
    J --> K[Done]
```

## Key Timeouts

| Component | Timeout | Purpose |
|-----------|---------|---------|
| Migration HTTP POST | 30s | Fire migration request |
| Status poll | 5s per request, 3s interval | Check VM status |
| Inactivity | 2 minutes | Assume migrated if source unreachable |
| Migration retry | 3 minutes per VM | Second attempt for failed VMs |
| Golden image build | 10 minutes | Wait for Docker build + ext4 conversion |
| Orchestrator stop | 60 seconds | Wait for old orch to finish migration |
| Drain failure limit | 3 attempts | Force-remove unreachable nodes |

## Failure Recovery

| Scenario | Behavior |
|----------|----------|
| Daemon crashes mid-upgrade | Restarts, sees UpgradeGoal in cluster.json, resumes |
| Node dies during drain | After 3 unreachable drain attempts, force-removed |
| Migration fails | Retried once; if still failing, drain aborted and retried next cycle |
| Golden image stuck | 10-minute timeout, then error (upgrade pauses, retries in 30s) |
| Target node dies mid-migration | Source VM stays running (rollback), migration retried |
| Orchestrator migration fails | Error logged, retried next reconcile cycle |

## State Persistence

All state is in `/var/lib/boxcutter/cluster.json`, written atomically (write → fsync → rename). The reconciler can crash at any point and resume correctly by re-reading the file.

Key fields:
- `nodes[].status`: "active", "upgrading", "draining"
- `nodes[].image_digest`: OCI manifest digest (immutable comparison)
- `nodes[].drain_fail_count`: tracks repeated drain failures
- `upgrade_goal`: declarative target state (cleared when complete)
- `upgrade_goal.node_image.digest`: target image digest for comparison
