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

## Phase 2.9: Code Audit & Systematic Hardening (complete)
- [x] Pre-sync failure now fatal — was silently ignored, causing corrupted disk on target
- [x] ImportSnapshot rejects existing running VM — prevents state corruption
- [x] Atomic migration guard — in-memory sync.Mutex set prevents race between concurrent migrate requests
- [x] Stop during migration returns 409 — was unguarded, could kill VM mid-migration
- [x] relocateStoppedVM verifies target files before deleting source — prevents data loss
- [x] relocateStoppedVM checks IsRunning before delete — prevents deleting rootfs under concurrently-started VM
- [x] ImportSnapshot error paths release loop devices (CleanupSnapshot) — prevents leaked /dev/loop*
- [x] ImportSnapshot error paths remove vmDir — prevents orphaned dirs confusing RestartAll
- [x] /dev/shm cleanup on migration commit and destroy — was leaking 512MB+ per migration
- [x] /dev/shm orphan cleanup on agent restart — removes stale dirs for deleted VMs
- [x] Removed contradictory double-delete of snapshot files in ImportSnapshot
- [x] MigrateVM commit cleans up dm-snapshot resources before removing source files
- [x] relocateStoppedVM cleans up dm-snapshot resources before removing source files
- [x] Bidirectional 3-VM cross-migration — all succeed simultaneously
- [x] Rapid ping-pong (3 round trips, 6 total migrations) — all succeed
- [x] Agent restart during migration — VM correctly resumed from paused state
- [x] Concurrent migrate race test — second request gets 409, first completes
- [x] SSH ConnectTimeout=10 on all migration SSH commands — prevents indefinite hang
- [x] Firecracker crash detection in ImportSnapshot — reports actual crash reason
- [x] API socket ready timeout — fails fast if FC doesn't start within 5s
- [x] HTTP timeout on golden version check — prevents hanging on unreachable target
- [x] Stop() checks both in-memory and filesystem migrating marker
- [x] /dev/shm completely clean after migration commit + destroy verified
- [x] Full node drain (3 VMs including 2GB) — 26s total, all healthy
- [x] Comprehensive 9-test regression suite — all pass

### Bugs Found and Fixed
14. **ImportSnapshot /dev/shm leak** — multiple error paths in ImportSnapshot didn't clean up /dev/shm/bc-<name>. Fixed: added os.RemoveAll(shmDir) to all error returns.
15. **ImportSnapshot leaks loop devices** — error paths after EnsureSnapshot didn't call CleanupSnapshot, leaking /dev/loop* and dm-snapshot targets. Fixed: added CleanupSnapshot to all error paths.
16. **ImportSnapshot orphans vmDir** — error paths left vmDir with vm.json, causing RestartAll to try restarting a broken VM. Fixed: os.RemoveAll(vmDir) on all error paths.
17. **Pre-sync failure silently ignored** — if tar --sparse pre-sync failed, migration continued. Target had empty/partial disk, causing data corruption after snapshot restore. Fixed: pre-sync failure is now fatal.
18. **ImportSnapshot overwrites existing VM** — no check for existing running VM. Could corrupt state and orphan processes. Fixed: reject if IsRunning(vmDir).
19. **Duplicate migration race** — two concurrent migrate requests both pass IsMigrating check (TOCTOU). Both proceed, causing competing snapshots. Fixed: atomic StartMigration() with sync.Mutex.
20. **Stop during migration unguarded** — Stop() didn't check IsMigrating. Killing VM mid-migration causes snapshot failure + confusing rollback. Fixed: Stop() returns error if migrating.
21. **relocateStoppedVM deletes source without verifying target** — tar transfer could silently fail, source deleted, data lost. Fixed: verify target files exist before source cleanup.
22. **relocateStoppedVM concurrent Start race** — concurrent Start() could launch VM while relocateStoppedVM is transferring files, then RemoveAll deletes rootfs under running VM. Fixed: IsRunning check before delete.
23. **relocateStoppedVM leaks dm-snapshot** — os.RemoveAll without CleanupSnapshot leaves loop devices and dm targets. Fixed: CleanupSnapshot before RemoveAll.
24. **/dev/shm leaked after migration commit** — source /dev/shm/bc-<name>/ (512MB+ mmapped import file) persisted after VM migrated away. Fixed: explicit cleanup in commit phase.
25. **/dev/shm leaked on destroy** — same issue as #24 but for Destroy(). Fixed: os.RemoveAll in Destroy.
26. **Orphaned /dev/shm dirs on restart** — stale empty /dev/shm/bc-* directories from previous VMs accumulated. Fixed: cleanupMigrationArtifacts removes dirs for non-existent VMs.
27. **No SSH ConnectTimeout** — unreachable target caused SSH commands to block indefinitely during migration. Fixed: ConnectTimeout=10 on all migration SSH commands.
28. **Stop() inconsistent migrating check** — Stop() only checked in-memory migratingSet, not filesystem marker. A stale marker from agent crash allowed Stop() to kill a VM during recovery. Fixed: check both.
29. **ImportSnapshot silent FC crash** — if Firecracker crashed on startup, ImportSnapshot waited 5s then failed with confusing "connection refused" from snapshot load. Fixed: detect process exit during socket wait, report actual crash log.

