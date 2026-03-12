# Migration Hardening Plan

## Objective
Battle-harden the VM migration process. Make it fast, reliable, and resilient to failures.

## Current Problems (all resolved)
1. ~~**Migration is too slow** — 2GB VMs take 4-12 minutes~~ → **7.6s downtime**
2. ~~**Data transfer over local bridge is inexplicably slow**~~ → **400MB/s achieved**
3. **Orchestrator upgrade loses Tailscale identity** — migration request times out (TODO)
4. **Upgrade reconciler gets confused** — state file drift after failed upgrades (TODO)
5. ~~**VMs experience minutes of downtime**~~ → **1.5-7.6s downtime**

## Tailscale Keys (ephemeral)
- Orchestrator: `REDACTED_TAILSCALE_KEY_ORCHESTRATOR`
- Nodes: `REDACTED_TAILSCALE_KEY_NODES`

## Phase 1: Diagnose Transfer Speed (complete)
- [x] Benchmark raw network throughput between nodes — SSH ~400MB/s on local bridge
- [x] Profile the migration pipeline: where is time spent?
- [x] Identify the bottleneck

### Root Cause Found
**rsync reads the ENTIRE 50GB sparse file** (not just 6.5GB allocated blocks) to compute block checksums. It does this TWICE — once for pre-sync, once for delta. That's ~200GB of I/O for a 6.5GB actual transfer.

## Phase 2: Optimize Migration Speed (complete)
- [x] Replace rsync with `tar --sparse` for disk transfers — uses SEEK_DATA/SEEK_HOLE (~7x less I/O)
- [x] Add SSH ControlMaster — one key exchange shared by all transfers
- [x] Replace `cat|ssh|tee` with `dd bs=4M|ssh|dd bs=4M` for memory transfer
- [x] Write snapshots to `/dev/shm` (tmpfs) on SOURCE — avoids ~37MB/s QCOW2 write
- [x] Write snapshots to `/dev/shm` on TARGET — avoids disk I/O contention from pre-sync writeback
- [x] Sequential snap→mem transfer — avoids SSH ControlMaster head-of-line blocking
- [x] Skip disk delta after pause — page cache in vm.mem covers recently-written blocks
- [x] Add per-step timing instrumentation to every migration phase
- [x] Speed up health check polling — immediate first check, 2s intervals
- [x] Clean up /dev/shm files on completion and rollback (both source and target)
- [x] Target: <5s downtime for a 512MB VM on local bridge → **achieved: 1.5s**

### Benchmark Results

| VM | RAM | Disk | Original | v5 (final) | Speedup |
|----|-----|------|----------|------------|---------|
| test-migrate-1 | 512MB | 1.1GB | 8+ min | **1.5s** | **320x** |
| salty-vole | 2GB | 6.5GB | 2m46s | **7.6s** | **22x** |

