#!/bin/bash
# Boxcutter Node VM setup — runs inside the Node VM
# Installs Firecracker, Tailscale, networking, Caddy, and all management scripts.
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
  KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/${ARCH}/vmlinux-6.1.128"
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
# 4. Install Tailscale on the Node VM
# -------------------------------------------------------------------
echo ""
echo "--- Installing Tailscale ---"
if ! command -v tailscale &>/dev/null; then
  curl -fsSL https://tailscale.com/install.sh | sh
  echo "Tailscale installed."
else
  echo "Tailscale already installed."
fi
systemctl enable tailscaled
systemctl start tailscaled

# Tailscale join is handled by boxcutter-setup using the bundle auth key.
mkdir -p /etc/boxcutter

# Install socat (needed for vm_ssh TAP binding)
apt-get install -y socat >/dev/null 2>&1 || true

# -------------------------------------------------------------------
# 5. Network config (per-TAP point-to-point, fwmark routing)
# -------------------------------------------------------------------
echo ""
echo "--- Configuring internal network ---"
mkdir -p /etc/boxcutter
echo "  VMs use per-TAP 10.0.0.1/30 ↔ 10.0.0.2/30 with fwmark policy routing"
echo "  External access via Tailscale"

# -------------------------------------------------------------------
# 6. Set up networking
# -------------------------------------------------------------------
echo ""
echo "--- Setting up VM network ---"
install -m 755 "${SRC}/scripts/boxcutter-net" /usr/local/bin/
cp "${SRC}/systemd/boxcutter-net.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable boxcutter-net

# Run it now
/usr/local/bin/boxcutter-net up

# -------------------------------------------------------------------
# 7. Install management scripts
# -------------------------------------------------------------------
echo ""
echo "--- Installing boxcutter scripts ---"
install -m 755 "${SRC}/scripts/boxcutter-ctl" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-proxy-sync" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-ssh" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-gateway" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-names" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-tls" /usr/local/bin/
install -m 755 "${SRC}/scripts/boxcutter-setup" /usr/local/bin/

# Ensure socat is installed (needed for vm_ssh TAP binding)
apt-get install -y socat >/dev/null 2>&1 || true