### Comprehensive Test Results
All 9 regression tests pass:
1. Basic migration (node-4 → node-5): PASS
2. Migrate back (node-5 → node-4): PASS
3. Concurrent duplicate migration: PASS (409)
4. Stop during migration: PASS (409)
5. Delete during migration: PASS (409)
6. Self-migration: PASS (400)
7. Non-existent VM: PASS (404)
8. ImportSnapshot existing VM: PASS (rejected)
9. Migration to unreachable target: PASS (fail + recover in 10s)

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

## Phase 3: Hardening Session (2026-03-12)

### Bugs Found & Fixed

| # | Bug | Severity | Fix |
|---|-----|----------|-----|
| 30 | Start() has no migration guard | Medium | Added `IsMigratingVM` + `IsMigrating` check, returns 409 |
| 31 | Start handler returns 500 instead of 409 for migrating VM | Low | Added string match like Stop/Destroy handlers |
| 32 | Rollback cleanup silently fails when ControlMaster is dead | High | Falls back to direct SSH when ControlMaster fails |
| 33 | Phase 1 failures (pre-sync/pause) don't clean target | High | `cleanTarget()` moved before Phase 2, called on all Phase 1 errors |
| 34 | Relocated stopped VM can't Start (missing fc-config.json) | Critical | `startSetup()` now regenerates `fc-config.json` every time |
| 35 | `ensureTargetHasGolden` has no SSH keepalive | Medium | Added `ServerAliveInterval=10 ServerAliveCountMax=3` |
| 36 | `relocateStoppedVM` has no SSH keepalive | Medium | Added to all SSH commands |
| 37 | `relocateStoppedVM` doesn't clean target on failure | High | Added `cleanTarget()` for tar, verify, and concurrent-start failures |
| 38 | Name collision: migrating to target with same VM name corrupts target | **Critical** | Pre-flight API check returns 409 before any file transfer |
| 39 | `ImportSnapshot` only rejects running VMs, not stopped ones | High | Now checks `LoadVMState` existence before `IsRunning` |
| 40 | `ImportSnapshot` stale TAP rules cause SetupTAP failure | High | Added `TeardownTAP` before `SetupTAP` (matches `startSetup` behavior) |
| 41 | Verify loop comment says "30 seconds" but runs 60s | Low | Fixed comment |

### Test Results (all passing)

