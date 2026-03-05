#!/bin/bash
set -e

# Boxcutter host bootstrap — run as root
# Usage: sudo ./install.sh

if [ "$EUID" -ne 0 ]; then
  echo "Error: must run as root (sudo ./install.sh)"
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOST_LAN_IP=$(ip route get 1.1.1.1 | awk '{print $7; exit}')

echo "=== Boxcutter Host Bootstrap ==="
echo "Host LAN IP: ${HOST_LAN_IP}"
echo "Script dir:  ${SCRIPT_DIR}"
echo ""

# -------------------------------------------------------------------
# Phase 1: Install SlicerVM
# -------------------------------------------------------------------
echo "=== Phase 1: Install SlicerVM ==="
if ! command -v slicer &>/dev/null; then
  curl -sLS https://get.slicervm.com | bash
  echo "SlicerVM installed."
else
  echo "SlicerVM already installed: $(slicer version)"
fi
slicer version

# -------------------------------------------------------------------
# Phase 2: dnsmasq
# -------------------------------------------------------------------
echo ""
echo "=== Phase 2: dnsmasq ==="
apt-get update -qq
apt-get install -y dnsmasq

# Deploy config with substituted IP
sed "s/HOST_LAN_IP_PLACEHOLDER/${HOST_LAN_IP}/g" \
  "${SCRIPT_DIR}/config/dnsmasq-vm.conf" > /etc/dnsmasq.d/vm.conf

systemctl enable --now dnsmasq
echo "dnsmasq configured for *.vm.lan → ${HOST_LAN_IP}"

# -------------------------------------------------------------------
# Phase 3: Caddy
# -------------------------------------------------------------------
echo ""
echo "=== Phase 3: Caddy ==="
if ! command -v caddy &>/dev/null; then
  apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y caddy
else
  echo "Caddy already installed: $(caddy version)"
fi

mkdir -p /etc/caddy/sites
cp "${SCRIPT_DIR}/config/Caddyfile" /etc/caddy/Caddyfile
systemctl enable --now caddy
echo "Caddy configured."

# -------------------------------------------------------------------
# Phase 4: SlicerVM config + systemd
# -------------------------------------------------------------------
echo ""
echo "=== Phase 4: SlicerVM config ==="
mkdir -p /etc/boxcutter
cp "${SCRIPT_DIR}/config/agent-dev.yaml" /etc/boxcutter/agent-dev.yaml

# Install systemd units
cp "${SCRIPT_DIR}/systemd/boxcutter.service" /etc/systemd/system/
cp "${SCRIPT_DIR}/systemd/boxcutter-proxy-sync.service" /etc/systemd/system/
cp "${SCRIPT_DIR}/systemd/boxcutter-gateway.service" /etc/systemd/system/
systemctl daemon-reload
echo "SlicerVM config and systemd units installed."

# -------------------------------------------------------------------
# Phase 6: Install scripts
# -------------------------------------------------------------------
echo ""
echo "=== Phase 6: Install scripts ==="
install -m 755 "${SCRIPT_DIR}/scripts/boxcutter-proxy-sync" /usr/local/bin/
install -m 755 "${SCRIPT_DIR}/scripts/boxcutter-ssh" /usr/local/bin/
install -m 755 "${SCRIPT_DIR}/scripts/boxcutter-gateway" /usr/local/bin/
install -m 755 "${SCRIPT_DIR}/scripts/bootstrap-vm.sh" /usr/local/bin/boxcutter-bootstrap-vm
echo "Scripts installed to /usr/local/bin/."

# -------------------------------------------------------------------
# Phase 7: SSH control interface
# -------------------------------------------------------------------
echo ""
echo "=== Phase 7: SSH control interface ==="
useradd -r -m -s /usr/sbin/nologin boxcutter 2>/dev/null || true
mkdir -p /home/boxcutter/.ssh
touch /home/boxcutter/.ssh/authorized_keys
chmod 700 /home/boxcutter/.ssh
chmod 600 /home/boxcutter/.ssh/authorized_keys
chown -R boxcutter:boxcutter /home/boxcutter/.ssh

# SSH ForceCommand config
cat > /etc/ssh/sshd_config.d/boxcutter.conf << 'EOF'
Match User boxcutter
    ForceCommand /usr/local/bin/boxcutter-ssh
    AllowTcpForwarding no
    X11Forwarding no
EOF
systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null || true
echo "SSH control interface configured."

# -------------------------------------------------------------------
# Phase 8: Gateway host registry (single-host default)
# -------------------------------------------------------------------
echo ""
echo "=== Phase 8: Gateway host registry ==="
if [ ! -f /etc/boxcutter/hosts ]; then
  cat > /etc/boxcutter/hosts << EOF
# name  ip               ssh_port
$(hostname -s)  ${HOST_LAN_IP}    22
EOF
  echo "Default host registry created at /etc/boxcutter/hosts"
else
  echo "Host registry already exists."
fi

# -------------------------------------------------------------------
# Phase 10: Enable on boot
# -------------------------------------------------------------------
echo ""
echo "=== Phase 10: Enable services on boot ==="
systemctl enable dnsmasq caddy boxcutter boxcutter-proxy-sync
echo "All services enabled."

# -------------------------------------------------------------------
# Summary
# -------------------------------------------------------------------
echo ""
echo "============================================"
echo "  Boxcutter host bootstrap complete!"
echo "============================================"
echo ""
echo "Host LAN IP: ${HOST_LAN_IP}"
echo ""
echo "Next steps:"
echo "  1. Activate SlicerVM:  sudo slicer activate"
echo "  2. Start the daemon:   sudo systemctl start boxcutter"
echo "  3. Build golden image: sudo slicer vm shell agent-1 --uid 0"
echo "     Then run: /usr/local/bin/boxcutter-bootstrap-vm"
echo "  4. Export golden image: sudo slicer vm shutdown agent-1"
echo "     sudo slicer disk export agent-1 -f ~/golden-agent.img --size 25G"
echo "  5. Start proxy sync:   sudo systemctl start boxcutter-proxy-sync"
echo "  6. Add SSH keys:       cat your_key >> /home/boxcutter/.ssh/authorized_keys"
echo ""
echo "Export Caddy root cert for client trust:"
echo "  sudo cp /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt ~/caddy-root.crt"
echo ""
