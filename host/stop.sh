#!/bin/bash
set -e
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGES_DIR="${SCRIPT_DIR}/../.images"
PID_FILE="${IMAGES_DIR}/node.pid"

if [ ! -f "$PID_FILE" ]; then
  echo "Node VM is not running (no PID file)."
  exit 0
fi

pid=$(cat "$PID_FILE")
if kill -0 "$pid" 2>/dev/null; then
  echo "Stopping Node VM (PID ${pid})..."
  kill "$pid"
  for i in $(seq 1 15); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  kill -0 "$pid" 2>/dev/null && kill -9 "$pid"
  echo "Stopped."
else
  echo "Node VM process not found (stale PID file)."
fi
rm -f "$PID_FILE"
