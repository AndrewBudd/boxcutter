#!/bin/bash
# Install QEMU and dependencies on the physical host.
# Download Ubuntu cloud image. Create TAP device with NAT.
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

# --- Create Node VM disk (COW on cloud image) ---
NODE_DISK_FILE="${IMAGES_DIR}/node.qcow2"
if [ ! -f "$NODE_DISK_FILE" ]; then
  echo "Creating Node VM disk (${NODE_DISK})..."
  qemu-img create -f qcow2 -b "$UBUNTU_IMG" -F qcow2 "$NODE_DISK_FILE" "$NODE_DISK"
else
  echo "Node VM disk already exists."
fi

# --- Generate cloud-init ISO ---
echo "Generating cloud-init ISO..."
CIDATA_DIR=$(mktemp -d)
trap "rm -rf ${CIDATA_DIR}" EXIT

# Substitute SSH key into user-data
sed "s|SSH_PUBKEY_PLACEHOLDER|${SSH_PUBKEY}|" \
  "${SCRIPT_DIR}/../cloud-init/user-data" > "${CIDATA_DIR}/user-data"
cp "${SCRIPT_DIR}/../cloud-init/meta-data" "${CIDATA_DIR}/meta-data"

# Substitute network config
sed -e "s|NODE_IP_PLACEHOLDER|${NODE_IP}|" \
    -e "s|NODE_CIDR_PLACEHOLDER|${HOST_TAP_CIDR}|" \
    -e "s|HOST_TAP_IP_PLACEHOLDER|${HOST_TAP_IP}|" \
    -e "s|NODE_MAC_PLACEHOLDER|${NODE_MAC}|" \
    "${SCRIPT_DIR}/../cloud-init/network-config" > "${CIDATA_DIR}/network-config"

genisoimage -output "${IMAGES_DIR}/cloud-init.iso" \
  -volid cidata -joliet -rock \
  "${CIDATA_DIR}/user-data" "${CIDATA_DIR}/meta-data" "${CIDATA_DIR}/network-config" \
  2>/dev/null

# --- Set up TAP device with NAT ---
echo "Setting up TAP device (${TAP_DEVICE}) with NAT..."
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null

if ! ip link show "$TAP_DEVICE" &>/dev/null; then
  sudo ip tuntap add dev "$TAP_DEVICE" mode tap user "$(whoami)"
  sudo ip link set "$TAP_DEVICE" up
  sudo ip addr add "${HOST_TAP_IP}/${HOST_TAP_CIDR}" dev "$TAP_DEVICE" 2>/dev/null || true
  echo "TAP device ${TAP_DEVICE} created (${HOST_TAP_IP}/${HOST_TAP_CIDR})"
else
  echo "TAP device already exists."
  # Ensure IP is assigned
  sudo ip addr add "${HOST_TAP_IP}/${HOST_TAP_CIDR}" dev "$TAP_DEVICE" 2>/dev/null || true
fi

# --- NAT: masquerade Node VM traffic to internet ---
echo "Setting up NAT masquerade..."
sudo iptables -t nat -C POSTROUTING -s "${HOST_TAP_IP}/${HOST_TAP_CIDR}" -o "$HOST_INTERFACE" -j MASQUERADE 2>/dev/null || \
sudo iptables -t nat -A POSTROUTING -s "${HOST_TAP_IP}/${HOST_TAP_CIDR}" -o "$HOST_INTERFACE" -j MASQUERADE

# Also NAT the VM internal network (10.0.1.0/24) which gets routed through the Node VM
sudo iptables -t nat -C POSTROUTING -s "${VM_IP_PREFIX}.0/${VM_CIDR}" -o "$HOST_INTERFACE" -j MASQUERADE 2>/dev/null || \
sudo iptables -t nat -A POSTROUTING -s "${VM_IP_PREFIX}.0/${VM_CIDR}" -o "$HOST_INTERFACE" -j MASQUERADE

# Forward traffic from TAP
sudo iptables -C FORWARD -i "$TAP_DEVICE" -j ACCEPT 2>/dev/null || \
sudo iptables -I FORWARD -i "$TAP_DEVICE" -j ACCEPT
sudo iptables -C FORWARD -o "$TAP_DEVICE" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
sudo iptables -I FORWARD -o "$TAP_DEVICE" -m state --state RELATED,ESTABLISHED -j ACCEPT

# Route the VM internal network through the Node VM
sudo ip route replace "${VM_IP_PREFIX}.0/${VM_CIDR}" via "$NODE_IP" dev "$TAP_DEVICE" 2>/dev/null || true

echo ""
echo "=== Host setup complete ==="
echo "Node VM disk: ${IMAGES_DIR}/node.qcow2"
echo "Cloud-init:   ${IMAGES_DIR}/cloud-init.iso"
echo "TAP device:   ${TAP_DEVICE} (${HOST_TAP_IP}/${HOST_TAP_CIDR})"
echo "Node VM IP:   ${NODE_IP} (internal — access via Tailscale)"
echo ""
echo "Next: ./host/launch.sh"
