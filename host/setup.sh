#!/bin/bash
# Install QEMU and dependencies on the physical host.
# Download Ubuntu cloud image. Create bridge with NAT.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/boxcutter.env"

IMAGES_DIR="${SCRIPT_DIR}/../.images"
mkdir -p "$IMAGES_DIR"

echo "=== Boxcutter host setup ==="

# --- Find local SSH key ---
SSH_PUBKEY=""
for keyfile in ~/.ssh/id_ed25519.pub ~/.ssh/id_rsa.pub; do
  if [ -f "$keyfile" ]; then
    SSH_PUBKEY=$(cat "$keyfile")
    break
  fi
done
if [ -z "$SSH_PUBKEY" ]; then
  echo "No SSH public key found. Generating one..."
  ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519 -N "" -q
  SSH_PUBKEY=$(cat ~/.ssh/id_ed25519.pub)
fi

# --- Install QEMU ---
if ! command -v qemu-system-x86_64 &>/dev/null; then
  echo "Installing QEMU..."
  sudo apt-get update -qq
  sudo apt-get install -y qemu-system-x86 qemu-utils genisoimage
else
  echo "QEMU already installed."
fi

# --- Download Ubuntu cloud image ---
UBUNTU_IMG="${IMAGES_DIR}/ubuntu-noble-cloudimg-amd64.img"
if [ ! -f "$UBUNTU_IMG" ]; then
  echo "Downloading Ubuntu 24.04 cloud image..."
  wget -q --show-progress -O "$UBUNTU_IMG" \
    https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
else
  echo "Ubuntu cloud image already present."
fi

# --- Set up bridge device ---
echo "Setting up bridge ${BRIDGE_DEVICE}..."
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null

if ! ip link show "$BRIDGE_DEVICE" &>/dev/null; then
  sudo ip link add name "$BRIDGE_DEVICE" type bridge
  sudo ip addr add "${HOST_BRIDGE_IP}/${HOST_BRIDGE_CIDR}" dev "$BRIDGE_DEVICE"
  sudo ip link set "$BRIDGE_DEVICE" up
  echo "Bridge ${BRIDGE_DEVICE} created (${HOST_BRIDGE_IP}/${HOST_BRIDGE_CIDR})"
else
  echo "Bridge ${BRIDGE_DEVICE} already exists."
  sudo ip addr add "${HOST_BRIDGE_IP}/${HOST_BRIDGE_CIDR}" dev "$BRIDGE_DEVICE" 2>/dev/null || true
fi

# --- Create orchestrator TAP device and attach to bridge ---
# Node TAPs are created on demand by launch.sh
if ! ip link show "$ORCH_TAP" &>/dev/null; then
  sudo ip tuntap add dev "$ORCH_TAP" mode tap user "$(whoami)"
  sudo ip link set "$ORCH_TAP" master "$BRIDGE_DEVICE"
  sudo ip link set "$ORCH_TAP" up
  echo "TAP ${ORCH_TAP} created and attached to ${BRIDGE_DEVICE}"
else
  echo "TAP ${ORCH_TAP} already exists."
  sudo ip link set "$ORCH_TAP" master "$BRIDGE_DEVICE" 2>/dev/null || true
fi

# --- NAT: masquerade VM traffic to internet ---
echo "Setting up NAT masquerade..."
SUBNET="${HOST_BRIDGE_IP}/${HOST_BRIDGE_CIDR}"
sudo iptables -t nat -C POSTROUTING -s "$SUBNET" -o "$HOST_INTERFACE" -j MASQUERADE 2>/dev/null || \
sudo iptables -t nat -A POSTROUTING -s "$SUBNET" -o "$HOST_INTERFACE" -j MASQUERADE

# Forward traffic from bridge
sudo iptables -C FORWARD -i "$BRIDGE_DEVICE" -j ACCEPT 2>/dev/null || \
sudo iptables -I FORWARD -i "$BRIDGE_DEVICE" -j ACCEPT
sudo iptables -C FORWARD -o "$BRIDGE_DEVICE" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
sudo iptables -I FORWARD -o "$BRIDGE_DEVICE" -m state --state RELATED,ESTABLISHED -j ACCEPT

echo ""
echo "=== Host setup complete ==="
echo "Bridge:        ${BRIDGE_DEVICE} (${HOST_BRIDGE_IP}/${HOST_BRIDGE_CIDR})"
echo "TAP devices:   ${ORCH_TAP} (node TAPs created on demand)"
echo "Orchestrator:  ${ORCH_IP}"
echo "Nodes:         ${NODE_SUBNET}.{3,4,...} (derived from node number)"
echo ""
echo "Next: make provision-orchestrator && make provision-node"
