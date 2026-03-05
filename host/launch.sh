#!/bin/bash
# Launch the Boxcutter Node VM with QEMU/KVM
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/boxcutter.env"

IMAGES_DIR="${SCRIPT_DIR}/../.images"
REPO_DIR="${SCRIPT_DIR}/.."

NODE_DISK="${IMAGES_DIR}/node.qcow2"
CLOUD_INIT="${IMAGES_DIR}/cloud-init.iso"
PID_FILE="${IMAGES_DIR}/node.pid"

[ -f "$NODE_DISK" ] || { echo "Node VM disk not found. Run host/setup.sh first."; exit 1; }
[ -f "$CLOUD_INIT" ] || { echo "Cloud-init ISO not found. Run host/setup.sh first."; exit 1; }

# Check if already running
if [ -f "$PID_FILE" ]; then
  pid=$(cat "$PID_FILE")
  if kill -0 "$pid" 2>/dev/null; then
    echo "Node VM already running (PID ${pid})"
    echo "SSH: ssh -p ${DNAT_SSH} ubuntu@${HOST_TAP_IP}"
    exit 0
  fi
  rm -f "$PID_FILE"
fi

# Ensure TAP is up
if ! ip link show "$TAP_DEVICE" &>/dev/null; then
  echo "TAP device not found. Run host/setup.sh first."
  exit 1
fi

DAEMON=false
[ "${1:-}" = "--daemon" ] || [ "${1:-}" = "-d" ] && DAEMON=true

echo "Starting Boxcutter Node VM..."
echo "  vCPU: ${NODE_VCPU}, RAM: ${NODE_RAM}, Disk: ${NODE_DISK}"
echo "  Network: ${TAP_DEVICE} → ${NODE_IP}"
echo ""

QEMU_ARGS=(
  -enable-kvm
  -cpu host
  -smp "${NODE_VCPU}"
  -m "${NODE_RAM}"
  -drive "file=${NODE_DISK},format=qcow2,if=virtio"
  -drive "file=${CLOUD_INIT},format=raw,if=virtio"
  -netdev "tap,id=net0,ifname=${TAP_DEVICE},script=no,downscript=no"
  -device "virtio-net-pci,netdev=net0,mac=${NODE_MAC}"
  -fsdev "local,id=boxcutter_dev,path=${REPO_DIR},security_model=mapped-xattr,readonly=on"
  -device "virtio-9p-pci,fsdev=boxcutter_dev,mount_tag=boxcutter"
  -serial mon:stdio
  -nographic
)

if [ "$DAEMON" = true ]; then
  # Background mode — log serial to file
  QEMU_ARGS=("${QEMU_ARGS[@]/%mon:stdio/file:${IMAGES_DIR}/node-console.log}")
  QEMU_ARGS+=(-daemonize -pidfile "$PID_FILE")
  qemu-system-x86_64 "${QEMU_ARGS[@]}"
  echo "Node VM started in background (PID $(cat "$PID_FILE"))"
  echo "Console log: ${IMAGES_DIR}/node-console.log"
  echo "SSH: ssh -p ${DNAT_SSH} ubuntu@${HOST_TAP_IP}"
else
  echo "Launching in foreground (Ctrl-A X to quit)..."
  echo ""
  exec qemu-system-x86_64 "${QEMU_ARGS[@]}"
fi
