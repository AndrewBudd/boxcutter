# Upgrade Notes — v0.15.0

## What's new

Rolling upgrades are now fully automated and tested end-to-end. The upgrade reconciler handles:
- Node replacement with VM migration (zero-downtime for VMs)
- Orchestrator replacement with DB rsync and IP reassignment
- Node agent restart for immediate re-registration after orch upgrade

## How to upgrade (on arrakis)

```bash
# 1. Wait for CI to build v0.15.0 images (check GitHub Actions)

# 2. Update the host binary
sudo boxcutter-host self-update

# 3. Run the upgrade
sudo boxcutter-host upgrade all
```

## What to expect during upgrade

1. **Image pull** (~2 min) — pulls node + orchestrator QCOW2 images from ghcr.io
2. **Node replacement** (~5 min per node) — launches new node, waits for health + golden image build, drains old node (migrating all VMs), stops old node
3. **Orchestrator replacement** (~3 min) — launches new orch at temp IP, rsyncs DB from old orch, stops old orch, reassigns IP, restarts node agents

Total time: ~10-15 min for a 1-node cluster, ~20-30 min for multi-node.

## New env vars

For dev clusters on smaller VMs:
- `MIN_FREE_MEMORY_MB` — minimum free RAM to allow node launch (default: 8192). Set to `512` for dev clusters.
- `NODE_RAM`, `NODE_VCPU`, `ORCH_RAM`, `ORCH_VCPU` — VM sizing (already existed)

## Bugs fixed

See the full list in the tag message (`git show v0.15.0`). Key fixes:
- 9 bootstrap issues from `docs/bootstrap-issues.md`
- Orchestrator upgrade: rsync auth, IP preservation, node re-registration
- NextNodeNum for prefixed cluster names
- Golden image Dockerfile: growfs service directory creation

## Test script

`host/test-upgrade.sh` runs a full end-to-end upgrade test:
- Provisions a cluster from base images
- Creates a VM
- Runs `boxcutter-host upgrade all`
- Verifies VM survived migration

Run on a dev VM (needs ~10GB RAM, 4 vCPU):
```bash
sudo MIN_FREE_MEMORY_MB=512 NODE_RAM=2G NODE_VCPU=2 ORCH_RAM=1G ORCH_VCPU=1 \
  BOXCUTTER_PREFIX=boxcutter-dev BOXCUTTER_REPO=/path/to/repo \
  bash host/test-upgrade.sh
```