| Test | Result |
|------|--------|
| Basic migration (node-4→node-5, node-5→node-4) | Pass — 1.7s downtime (prefaulted) |
| Double migration (409 conflict) | Pass |
| Self-migration (400) | Pass |
| Non-existent VM migration (404) | Pass |
| Stop during migration (409) | Pass |
| Destroy during migration (409) | Pass |
| Start during migration (409) | Pass (was 500, fixed) |
| Rapid ping-pong (back-to-back migrations) | Pass |
| Migration to unreachable target | Pass — fast fail, VM running |
| Agent restart during migration | Pass — source VM resumed from stale marker |
| Simultaneous cross-migrations | Pass — both ~2s downtime |
| Two VMs from same node simultaneously | Pass |
| Stopped VM relocation + start | Pass (was crash, fixed) |
| Rapid sequential migrations ×3 | Pass — no leaks |
| Drain (3 VMs simultaneously) | Pass — all 3 in ~9s |
| 2GB VM migration | Pass — 15.7s downtime, 4.7s transfer |
| /dev/shm disk fallback (adaptive) | Pass — detected low space, fell back to disk |
| Network partition during Phase 2 | Pass — 30s timeout, rollback, source resumed |
| Name collision migration (target has same VM) | Pass — 409 Conflict, no data loss |
| Full drain (4 VMs node-5→node-4) | Pass — all migrated |

## Phase 4: Split-Brain, Drain, and Large VM Hardening (2026-03-12)

### Bugs Found & Fixed

| # | Bug | Severity | Fix |
|---|-----|----------|-----|
| 42 | **Split-brain after agent crash** — if agent crashes after import-snapshot but before source cleanup, RestartAll blindly resumes paused source VM, creating two running copies on different nodes | **Critical** | Migration marker now stores target address. RestartAll checks target before resuming: if target has running copy → destroy local; if not → resume safely. Unreachable target times out in ~3s. |
| 43 | **Drain "stopped" race** — between stopVM() and os.RemoveAll() in MigrateVM, source briefly reports "stopped". Drain interprets this as "migration failed and rolled back", causing false drain failure | High | Drain now treats "stopped" as activity (keeps polling for 404). Only "running" is interpreted as migration failure. |
| 44 | **Drain HTTP status ignored** — JSON decode of non-JSON responses (404 text, 500 errors) silently failed, causing srcStatus="" which was treated as "VM gone". A 500 error would wrongly conclude migration succeeded | High | Drain now explicitly checks HTTP status: 404 = gone, non-200/non-404 = transient (keep polling) |

### Test Results