**v5 Breakdown (salty-vole, 2GB RAM, 6.5GB rootfs):**
- Pre-sync (zero downtime): 36s-1m38s (varies, doesn't affect downtime)
- Pause: 0ms
- Snapshot creation: 2.1s (to /dev/shm)
- Snap transfer: 22ms
- Mem transfer: 4.8s (shm→shm, 400MB/s)
- Import + resume: 616ms
- Health verify: 1ms
- **Total downtime: 7.6s**

**v5 Breakdown (test-migrate-1, 512MB RAM, 1.1GB rootfs):**
- Pre-sync (zero downtime): 3.7-5s
- Pause: 0ms
- Snapshot creation: 261ms
- Snap transfer: 24ms
- Mem transfer: 943ms
- Import + resume: 119ms
- **Total downtime: 1.3-2.6s**

### Optimization History
1. **v1** (tar --sparse + dd + ControlMaster): 24s downtime for 512MB (from 8+ min)
2. **v2** (+ /dev/shm source snapshots): 5.6s for 512MB; 2m46s for 2GB (disk delta bottleneck)
3. **v3** (skip disk delta): 33s for 2GB (target disk I/O contention)
4. **v4** (/dev/shm on target): mem 5s, but snap regressed to 35s (ControlMaster HOL blocking)
5. **v5** (sequential snap→mem): **7.6s for 2GB, 1.5s for 512MB** (final)

### Key Insight: Why v3→v4 Snap Regressed
When both transfers write to /dev/shm (fast), they both push data through the SSH ControlMaster's multiplexed TCP connection. The 2GB mem transfer dominates the TCP channel, causing the tiny snap to queue behind it (head-of-line blocking). Fix: send snap first (22ms), then mem (5s).

## Phase 2.5: Hardening Tests (complete)
- [x] Concurrent migration of 2 VMs — works, ~3s downtime each
- [x] Rapid back-and-forth ping-pong migration — consistent 1.3-2.6s
- [x] Fresh VM migration immediately after creation — 2.1s downtime
- [x] Migration to unreachable target — fails fast at SSH, VM keeps running
- [x] Migration of non-existent VM — returns 404 (was: silent 202 + async failure)
- [x] Double migration attempt — returns 409 Conflict
- [x] Target agent down during migration — rollback triggered, source VM resumed
- [x] Orphaned file cleanup on target after rollback — verified clean

### Bugs Found and Fixed
1. **Migrate non-existent VM returned 202** — handler started async migration without validating VM exists. Fixed: added LoadVMState check, returns 404.
2. **Double migration race** — no guard against triggering migration on already-migrating VM. Fixed: added IsMigrating check, returns 409 Conflict.

## Phase 2.6: Advanced Hardening (complete)
- [x] Stale migration recovery — agent restart resumes paused VMs (KillMode=process)
- [x] Zero-downtime agent restarts — VMs survive `systemctl restart` (KillMode=process)
- [x] /dev/shm exhaustion — adaptive: uses tmpfs when space permits, falls back to disk
- [x] Self-migration guard — returns 400, prevents VM destruction from rm -rf same-dir
- [x] Source /dev/shm cleanup — partial files cleaned after failed snapshot attempt
- [x] 4-way concurrent migration — all succeed, 2-5s downtime each
- [x] Bidirectional cross-migration — 4 VMs swapped between nodes simultaneously
- [x] Rapid ping-pong (3 round-trips) — consistent 1.7-2.0s downtime for 512MB
- [x] Target agent down pre-migration — connection refused → rollback → VM running
- [x] Agent crash mid-migration — systemd cgroup cleanup kills FC → VM restarts fresh

### Bugs Found and Fixed
3. **Firecracker mmaps /dev/shm permanently** — snapshot restore mmaps vm.mem from /dev/shm. Even after os.RemoveAll, the unlinked file's tmpfs space stays consumed until FC exits. Migrating 4 VMs (5.5GB total) filled /dev/shm, causing subsequent snapshots to fail with ENOSPC. Fixed: check target /dev/shm space before writing, fall back to disk.
4. **fcSnapshot leaks partial files** — failed /dev/shm write left partial vm.mem (e.g., 1.9GB for a 2GB VM), further reducing available space. Fixed: os.RemoveAll(shmDir) before fallback retry.
5. **Self-migration destroys VM** — migrating VM to same node shares vmDir. Rollback's `rm -rf dstVMDir` deletes source files. Fixed: reject when target matches local bridge IP.

## Phase 2.7: Concurrency & Performance (complete)
- [x] Snapshot path collision — fcSnapshot uses `-mig` suffix to avoid writing to Firecracker's mmapped file
- [x] Manager mutex blocks import-snapshot — reduced lock scope: Create/Start release mutex before SSH/Tailscale waits
- [x] VM resurrection after migration — postStartVM checks IsRunning before SSH/Tailscale operations
- [x] Guest memory prefault — reads /proc/<pid>/mem after snapshot restore to pre-populate PTEs
- [x] Concurrent cross-migration — both directions succeed simultaneously (was: timeout + split-brain)
- [x] Concurrent create + inbound migration — both succeed (was: 18.9s import-snapshot blocked on mutex)

### Bugs Found and Fixed
6. **Snapshot path collision** — fcSnapshot wrote to same `/dev/shm/bc-<name>/` path as mmapped import file. Firecracker's COW + page faults caused 30s+ snapshot creation for 512MB VMs. Fixed: use `-mig` suffix for snapshot output directory.
7. **Manager mutex blocks concurrent operations** — Create() held global mutex for 30+ seconds during SSH/Tailscale waits. ImportSnapshot on same node blocked until Create released lock. Cross-migration timed out after 2 minutes, leaving split-brain state (VM on both nodes). Fixed: split Create/Start into locked setup + unlocked post-start phases. Mutex now held <1 second.
8. **VM resurrected after migration** — Create's postStartVM continued SSH/Tailscale operations even after VM was migrated away, restarting the Firecracker process. Fixed: added IsRunning checks in postStartVM to abort if VM was deleted/migrated.
9. **25s snapshot creation for restored VMs** — After snapshot restore, Firecracker's MAP_PRIVATE mmap has 131K lazy page faults (512MB = 25s, 2GB = 100s+). Fixed: background goroutine reads /proc/<pid>/mem to force get_user_pages() PTE population. 512MB: 25s → 200ms. 2GB: 100s+ → 850ms.

### Performance Notes
- **Prefault overhead**: ~145ms for 512MB, ~500ms for 2GB (background, doesn't block import response)
- **Cross-migration now works**: Both VMs migrate simultaneously, ~1.5s downtime each
- **Concurrent create + migrate**: No blocking, import-snapshot completes in ~120ms
- **Disk fallback penalty**: ~36s for 2GB VM (vs 5s with tmpfs). Acceptable for exhaustion scenarios.

## Phase 2.8: Agent Restart & Network Resilience (complete)
- [x] Orphaned SSH ControlMaster cleanup — killed on agent restart via `pkill` + socket removal
- [x] Orphaned target directory cleanup — removed VM dirs without vm.json on agent restart
- [x] SSH keepalive — ServerAliveInterval=10/CountMax=3 detects dead connections in ~30s
- [x] Source agent restart during migration — stale marker recovery resumes paused VM
- [x] Target agent restart during migration — source rollback + resume, target cleaned on restart
- [x] Network partition during mem transfer — SSH keepalive detects, rollback in ~39s (was: indefinite)
- [x] /dev/shm full on target — adaptive disk fallback, 1.594s downtime
- [x] Migration under heavy guest I/O — succeeds, snapshot time proportional to dirty pages
- [x] Double migration (immediate re-migrate) — both legs succeed
- [x] 5-way simultaneous cross-migration — all 5 VMs arrive correctly
- [x] DELETE during migration — returns 409 Conflict (was: race condition + orphaned files)
- [x] Migration to unreachable target — fails fast (3s, "No route to host"), VM stays running
- [x] Full node drain (4 VMs sequential) — all succeed, 1.5-3.4s downtime each, 92s total
- [x] Create VM during active migration — no mutex contention, both succeed
- [x] End-to-end state preservation — files + processes + uptime survive migration

### Bugs Found and Fixed
10. **Orphaned SSH ControlMaster** — `ssh -fN` ControlMaster processes survive agent restart (KillMode=process only kills main Go process). Accumulate indefinitely, holding open SSH connections. Fixed: `pkill -f ssh.*ControlPath=/tmp/bc-migrate-` on startup + socket file cleanup.
11. **Orphaned target directories** — interrupted migrations leave partial VM directories on target (rootfs.ext4 from pre-sync, no vm.json). Waste disk space and confuse subsequent operations. Fixed: `cleanupMigrationArtifacts` removes dirs without vm.json on agent restart.
12. **No transfer timeout (network partition)** — if network drops during mem transfer, SSH blocks indefinitely waiting for TCP keepalive (~15min). VM stays paused the entire time. Fixed: added `ServerAliveInterval=10 ServerAliveCountMax=3` to migration SSH options, detecting dead connections in ~30s.
13. **DELETE succeeds during active migration** — Destroy() didn't check IsMigrating, causing race: migration goroutine continues after VM directory is deleted, leaving orphaned files on target with no cleanup. Fixed: Destroy returns error if IsMigrating, handler returns 409 Conflict.

### Stress Test Results (5-way simultaneous)
| VM | RAM | Direction | Downtime | Notes |
|----|-----|-----------|----------|-------|
| test-a | 512MB | node-5→node-4 | 6.3s | First to complete |
| test-d | 512MB | node-5→node-4 | 36.5s | I/O contention |
| test-f | 512MB | node-5→node-4 | 37.4s | I/O contention |
| test-g | 2048MB | node-5→node-4 | 42.1s | Large VM + contention |
| shiny-egret | 512MB | node-4→node-5 | 1m15s | Sending node busy receiving 4 VMs |

## Phase 3: Fix Orchestrator Migration (TODO)
- [x] Switch to ephemeral Tailscale keys — separate keys for orchestrator and nodes
- [x] Fixed `provision.sh` to include ALL authorized SSH keys
- [x] Added 30s timeout to `tailscale up` in `boxcutter-setup`
- [ ] Fix orchestrator migration: rsync state including Tailscale
- [ ] Increase health check timeout / add retries
- [ ] Test full orchestrator upgrade cycle end-to-end

## Phase 4: Upgrade Reconciler Robustness (TODO)
- [ ] Fix state tracking for replacement nodes
- [ ] Handle partial failures gracefully (don't lose track of VMs)
- [ ] Add idempotent recovery (re-run should always converge)

## Phase 5: Stress Testing (TODO)
- [x] 5+ VMs concurrent migration — tested: 5 VMs simultaneously, all completed
- [ ] Rolling node upgrade with live VMs
- [x] Failure injection (kill source/target mid-migration) — tested both, correct recovery
- [x] /dev/shm exhaustion (VM too large for tmpfs) — graceful fallback to disk
- [x] Network partition during migration — rollback in ~39s (was: indefinite)
- [x] Source/target agent restart during migration — correct recovery in all cases
