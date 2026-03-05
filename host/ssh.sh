#!/bin/bash
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/boxcutter.env"
exec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null ubuntu@"${NODE_IP}"
