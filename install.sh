#!/bin/bash
# Boxcutter Node VM setup — runs inside the Node VM
# Installs Firecracker, networking, Caddy, dnsmasq, all management scripts.
set -e

if [ "$EUID" -ne 0 ]; then
  echo "Error: must run as root"
  exit 1
fi

# Determine source directory (9p mount or local)
if [ -d /mnt/boxcutter/scripts ]; then
  SRC="/mnt/boxcutter"
elif [ -d "$(dirname "$0")/scripts" ]; then
  SRC="$(cd "$(dirname "$0")" && pwd)"
else
  echo "Error: cannot find boxcutter source files"
  exit 1
fi

BOXCUTTER_HOME="/var/lib/boxcutter"
ARCH=$(uname -m)

echo "=== Boxcutter Node VM Setup ==="
echo "Source: ${SRC}"
echo ""

# -------------------------------------------------------------------
# 1. Install Firecracker
# -------------------------------------------------------------------
echo "--- Installing Firecracker ---"
if ! command -v firecracker &>/dev/null; then
  FC_VERSION="v1.12.0"
  FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz"
  echo "Downloading Firecracker ${FC_VERSION}..."
  curl -sL "$FC_URL" | tar xz -C /tmp
  mv "/tmp/release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
  chmod +x /usr/local/bin/firecracker
  rm -rf "/tmp/release-${FC_VERSION}-${ARCH}"
  echo "Firecracker installed: $(firecracker --version 2>&1 | head -1)"
else
  echo "Firecracker already installed: $(firecracker --version 2>&1 | head -1)"
fi

# -------------------------------------------------------------------
# 2. Download Firecracker kernel
# -------------------------------------------------------------------
echo ""
echo "--- Downloading Firecracker kernel ---"
mkdir -p "${BOXCUTTER_HOME}/kernel"
KERNEL="${BOXCUTTER_HOME}/kernel/vmlinux"
if [ ! -f "$KERNEL" ]; then
  KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/${ARCH}/vmlinux-6.1.102"
  echo "Downloading kernel..."
  curl -sL "$KERNEL_URL" -o "$KERNEL"
  echo "Kernel downloaded."
else
  echo "Kernel already present."
fi

# -------------------------------------------------------------------
# 3. Install Caddy
# -------------------------------------------------------------------
echo ""
echo "--- Installing Caddy ---"
if ! command -v caddy &>/dev/null; then
  apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y caddy
fi
mkdir -p /etc/caddy/sites
cp "${SRC}/config/Caddyfile" /etc/caddy/Caddyfile
systemctl enable caddy

# -------------------------------------------------------------------
# 4. Configure dnsmasq
# -------------------------------------------------------------------
echo ""
echo "--- Configuring dnsmasq ---"
# Get the host LAN IP from the env file (passed via cloud-init or manual)
# Default: resolve *.vm.lan to the Node VM's bridge IP for internal access
cp "${SRC}/config/dnsmasq-bridge.conf" /etc/dnsmasq.d/boxcutter-bridge.conf
mkdir -p /etc/boxcutter/dhcp-hosts
systemctl enable dnsmasq

# -------------------------------------------------------------------
# 5. Set up networking
# -------------------------------------------------------------------
echo ""
echo "--- Setting up bridge network ---"
install -m 755 "${SRC}/scripts/boxcutter-net" /usr/local/bin/
cp "${SRC}/systemd/boxcutter-net.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable boxcutter-net

# Run it now
/usr/local/bin/boxcutter-net up

# -------------------------------------------------------------------
# 6. Install management scripts
# -------------------------------------------------------------------
echo ""
echo "--- Installing boxcutter scripts ---"
install -m 755 "${SRC}/scripts/boxcutter-ctl" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-proxy-sync" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-ssh" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-gateway" /usr/local/bin/

# -------------------------------------------------------------------
# 7. Systemd services
# -------------------------------------------------------------------
echo ""
echo "--- Installing systemd services ---"
cp "${SRC}/systemd/boxcutter-proxy-sync.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable boxcutter-proxy-sync

# -------------------------------------------------------------------
# 8. SSH control interface
# -------------------------------------------------------------------
echo ""
echo "--- Setting up SSH control interface ---"
useradd -r -m -s /usr/sbin/nologin boxcutter 2>/dev/null || true
mkdir -p /home/boxcutter/.ssh
touch /home/boxcutter/.ssh/authorized_keys
chmod 700 /home/boxcutter/.ssh
chmod 600 /home/boxcutter/.ssh/authorized_keys
chown -R boxcutter:boxcutter /home/boxcutter/.ssh

mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/boxcutter.conf << 'EOF'
Match User boxcutter
    ForceCommand /usr/local/bin/boxcutter-ssh
    AllowTcpForwarding no
    X11Forwarding no
EOF
systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true

# -------------------------------------------------------------------
# 9. Generate SSH keypair for VM access
# -------------------------------------------------------------------
echo ""
echo "--- Generating VM access SSH key ---"
mkdir -p "${BOXCUTTER_HOME}/ssh"
if [ ! -f "${BOXCUTTER_HOME}/ssh/id_ed25519" ]; then
  ssh-keygen -t ed25519 -f "${BOXCUTTER_HOME}/ssh/id_ed25519" -N "" -q
  echo "SSH keypair generated for VM access."
fi

# -------------------------------------------------------------------
# 10. Create state directories
# -------------------------------------------------------------------
mkdir -p "${BOXCUTTER_HOME}/vms"
mkdir -p "${BOXCUTTER_HOME}/golden"

# Copy golden image build scripts
cp "${SRC}/golden/build.sh" "${BOXCUTTER_HOME}/golden/build.sh"
cp "${SRC}/golden/provision.sh" "${BOXCUTTER_HOME}/golden/provision.sh"
chmod +x "${BOXCUTTER_HOME}/golden/build.sh" "${BOXCUTTER_HOME}/golden/provision.sh"

# -------------------------------------------------------------------
# Done
# -------------------------------------------------------------------
echo ""
echo "============================================"
echo "  Boxcutter Node VM setup complete!"
echo "============================================"
echo ""
echo "Next steps:"
echo "  1. Build golden image:  sudo boxcutter-ctl golden build"
echo "  2. Create a VM:         sudo boxcutter-ctl create agent-1"
echo "  3. Start the VM:        sudo boxcutter-ctl start agent-1"
echo "  4. Shell into it:       sudo boxcutter-ctl shell agent-1"
echo ""
echo "Start all services:"
echo "  sudo systemctl start dnsmasq caddy boxcutter-net boxcutter-proxy-sync"
echo ""