# -------------------------------------------------------------------
# 7a. Copy bootstrap bundle and run setup
# -------------------------------------------------------------------
echo ""
echo "--- Bootstrap bundle ---"
BUNDLE_SRC="/mnt/boxcutter/.boxcutter"
if [ -d "$BUNDLE_SRC" ]; then
  mkdir -p /etc/boxcutter/secrets
  # Copy config
  cp "$BUNDLE_SRC/boxcutter.yaml" /etc/boxcutter/boxcutter.yaml
  # Copy secrets (don't overwrite auto-generated ones)
  for f in "$BUNDLE_SRC"/secrets/*; do
    [ -f "$f" ] || continue
    dest="/etc/boxcutter/secrets/$(basename "$f")"
    if [ ! -f "$dest" ]; then
      cp "$f" "$dest"
    fi
  done
  chmod 600 /etc/boxcutter/secrets/*
  echo "Bundle copied from ${BUNDLE_SRC}"
else
  echo "Warning: no bundle at ${BUNDLE_SRC} — manual setup required"
fi

echo ""
echo "--- Running boxcutter-setup ---"
/usr/local/bin/boxcutter-setup

# -------------------------------------------------------------------
# 7b. Build Go services
# -------------------------------------------------------------------
echo ""
echo "--- Building Go services ---"

# Forward proxy
PROXY_SRC="${BOXCUTTER_HOME}/proxy"
[ ! -d "$PROXY_SRC" ] && [ -d /mnt/boxcutter/proxy ] && PROXY_SRC="/mnt/boxcutter/proxy"
if [ -d "$PROXY_SRC" ]; then
  cd "$PROXY_SRC"
  go build -o /usr/local/bin/boxcutter-proxy ./cmd/proxy/
  echo "  Forward proxy built."
fi

# Node agent
NODE_SRC="${BOXCUTTER_HOME}/node"
[ ! -d "$NODE_SRC" ] && [ -d /mnt/boxcutter/node ] && NODE_SRC="/mnt/boxcutter/node"
if [ -d "$NODE_SRC" ]; then
  cd "$NODE_SRC"
  go build -o /usr/local/bin/boxcutter-node ./cmd/node/
  echo "  Node agent built."
fi

# Orchestrator
ORCH_SRC="${BOXCUTTER_HOME}/orchestrator"
[ ! -d "$ORCH_SRC" ] && [ -d /mnt/boxcutter/orchestrator ] && ORCH_SRC="/mnt/boxcutter/orchestrator"
if [ -d "$ORCH_SRC" ]; then
  cd "$ORCH_SRC"
  go build -o /usr/local/bin/boxcutter-orchestrator ./cmd/orchestrator/
  go build -o /usr/local/bin/boxcutter-ssh-orchestrator ./cmd/ssh/
  echo "  Orchestrator built."
fi

# Create default allowlist if it doesn't exist
if [ ! -f /etc/boxcutter/proxy-allowlist.conf ]; then
  cat > /etc/boxcutter/proxy-allowlist.conf <<'ALEOF'
# Egress allowlist for paranoid mode VMs
# Lines starting with # are comments
# Supports exact match and wildcard (*.example.com)
*.github.com
github.com
*.githubusercontent.com
api.github.com
*.npmjs.org
registry.npmjs.org
*.rubygems.org
ALEOF
fi

# -------------------------------------------------------------------
# 7c. Install DERP relay
# -------------------------------------------------------------------
echo ""
echo "--- Installing DERP relay ---"
if ! command -v derper &>/dev/null; then
  go install tailscale.com/cmd/derper@latest 2>/dev/null || true
  # Move from GOPATH to /usr/local/bin
  GOPATH_BIN=$(go env GOPATH)/bin
  [ -f "${GOPATH_BIN}/derper" ] && mv "${GOPATH_BIN}/derper" /usr/local/bin/derper
fi
if command -v derper &>/dev/null; then
  # Set up cert symlinks for derper (expects <hostname>.crt/.key)
  mkdir -p /etc/boxcutter/derp-certs
  ln -sf /etc/boxcutter/secrets/leaf.crt /etc/boxcutter/derp-certs/10.0.0.1.crt
  ln -sf /etc/boxcutter/secrets/leaf.key /etc/boxcutter/derp-certs/10.0.0.1.key
  echo "DERP relay ready."
else
  echo "Warning: derper not installed (Go may not be available yet)"
fi

# -------------------------------------------------------------------
# 8. Systemd services
# -------------------------------------------------------------------
echo ""
echo "--- Installing systemd services ---"
cp "${SRC}/systemd/boxcutter-proxy-sync.service" /etc/systemd/system/
cp "${SRC}/systemd/boxcutter-proxy.service" /etc/systemd/system/
cp "${SRC}/systemd/boxcutter-derper.service" /etc/systemd/system/
cp "${SRC}/systemd/boxcutter-node.service" /etc/systemd/system/
cp "${SRC}/systemd/boxcutter-orchestrator.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable boxcutter-proxy-sync
systemctl enable boxcutter-proxy 2>/dev/null || true
systemctl enable boxcutter-derper 2>/dev/null || true
systemctl enable boxcutter-node 2>/dev/null || true
systemctl enable boxcutter-orchestrator 2>/dev/null || true

# -------------------------------------------------------------------
# 9. SSH control interface
# -------------------------------------------------------------------
echo ""
echo "--- Setting up SSH control interface ---"
useradd -r -m -s /bin/bash boxcutter 2>/dev/null || true
echo "boxcutter ALL=(ALL) NOPASSWD: /usr/local/bin/boxcutter-ctl, /usr/bin/tee -a /etc/boxcutter/secrets/authorized-keys, /usr/bin/sort -u /etc/boxcutter/secrets/authorized-keys -o /etc/boxcutter/secrets/authorized-keys" > /etc/sudoers.d/boxcutter
chmod 440 /etc/sudoers.d/boxcutter
mkdir -p /home/boxcutter/.ssh
touch /home/boxcutter/.ssh/authorized_keys
chmod 700 /home/boxcutter/.ssh
chmod 600 /home/boxcutter/.ssh/authorized_keys
chown -R boxcutter:boxcutter /home/boxcutter/.ssh

# Sync authorized keys from bundle (boxcutter-setup already populated these)
if [ -f /etc/boxcutter/secrets/authorized-keys ]; then
  cat /etc/boxcutter/secrets/authorized-keys >> /home/boxcutter/.ssh/authorized_keys
  sort -u /home/boxcutter/.ssh/authorized_keys -o /home/boxcutter/.ssh/authorized_keys
  echo "Authorized keys synced from bundle."
fi

# --- Accept any SSH username (maps to boxcutter) ---
echo "Building NSS catchall module for Node VM..."
BOXCUTTER_UID=$(id -u boxcutter)
BOXCUTTER_GID=$(id -g boxcutter)
cp "${SRC}/golden/nss_catchall.c" /tmp/nss_catchall_node.c
# Patch uid/gid/home for boxcutter user
sed -i "s/result->pw_uid = 1000/result->pw_uid = ${BOXCUTTER_UID}/" /tmp/nss_catchall_node.c
sed -i "s/result->pw_gid = 1000/result->pw_gid = ${BOXCUTTER_GID}/" /tmp/nss_catchall_node.c
sed -i 's|/home/dev|/home/boxcutter|g' /tmp/nss_catchall_node.c
# Add Node VM system users to the skip list
sed -i 's/"avahi",/"avahi", "ubuntu", "caddy",/' /tmp/nss_catchall_node.c

LIBDIR=$(gcc -print-multi-os-directory 2>/dev/null && echo /usr/lib/x86_64-linux-gnu || echo /usr/lib/x86_64-linux-gnu)
apt-get install -y gcc libc6-dev > /dev/null 2>&1
gcc -shared -fPIC -o /usr/lib/x86_64-linux-gnu/libnss_catchall.so.2 /tmp/nss_catchall_node.c
rm /tmp/nss_catchall_node.c
apt-get remove -y gcc > /dev/null 2>&1 || true

sed -i 's/^passwd:.*/passwd:         files catchall/' /etc/nsswitch.conf
sed -i 's/^shadow:.*/shadow:         files catchall/' /etc/nsswitch.conf

