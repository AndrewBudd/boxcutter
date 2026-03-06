#!/bin/bash
# SSH into a Boxcutter VM
# Usage: bash host/ssh.sh <orchestrator|node> [NAME]
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/boxcutter.env"

VM_TYPE="${1:-}"
if [ "$VM_TYPE" = "orchestrator" ]; then
  exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@"${ORCH_IP}"
elif [ "$VM_TYPE" = "node" ]; then
  exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@"${NODE_IP}"
else
  echo "Usage: bash host/ssh.sh <orchestrator|node>"
  exit 1
fi
