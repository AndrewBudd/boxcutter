#!/bin/bash
# Redirect 169.254.169.254:80 from VMs to the vmid metadata service.
# Run this on the Node VM (the Firecracker host) after boot.
#
# VMs reach 169.254.169.254 via their default gateway (the bridge).
# This PREROUTING rule catches those packets and redirects to vmid's port.

set -euo pipefail

VMID_PORT="${VMID_PORT:-8775}"
BRIDGE_IF="${BRIDGE_IF:-brvm0}"

# Add the metadata IP to the bridge interface if not already present
if ! ip addr show dev "$BRIDGE_IF" | grep -q 169.254.169.254; then
    ip addr add 169.254.169.254/32 dev "$BRIDGE_IF"
    echo "Added 169.254.169.254 to $BRIDGE_IF"
fi

# PREROUTING: redirect metadata requests from VMs to vmid
iptables -t nat -C PREROUTING -d 169.254.169.254/32 -p tcp --dport 80 \
    -j REDIRECT --to-port "$VMID_PORT" 2>/dev/null || \
iptables -t nat -A PREROUTING -d 169.254.169.254/32 -p tcp --dport 80 \
    -j REDIRECT --to-port "$VMID_PORT"

echo "iptables: 169.254.169.254:80 → localhost:$VMID_PORT"