# AuthorizedKeysCommand — serve boxcutter's keys for any user
cat > /usr/local/bin/auth-keys-any << 'SCRIPT'
#!/bin/bash
cat /home/boxcutter/.ssh/authorized_keys
SCRIPT
chmod 755 /usr/local/bin/auth-keys-any

mkdir -p /etc/ssh/sshd_config.d
cat > /etc/ssh/sshd_config.d/boxcutter.conf << 'EOF'
AuthorizedKeysCommand /usr/local/bin/auth-keys-any %u
AuthorizedKeysCommandUser root

Match User !ubuntu,!root,*
    ForceCommand /usr/local/bin/boxcutter-ssh
    AllowTcpForwarding no
    X11Forwarding no
EOF
systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true

# -------------------------------------------------------------------
# 10. VM access SSH key (handled by boxcutter-setup)
# -------------------------------------------------------------------
echo ""
echo "--- VM access SSH key ---"
echo "SSH keypair managed at /etc/boxcutter/secrets/node-ssh.key (via boxcutter-setup)"

# -------------------------------------------------------------------
# 11. Ensure loop devices work (needed for golden image build)
# -------------------------------------------------------------------
if [ ! -e /dev/loop-control ]; then
  mknod /dev/loop-control c 10 237
fi
for i in $(seq 0 7); do
  [ -b /dev/loop$i ] || mknod -m 660 /dev/loop$i b 7 $i
done

# Ensure /dev/net/tun exists (needed for TAP devices / Firecracker networking)
mkdir -p /dev/net
[ -e /dev/net/tun ] || mknod /dev/net/tun c 10 200
chmod 0666 /dev/net/tun

# Ensure /dev/kvm exists (needed for Firecracker)
[ -e /dev/kvm ] || mknod /dev/kvm c 10 232
chmod 660 /dev/kvm
chgrp kvm /dev/kvm 2>/dev/null || true

# -------------------------------------------------------------------
# 12. Create state directories
# -------------------------------------------------------------------
mkdir -p "${BOXCUTTER_HOME}/vms"
mkdir -p "${BOXCUTTER_HOME}/golden"

# Copy golden image build scripts
cp "${SRC}/golden/build.sh" "${BOXCUTTER_HOME}/golden/build.sh"
cp "${SRC}/golden/provision.sh" "${BOXCUTTER_HOME}/golden/provision.sh"
cp "${SRC}/golden/nss_catchall.c" "${BOXCUTTER_HOME}/golden/nss_catchall.c"
chmod +x "${BOXCUTTER_HOME}/golden/build.sh" "${BOXCUTTER_HOME}/golden/provision.sh"

# Copy Go source for building
cp -r "${SRC}/vmid" "${BOXCUTTER_HOME}/vmid" 2>/dev/null || true
cp -r "${SRC}/proxy" "${BOXCUTTER_HOME}/proxy" 2>/dev/null || true
cp -r "${SRC}/node" "${BOXCUTTER_HOME}/node" 2>/dev/null || true
cp -r "${SRC}/orchestrator" "${BOXCUTTER_HOME}/orchestrator" 2>/dev/null || true

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
echo "  2. Provision image:     sudo boxcutter-ctl golden provision"
echo "  3. Create a VM:         sudo boxcutter-ctl create agent-1"
echo "  4. Start the VM:        sudo boxcutter-ctl start agent-1"
echo ""
echo "Start all services:"
echo "  sudo systemctl start caddy boxcutter-net vmid boxcutter-proxy boxcutter-proxy-sync boxcutter-derper boxcutter-node boxcutter-orchestrator"
echo ""