| Test | Result |
|------|--------|
| Split-brain detection (target has running copy) | Pass — local copy destroyed, target untouched |
| No split-brain (target doesn't have copy, mid-transfer crash) | Pass — source resumed locally |
| Unreachable target during crash recovery | Pass — 3s timeout, source resumed |
| Full drain (3 VMs, node-6 → node-5) | Pass — all migrated, verified, node stopped |
| Concurrent migration (2 VMs simultaneously) | Pass — both succeed, 1.7-1.9s downtime |
| 2GB VM migration (tmpfs target) | Pass — 6.1s downtime, 4.8s transfer |
| 2GB VM migration (disk target, /dev/shm full) | Pass — 7.3s downtime, fallback detected |
| Source /dev/shm exhaustion during snapshot | Pass — "using disk" fallback, 66s downtime (expected) |
| Auto-scaler re-launch after drain | Expected — high utilization triggers new node |

### Performance: /dev/shm vs Disk

| Scenario | Source Snapshot | Target Write | Snapshot Time | Transfer Time | Total Downtime |
|----------|---------------|-------------|---------------|---------------|----------------|
| Both tmpfs (ideal) | /dev/shm | /dev/shm | 1.1s | 4.8s | 6.1s |
| Disk target | /dev/shm | vmDir | 1.2s | 5.9s | 7.3s |
| Disk source | vmDir | /dev/shm | **61.6s** | 4.7s | **66.5s** |

Key insight: source /dev/shm exhaustion is catastrophic for downtime (61s snapshot) because Firecracker writes 2GB through QCOW2 virtual disk with COW page faults. Target disk fallback adds only ~20% overhead.

## Phase 5: Host Infrastructure Hardening

### Bugs Found

**Bug #45 (Critical): Auto-scaled nodes get stale binaries**
- Auto-scaler provisions new nodes from the base QCOW2 image, which has an old boxcutter-node binary
- Source node transfers snapshot files to `/dev/shm/bc-<name>/` on target
- Old target binary doesn't support `/dev/shm` import paths, causing all migrations to fail with "Failed to open snapshot file: No such file or directory"
- Fix: `deployNodeBinary()` goroutine in `addNode()` — builds latest binary from source, SCPs to new node after SSH ready, restarts service
- Had to resolve: go binary not in root's PATH (use absolute path), VCS stamping errors (-buildvcs=false), HOME not writable (set to /tmp)

**Bug #46 (Low): ORCHESTRATOR_URL_PLACEHOLDER on one auto-scaled node**
- node-7 had `ORCHESTRATOR_URL_PLACEHOLDER` instead of real URL in boxcutter.yaml
- Could not reproduce on node-8+ — cloud-init provisioning works correctly
- Likely a one-off cloud-init race or stale ISO; not a systemic bug

**Bug #47 (Critical): Host systemd unit kills QEMU VMs on daemon crash**
- Default `KillMode=control-group` sends SIGTERM to all processes in the cgroup
- When host daemon crashes, all QEMU VMs (orchestrator + nodes) are killed
- Fix: `KillMode=process` in `boxcutter-host.service` (matches boxcutter-node pattern)
- Verified: after fix, `kill -9 <host-pid>` leaves QEMU processes running

**Bug #48 (Medium): Auto-scaler yo-yo — CPU/RAM threshold mismatch**
- CPU at 100% triggers scale-up, but RAM at 19% (across 2 nodes) triggers immediate scale-down
- New empty node gets created, then immediately drained because combined RAM is low
- Fix: `scaleDownCandidate()` now checks both RAM AND CPU utilization
- Won't scale down if either metric exceeds the scale-down threshold

**Bug #49 (Medium): Drain mutex needed for concurrent drain prevention**
- API handler, auto-scaler, and upgrade reconciler can all call `drainNode()` concurrently
- Without serialization, two drains could pick the same target or double-drain a node
- Fix: `drainMu sync.Mutex` + early-exit when node already in "draining" status
- Tested: second drain attempt blocks until first completes, then finds node gone

**Bug #50 (Low): findClusterSSHKey fails in systemd context**
- When running as systemd service, SUDO_USER is not set
- Function couldn't find `~/.boxcutter/secrets/cluster-ssh.key`
- Fix: also check owner of BOXCUTTER_REPO directory via stat to find the right home

### Test Results

| Test | Result |
|------|--------|
| Single VM migration (node-to-node) | Pass — 3.3s |
| Concurrent 2-VM migration | Pass — both succeed, serialized import on target |
| 3-VM concurrent migration (mixed sizes) | Pass — all succeed |
| Full node drain (3 VMs sequential) | Pass — 10s + 80s + 70s (disk fallback for 2GB VMs) |
| Full node drain (6 VMs sequential) | Pass — all migrated, node stopped |
| Double-drain prevention (mutex) | Pass — second drain blocked, found node gone |
| Host daemon crash mid-drain (KillMode=process) | Pass — QEMU VMs survive |
| Auto-deploy to new auto-scaled node | Pass — binary built from source, deployed via SCP |
| Migration to auto-deployed node (both directions) | Pass — 3.3s |
| CPU-aware scale-down (no yo-yo) | Pass — node stays up when CPU > 30% |

## Phase 6: Crash Recovery & Drain Hardening (2026-03-12)

### Bugs Found & Fixed

| # | Bug | Severity | Fix |
|---|-----|----------|-----|
| 51 | **Auto-scale fails in systemd service** — BOXCUTTER_REPO not set when running as systemd service. `provision.sh` not found at prod path `/var/lib/boxcutter/host/provision.sh`. Auto-scaled node provisioning fails with "exit status 1" | **Critical** | `install-host` Makefile target now auto-sets `Environment=BOXCUTTER_REPO=$(CURDIR)` in systemd unit |
| 52 | **Draining node stuck after host crash** — host daemon crashes mid-drain, node stays in "draining" status with live QEMU. On restart, `drainNode()` skips nodes already in "draining" status. VMs are orphaned. | **Critical** | `bootRecover()` detects draining nodes with live QEMU, schedules drain resume via goroutine (30s warmup). `drainNode()` now accepts and resumes "draining" nodes instead of skipping. |
| 52b | **Resumed drain fails on already-migrated VMs** — when resuming a drain, VMs that completed migration before the crash still exist on source (stale). Re-migration attempt returns 409 ("already migrating" or "target already has VM"), counted as failure. Drain aborts with failedMigrations > 0. | **High** | Added pre-flight check in drain loop: before migrating, verify VM doesn't already exist on target. If running on target → skip (previously migrated). |
| 53 | **boxcutter-node.service missing KillMode=process** — auto-scaled nodes get base image without KillMode=process. When node agent crashes/restarts, default `control-group` KillMode kills ALL Firecracker VMs. Every VM gets cold-restarted instead of preserved. | **High** | `deployNodeBinary()` now also deploys `node/systemd/boxcutter-node.service` (with KillMode=process) to auto-scaled nodes. |

### Test Results

| Test | Result |
|------|--------|
| Full drain (7 VMs, node-9→node-10) | Pass — all migrated, 5 via /dev/shm + 2 disk fallback, node stopped |
| Auto-scale during drain (CPU at 200%) | Pass — correctly blocked by memory guard (rolling upgrade reserve) |
| Auto-scale after drain (CPU at 233%) | Pass — added node-11, auto-deployed binary in 17s |
| CPU-aware scale-down prevention | Pass — RAM at 27% but CPU at 116% prevents scale-down |
| Concurrent VM creation during drain | Pass — new VM created on target node, drain continues |
| Host crash mid-drain (kill -9) | Pass — QEMU VMs survive (KillMode=process), drain resumes on restart |
| Drain resume after crash (8 VMs) | Pass — skips already-migrated VM, migrates remaining 8, stops node |
| Full crash-resume-drain (11 VMs) | Pass — aqua-frog completed by agent goroutine, 10 others migrated by resumed drain |
| Agent crash mid-migration (no KillMode) | Partial — all 10 VMs cold-restarted (default KillMode=control-group kills Firecracker) |
| Agent crash mid-migration (with KillMode=process) | Pass — 9 VMs preserved, migrating VM detected stale marker, target checked, resumed locally |
| Agent crash split-brain prevention | Pass — target has no copy → resume source. Target has running copy → destroy local. |

### Key Architecture Observations

1. **KillMode=process is critical** on both host and node systemd units. Without it, a daemon crash causes cascade failure (all child processes killed).

2. **Node agent migration goroutines survive host daemon crashes** — the migration runs as a goroutine inside the node agent, independent of the host daemon. If the host daemon dies mid-drain, in-flight migrations complete on their own.

3. **Drain resume is crash-safe** — the drain resume re-queries the VM list from the node, skips VMs already on the target, and continues with remaining VMs. Idempotent and convergent.

4. **Auto-deploy must include service files** — deploying only the binary leaves systemd configuration stale. The service file (with KillMode=process) is as important as the binary itself.

## Phase 7: Remaining (TODO)
- [ ] Rolling node upgrade with live VMs via OCI images
- [ ] Orchestrator upgrade with state migration
- [ ] Test with real Tailscale-connected VMs (verify Tailscale reconnects after migration)
