#!/bin/bash
# Stop a Boxcutter VM
# Usage: bash host/stop.sh <orchestrator|node> [NAME]
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGES_DIR="${SCRIPT_DIR}/../.images"

VM_TYPE="${1:-}"
shift || true

if [ "$VM_TYPE" != "orchestrator" ] && [ "$VM_TYPE" != "node" ]; then
  echo "Usage: bash host/stop.sh <orchestrator|node> [NAME]"
  exit 1
fi

if [ "$VM_TYPE" = "orchestrator" ]; then
  VM_NAME="orchestrator"
else
  VM_NAME="${1:-boxcutter-node-1}"
fi

PID_FILE="${IMAGES_DIR}/${VM_NAME}.pid"

if [ ! -f "$PID_FILE" ]; then
  echo "${VM_NAME} is not running (no PID file)."
  exit 0
fi

pid=$(cat "$PID_FILE")
if kill -0 "$pid" 2>/dev/null; then
  echo "Stopping ${VM_NAME} (PID ${pid})..."
  kill "$pid"
  for i in $(seq 1 15); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  kill -0 "$pid" 2>/dev/null && kill -9 "$pid"
  echo "Stopped."
else
  echo "${VM_NAME} process not found (stale PID file)."
fi
rm -f "$PID_FILE"
