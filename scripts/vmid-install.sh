#!/bin/bash
# Install and configure vmid on the Node VM.
# Run as root. Expects Go to be installed.
set -euo pipefail

BOXCUTTER_HOME="/var/lib/boxcutter"
VMID_SRC="${BOXCUTTER_HOME}/vmid"

# Check for 9p mount fallback
if [ ! -d "$VMID_SRC" ] && [ -d /mnt/boxcutter/vmid ]; then
  VMID_SRC="/mnt/boxcutter/vmid"
fi

echo "=== Building vmid ==="
cd "$VMID_SRC"
go build -o /usr/local/bin/vmid ./cmd/vmid/

echo "=== Configuring vmid ==="
mkdir -p /etc/vmid /run/vmid

# Create config if it doesn't exist
if [ ! -f /etc/vmid/config.yaml ]; then
  cat > /etc/vmid/config.yaml <<'EOF'
listen:
  vm_port: 8775
  admin_socket: /run/vmid/admin.sock

jwt:
  ttl: "10m"

log:
  level: info
  format: json
EOF
  echo "  Created /etc/vmid/config.yaml (edit to add GitHub App config)"
fi

echo "=== Installing systemd service ==="
cp "${BOXCUTTER_HOME}/systemd/vmid.service" /etc/systemd/system/ 2>/dev/null || \
  cp /mnt/boxcutter/systemd/vmid.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable vmid

echo "=== Setting up iptables redirect ==="
BRIDGE_IF="brvm0"
VMID_PORT=8775

# Add metadata IP to bridge
if ! ip addr show dev "$BRIDGE_IF" 2>/dev/null | grep -q 169.254.169.254; then
  ip addr add 169.254.169.254/32 dev "$BRIDGE_IF" 2>/dev/null || true
fi

# iptables redirect
iptables -t nat -C PREROUTING -d 169.254.169.254/32 -p tcp --dport 80 \
  -j REDIRECT --to-port "$VMID_PORT" 2>/dev/null || \
iptables -t nat -A PREROUTING -d 169.254.169.254/32 -p tcp --dport 80 \
  -j REDIRECT --to-port "$VMID_PORT"

echo "=== Starting vmid ==="
systemctl start vmid

echo ""
echo "vmid installed and running."
echo "  Config: /etc/vmid/config.yaml"
echo "  Socket: /run/vmid/admin.sock"
echo "  Health: curl --unix-socket /run/vmid/admin.sock http://localhost/healthz"
