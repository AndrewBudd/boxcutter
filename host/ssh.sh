#!/bin/bash
# SSH into a Boxcutter VM
# Usage: bash host/ssh.sh <orchestrator|node> [NODE_NUMBER]
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/boxcutter.env"

VM_TYPE="${1:-}"
if [ "$VM_TYPE" = "orchestrator" ]; then
  exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@"${ORCH_IP}"
elif [ "$VM_TYPE" = "node" ]; then
  NODE_NUM="${2:-1}"
  NODE_OCTET=$((NODE_IP_OFFSET + NODE_NUM))
  IP="${NODE_SUBNET}.${NODE_OCTET}"
  exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@"${IP}"
else
  echo "Usage: bash host/ssh.sh <orchestrator|node> [NODE_NUMBER]"
  exit 1
fi
