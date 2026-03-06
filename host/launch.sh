#!/bin/bash
# Launch a Boxcutter VM with QEMU/KVM
# Usage: bash host/launch.sh <orchestrator|node> [NAME] [--daemon]
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/boxcutter.env"

IMAGES_DIR="${SCRIPT_DIR}/../.images"

VM_TYPE="${1:-}"
shift || true

if [ "$VM_TYPE" != "orchestrator" ] && [ "$VM_TYPE" != "node" ]; then
  echo "Usage: bash host/launch.sh <orchestrator|node> [NAME] [--daemon]"
  exit 1
fi

# Parse args
DAEMON=false
VM_NAME=""
for arg in "$@"; do
  case "$arg" in
    --daemon|-d) DAEMON=true ;;
    *) [ -z "$VM_NAME" ] && VM_NAME="$arg" ;;
  esac
done

# Set VM-specific parameters
if [ "$VM_TYPE" = "orchestrator" ]; then
  VM_NAME="orchestrator"
  VCPU="$ORCH_VCPU"
  RAM="$ORCH_RAM"
  DISK="${IMAGES_DIR}/orchestrator.qcow2"
  ISO="${IMAGES_DIR}/orchestrator-cloud-init.iso"
  TAP="$ORCH_TAP"
  MAC="$ORCH_MAC"
  IP="$ORCH_IP"
else
  VM_NAME="${VM_NAME:-boxcutter-node-1}"
  VCPU="$NODE_VCPU"
  RAM="$NODE_RAM"
  DISK="${IMAGES_DIR}/${VM_NAME}.qcow2"
  ISO="${IMAGES_DIR}/${VM_NAME}-cloud-init.iso"
  TAP="$NODE_TAP"
  MAC="$NODE_MAC"
  IP="$NODE_IP"
fi

PID_FILE="${IMAGES_DIR}/${VM_NAME}.pid"

[ -f "$DISK" ] || { echo "VM disk not found: $DISK. Run: make provision-${VM_TYPE}"; exit 1; }
[ -f "$ISO" ] || { echo "Cloud-init ISO not found: $ISO. Run: make provision-${VM_TYPE}"; exit 1; }

# Check if already running
if [ -f "$PID_FILE" ]; then
  pid=$(cat "$PID_FILE")
  if kill -0 "$pid" 2>/dev/null; then
    echo "${VM_NAME} already running (PID ${pid})"
    exit 0
  fi
  rm -f "$PID_FILE"
fi

# Ensure TAP device is up and attached to bridge
if ! ip link show "$TAP" &>/dev/null; then
  echo "Creating TAP device ${TAP}..."
  sudo ip tuntap add dev "$TAP" mode tap user "$(whoami)"
  sudo ip link set "$TAP" master "$BRIDGE_DEVICE"
  sudo ip link set "$TAP" up
else
  sudo ip link set "$TAP" up
fi

echo "Starting ${VM_NAME} (${VM_TYPE})..."
echo "  vCPU: ${VCPU}, RAM: ${RAM}"
echo "  Network: ${TAP} (${HOST_BRIDGE_IP} → ${IP})"
echo ""

QEMU_ARGS=(
  -enable-kvm
  -cpu host
  -smp "${VCPU}"
  -m "${RAM}"
  -drive "file=${DISK},format=qcow2,if=virtio"
  -drive "file=${ISO},format=raw,if=virtio"
  -netdev "tap,id=net0,ifname=${TAP},script=no,downscript=no"
  -device "virtio-net-pci,netdev=net0,mac=${MAC}"
  -serial mon:stdio
  -nographic
)

if [ "$DAEMON" = true ]; then
  QEMU_DAEMON_ARGS=()
  for arg in "${QEMU_ARGS[@]}"; do
    case "$arg" in
      mon:stdio) continue ;;
      -nographic) continue ;;
      -serial) continue ;;
      *) QEMU_DAEMON_ARGS+=("$arg") ;;
    esac
  done
  QEMU_DAEMON_ARGS+=(-display none -serial "file:${IMAGES_DIR}/${VM_NAME}-console.log" -daemonize -pidfile "$PID_FILE")
  qemu-system-x86_64 "${QEMU_DAEMON_ARGS[@]}"
  echo "${VM_NAME} started in background (PID $(cat "$PID_FILE"))"
  echo "Console log: ${IMAGES_DIR}/${VM_NAME}-console.log"
  echo "SSH: ssh ubuntu@${IP}"
else
  echo "Launching in foreground (Ctrl-A X to quit)..."
  echo ""
  exec qemu-system-x86_64 "${QEMU_ARGS[@]}"
fi
