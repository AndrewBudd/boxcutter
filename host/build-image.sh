#!/bin/bash
# Build a pre-baked QCOW2 image for OCI distribution.
#
# This script produces a "fat" standalone QCOW2 with all packages and binaries
# pre-installed. It reuses the existing cloud-init bootstrap logic from
# provision.sh, then cleans up instance-specific state so the image is generic.
#
# Usage:
#   bash host/build-image.sh node              Build a node VM image
#   bash host/build-image.sh orchestrator      Build an orchestrator VM image
#
# Requirements:
#   - Go toolchain (for compiling boxcutter binaries)
#   - qemu-system-x86_64 + qemu-img
#   - genisoimage
#   - KVM (/dev/kvm)
#   - zstd
#
# Output:
#   .images/node-image.qcow2.zst         (compressed node image)
#   .images/orchestrator-image.qcow2.zst (compressed orchestrator image)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="${SCRIPT_DIR}/.."
source "${SCRIPT_DIR}/boxcutter.env"

IMAGES_DIR="${REPO_DIR}/.images"
BUILD_DIR=""

# Ensure Go is in PATH (may not be in root's PATH under sudo)
if ! command -v go &>/dev/null && [ -x /usr/local/go/bin/go ]; then
  export PATH="/usr/local/go/bin:$PATH"
fi

VM_TYPE="${1:-}"
if [ "$VM_TYPE" != "node" ] && [ "$VM_TYPE" != "orchestrator" ] && [ "$VM_TYPE" != "golden" ]; then
  echo "Usage: bash host/build-image.sh <node|orchestrator|golden>"
  exit 1
fi

cleanup() {
  [ -n "$BUILD_DIR" ] && rm -rf "$BUILD_DIR"
  # Kill any leftover QEMU from this build
  [ -f "${IMAGES_DIR}/build-${VM_TYPE}.pid" ] && {
    local pid
    pid=$(cat "${IMAGES_DIR}/build-${VM_TYPE}.pid" 2>/dev/null) || true
    kill "$pid" 2>/dev/null || true
    rm -f "${IMAGES_DIR}/build-${VM_TYPE}.pid"
  }
}
trap cleanup EXIT

mkdir -p "$IMAGES_DIR"
BUILD_DIR=$(mktemp -d)

echo "=== Building ${VM_TYPE} image for OCI distribution ==="
echo ""

# --- Download Ubuntu cloud image ---
UBUNTU_IMG="${IMAGES_DIR}/ubuntu-noble-cloudimg-amd64.img"
if [ ! -f "$UBUNTU_IMG" ]; then
  echo "--- Downloading Ubuntu cloud image ---"
  wget -q --show-progress -O "$UBUNTU_IMG" \
    https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
fi

# --- Golden image: special path (package existing golden rootfs) ---
if [ "$VM_TYPE" = "golden" ]; then
  echo "--- Packaging golden image for OCI distribution ---"

  # Look for golden image on a running node, or locally
  GOLDEN_SRC=""
  for candidate in /var/lib/boxcutter/golden/rootfs.ext4 "${REPO_DIR}/.images/golden-rootfs.ext4"; do
    if [ -f "$candidate" ]; then
      GOLDEN_SRC="$candidate"
      break
    fi
  done

  if [ -z "$GOLDEN_SRC" ]; then
    # Try to fetch from a running node
    for ip in 192.168.50.3 192.168.50.4 192.168.50.5; do
      if ssh -o ConnectTimeout=3 -o StrictHostKeyChecking=no ubuntu@${ip} "test -f /var/lib/boxcutter/golden/rootfs.ext4" 2>/dev/null; then
        echo "  Found golden image on ${ip}, copying..."
        scp -o StrictHostKeyChecking=no ubuntu@${ip}:/var/lib/boxcutter/golden/rootfs.ext4 "${IMAGES_DIR}/golden-rootfs.ext4"
        GOLDEN_SRC="${IMAGES_DIR}/golden-rootfs.ext4"
        break
      fi
    done
  fi

  if [ -z "$GOLDEN_SRC" ]; then
    echo "Error: No golden image found. Build one on a node first:"
    echo "  ssh ubuntu@<node-ip> sudo /var/lib/boxcutter/golden/docker-to-ext4.sh"
    exit 1
  fi

  echo "  Source: ${GOLDEN_SRC}"
  OUTPUT_EXT4="${IMAGES_DIR}/golden-image.ext4"
  cp "$GOLDEN_SRC" "$OUTPUT_EXT4"

  echo "--- Compressing with zstd ---"
  OUTPUT_ZST="${OUTPUT_EXT4}.zst"
  zstd -f -T0 "$OUTPUT_EXT4" -o "$OUTPUT_ZST"
  echo "  Compressed: ${OUTPUT_ZST} ($(du -h "$OUTPUT_ZST" | cut -f1))"

  echo ""
  echo "=== Golden image build complete ==="
  echo "  Image: ${OUTPUT_EXT4}"
  echo "  Compressed: ${OUTPUT_ZST}"
  echo ""
  echo "Push to OCI registry:"
  echo "  boxcutter-host build-image golden --push --tag VERSION"
  exit 0
fi

# --- Build Go binaries ---
echo "--- Building Go binaries ---"
if [ "$VM_TYPE" = "node" ]; then
  (cd "${REPO_DIR}/node/vmid" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/vmid" ./cmd/vmid/)
  echo "  vmid"
  (cd "${REPO_DIR}/node/proxy" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-proxy" ./cmd/proxy/)
  echo "  boxcutter-proxy"
  (cd "${REPO_DIR}/node/agent" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-node" ./cmd/node/)
  echo "  boxcutter-node"
else
  (cd "${REPO_DIR}/orchestrator" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-orchestrator" ./cmd/orchestrator/)
  echo "  boxcutter-orchestrator"
  (cd "${REPO_DIR}/orchestrator" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-ssh-orchestrator" ./cmd/ssh/)
  echo "  boxcutter-ssh-orchestrator"
fi

# --- Package payload (same structure as provision.sh, but with placeholder config) ---
echo ""
echo "--- Packaging payload ---"
PD="${BUILD_DIR}/payload"
mkdir -p "${PD}/bin" "${PD}/scripts" "${PD}/systemd" "${PD}/config"

if [ "$VM_TYPE" = "node" ]; then
  cp "${BUILD_DIR}/vmid" "${BUILD_DIR}/boxcutter-proxy" "${BUILD_DIR}/boxcutter-node" "${PD}/bin/"

  for script in boxcutter-ctl boxcutter-net boxcutter-proxy-sync boxcutter-ssh boxcutter-tls boxcutter-setup; do
    [ -f "${REPO_DIR}/node/scripts/${script}" ] && cp "${REPO_DIR}/node/scripts/${script}" "${PD}/scripts/"
  done

  cp "${REPO_DIR}"/node/systemd/*.service "${PD}/systemd/"
  cp "${REPO_DIR}/node/config/Caddyfile" "${PD}/config/" 2>/dev/null || true

  mkdir -p "${PD}/golden"
  cp "${REPO_DIR}"/node/golden/Dockerfile "${REPO_DIR}"/node/golden/docker-to-ext4.sh \
     "${REPO_DIR}"/node/golden/nss_catchall.c "${REPO_DIR}"/node/golden/vsock_listen.c \
     "${REPO_DIR}"/node/golden/gh-token-refresh.sh "${REPO_DIR}"/node/golden/gh-token-refresh.service \
     "${REPO_DIR}"/node/golden/gh-token-refresh.timer \
     "${PD}/golden/"
  cp -r "${REPO_DIR}"/node/golden/config "${PD}/golden/"
else
  cp "${BUILD_DIR}/boxcutter-orchestrator" "${BUILD_DIR}/boxcutter-ssh-orchestrator" "${PD}/bin/"
  cp "${REPO_DIR}/orchestrator/scripts/boxcutter-names" "${PD}/scripts/"
  cp "${REPO_DIR}/orchestrator/systemd/boxcutter-orchestrator.service" "${PD}/systemd/"
  cp "${REPO_DIR}/node/golden/nss_catchall.c" "${PD}/"
fi

PAYLOAD_TAR="${BUILD_DIR}/payload.tar.gz"
tar czf "$PAYLOAD_TAR" -C "${PD}" .
echo "  Payload: $(du -h "$PAYLOAD_TAR" | cut -f1)"

# --- Create the inject-config script (baked into image) ---
# This script runs on boot when a user provides a slim cloud-init ISO with
# just their secrets/config. It replaces the massive inline bootstrap.
cat > "${BUILD_DIR}/boxcutter-inject-config.sh" <<'INJECT_EOF'
#!/bin/bash
# Inject secrets and config into a pre-built boxcutter image.
# Called by cloud-init runcmd on first boot of a pulled image.
set -e

CONFIG_TAR="/opt/boxcutter-config.tar.gz"
[ -f "$CONFIG_TAR" ] || { echo "No config tarball found at $CONFIG_TAR"; exit 0; }

echo "Injecting boxcutter configuration..."

mkdir -p /etc/boxcutter/secrets
tar xzf "$CONFIG_TAR" -C /etc/boxcutter/
chmod 600 /etc/boxcutter/secrets/* 2>/dev/null || true

# Run setup (generates derived secrets, joins Tailscale, generates vmid config)
if command -v boxcutter-setup &>/dev/null; then
  /usr/local/bin/boxcutter-setup
fi

# Network setup (for node VMs)
if command -v boxcutter-net &>/dev/null; then
  /usr/local/bin/boxcutter-net up
fi

# Ensure device nodes exist (for node VMs)
if [ -f /usr/local/bin/boxcutter-node ]; then
  [ -e /dev/loop-control ] || mknod /dev/loop-control c 10 237
  for i in $(seq 0 7); do [ -b /dev/loop$i ] || mknod -m 660 /dev/loop$i b 7 $i; done
  mkdir -p /dev/net
  [ -e /dev/net/tun ] || mknod /dev/net/tun c 10 200; chmod 0666 /dev/net/tun
  [ -e /dev/kvm ] || mknod /dev/kvm c 10 232; chmod 660 /dev/kvm
  chgrp kvm /dev/kvm 2>/dev/null || true
fi

# Start services
systemctl daemon-reload
if [ -f /usr/local/bin/boxcutter-node ]; then
  systemctl start vmid boxcutter-proxy boxcutter-node caddy 2>/dev/null || true
else
  systemctl start boxcutter-orchestrator 2>/dev/null || true
fi

rm -f "$CONFIG_TAR"
echo "Configuration injected successfully."
INJECT_EOF
chmod 755 "${BUILD_DIR}/boxcutter-inject-config.sh"

# --- Generate a build SSH key (temporary, for waiting on cloud-init) ---
ssh-keygen -t ed25519 -f "${BUILD_DIR}/build-key" -N "" -q
BUILD_SSH_PUBKEY=$(cat "${BUILD_DIR}/build-key.pub")

# --- Create cloud-init for the build VM ---
echo ""
echo "--- Generating build cloud-init ---"

CIDATA="${BUILD_DIR}/cidata"
mkdir -p "$CIDATA"
PAYLOAD_B64=$(base64 -w0 "$PAYLOAD_TAR")

# Use a dummy IP for the build VM
BUILD_IP="192.168.50.200"
BUILD_MAC="52:54:00:ff:ff:01"
BUILD_TAP="tap-build"

if [ "$VM_TYPE" = "node" ]; then
  DISK_SIZE="$NODE_DISK"
  BUILD_VCPU="$NODE_VCPU"
  BUILD_RAM="$NODE_RAM"

  cat > "${CIDATA}/user-data" <<USERDATA
#cloud-config

hostname: build-node
manage_etc_hosts: true

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ${BUILD_SSH_PUBKEY}

package_update: true

packages:
  - jq
  - curl
  - wget
  - docker.io
  - e2fsprogs
  - iptables-persistent
  - socat
  - net-tools
  - openssh-server
  - ca-certificates
  - gnupg

write_files:
  - path: /opt/boxcutter-payload.tar.gz
    encoding: b64
    content: ${PAYLOAD_B64}
    permissions: '0644'

  - path: /opt/boxcutter-build-bootstrap.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -e
      PD="/opt/boxcutter-payload"
      mkdir -p "\$PD"
      tar xzf /opt/boxcutter-payload.tar.gz -C "\$PD"

      # Install binaries and scripts
      install -m 755 "\$PD/bin/"* /usr/local/bin/
      for s in "\$PD/scripts/"*; do [ -f "\$s" ] && install -m 755 "\$s" /usr/local/bin/; done
      cp "\$PD/systemd/"*.service /etc/systemd/system/
      systemctl daemon-reload

      # Directories
      mkdir -p /etc/caddy/sites /etc/boxcutter/secrets
      BOXCUTTER_HOME="/var/lib/boxcutter"
      mkdir -p "\$BOXCUTTER_HOME/kernel" "\$BOXCUTTER_HOME/vms" "\$BOXCUTTER_HOME/golden"
      cp -r "\$PD/golden/"* "\$BOXCUTTER_HOME/golden/" 2>/dev/null || true
      chmod +x "\$BOXCUTTER_HOME/golden/docker-to-ext4.sh" 2>/dev/null || true
      cp "\$PD/config/Caddyfile" /etc/caddy/Caddyfile 2>/dev/null || true

      # Firecracker
      if ! command -v firecracker &>/dev/null; then
        FC_VERSION="v1.12.0"; ARCH=\$(uname -m)
        curl -sL "https://github.com/firecracker-microvm/firecracker/releases/download/\${FC_VERSION}/firecracker-\${FC_VERSION}-\${ARCH}.tgz" | tar xz -C /tmp
        mv "/tmp/release-\${FC_VERSION}-\${ARCH}/firecracker-\${FC_VERSION}-\${ARCH}" /usr/local/bin/firecracker
        chmod +x /usr/local/bin/firecracker
        rm -rf "/tmp/release-\${FC_VERSION}-\${ARCH}"
      fi

      # Firecracker kernel
      [ -f "\$BOXCUTTER_HOME/kernel/vmlinux" ] || \
        curl -sL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/\$(uname -m)/vmlinux-6.1.128" \
          -o "\$BOXCUTTER_HOME/kernel/vmlinux"

      # Caddy
      if ! command -v caddy &>/dev/null; then
        apt-get install -y debian-keyring debian-archive-keyring apt-transport-https
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -o Dpkg::Options::="--force-confnew" caddy
      fi
      systemctl enable caddy

      # Tailscale
      if ! command -v tailscale &>/dev/null; then curl -fsSL https://tailscale.com/install.sh | sh; fi
      systemctl enable tailscaled

      # ORAS CLI (for pulling golden images from OCI registry)
      if ! command -v oras &>/dev/null; then
        ORAS_VERSION="1.2.2"
        curl -sLO "https://github.com/oras-project/oras/releases/download/v\${ORAS_VERSION}/oras_\${ORAS_VERSION}_linux_amd64.tar.gz"
        tar xzf "oras_\${ORAS_VERSION}_linux_amd64.tar.gz" -C /usr/local/bin/ oras
        rm -f "oras_\${ORAS_VERSION}_linux_amd64.tar.gz"
      fi

      # Go + DERP
      if ! command -v go &>/dev/null; then
        curl -sL "https://go.dev/dl/go1.22.5.linux-amd64.tar.gz" | tar xz -C /usr/local
        export PATH=\$PATH:/usr/local/go/bin
      fi
      if ! command -v derper &>/dev/null; then
        export PATH=\$PATH:/usr/local/go/bin
        GOPATH=/root/go go install tailscale.com/cmd/derper@latest 2>/dev/null || true
        [ -f /root/go/bin/derper ] && mv /root/go/bin/derper /usr/local/bin/derper
      fi

      # Proxy allowlist
      cat > /etc/boxcutter/proxy-allowlist.conf <<'ALEOF'
      *.github.com
      github.com
      *.githubusercontent.com
      api.github.com
      *.npmjs.org
      registry.npmjs.org
      *.rubygems.org
      ALEOF

      # Enable services (but don't start — no config yet)
      systemctl enable boxcutter-net vmid boxcutter-proxy boxcutter-proxy-sync boxcutter-derper boxcutter-node 2>/dev/null || true

      # Install the inject-config script for future slim cloud-init boots
      # (This is copied from the payload)

      # Signal build complete
      touch /opt/boxcutter-build-complete
      echo "Build bootstrap complete."

runcmd:
  - bash /opt/boxcutter-build-bootstrap.sh
USERDATA

else
  # Orchestrator
  DISK_SIZE="$ORCH_DISK"
  BUILD_VCPU="$ORCH_VCPU"
  BUILD_RAM="$ORCH_RAM"

  cat > "${CIDATA}/user-data" <<USERDATA
#cloud-config

hostname: build-orchestrator
manage_etc_hosts: true

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ${BUILD_SSH_PUBKEY}

package_update: true

packages:
  - jq
  - curl
  - openssh-server
  - ca-certificates

write_files:
  - path: /opt/boxcutter-payload.tar.gz
    encoding: b64
    content: ${PAYLOAD_B64}
    permissions: '0644'

  - path: /opt/boxcutter-build-bootstrap.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -e
      PD="/opt/boxcutter-payload"
      mkdir -p "\$PD"
      tar xzf /opt/boxcutter-payload.tar.gz -C "\$PD"

      install -m 755 "\$PD/bin/"* /usr/local/bin/
      for s in "\$PD/scripts/"*; do [ -f "\$s" ] && install -m 755 "\$s" /usr/local/bin/; done
      cp "\$PD/systemd/"*.service /etc/systemd/system/
      systemctl daemon-reload

      mkdir -p /etc/boxcutter/secrets /var/lib/boxcutter

      # Tailscale
      if ! command -v tailscale &>/dev/null; then curl -fsSL https://tailscale.com/install.sh | sh; fi
      systemctl enable tailscaled

      # SSH control interface
      useradd -r -m -s /bin/bash boxcutter 2>/dev/null || true
      mkdir -p /home/boxcutter/.ssh
      touch /home/boxcutter/.ssh/authorized_keys
      chmod 700 /home/boxcutter/.ssh
      chmod 600 /home/boxcutter/.ssh/authorized_keys
      chown -R boxcutter:boxcutter /home/boxcutter/.ssh

      # NSS catchall (any SSH username → boxcutter)
      BOXCUTTER_UID=\$(id -u boxcutter)
      BOXCUTTER_GID=\$(id -g boxcutter)
      cp "\$PD/nss_catchall.c" /tmp/nss_catchall.c
      sed -i "s/result->pw_uid = 1000/result->pw_uid = \${BOXCUTTER_UID}/" /tmp/nss_catchall.c
      sed -i "s/result->pw_gid = 1000/result->pw_gid = \${BOXCUTTER_GID}/" /tmp/nss_catchall.c
      sed -i 's|/home/dev|/home/boxcutter|g' /tmp/nss_catchall.c
      sed -i 's/"avahi",/"avahi", "ubuntu",/' /tmp/nss_catchall.c
      apt-get install -y gcc libc6-dev > /dev/null 2>&1
      gcc -shared -fPIC -o /usr/lib/x86_64-linux-gnu/libnss_catchall.so.2 /tmp/nss_catchall.c
      rm /tmp/nss_catchall.c
      apt-get remove -y gcc > /dev/null 2>&1 || true
      sed -i 's/^passwd:.*/passwd:         files catchall/' /etc/nsswitch.conf
      sed -i 's/^shadow:.*/shadow:         files catchall/' /etc/nsswitch.conf

      cat > /usr/local/bin/auth-keys-any <<'SCRIPT'
      #!/bin/bash
      cat /home/boxcutter/.ssh/authorized_keys
      SCRIPT
      chmod 755 /usr/local/bin/auth-keys-any

      mkdir -p /etc/ssh/sshd_config.d
      cat > /etc/ssh/sshd_config.d/boxcutter.conf <<'SSHEOF'
      AuthorizedKeysCommand /usr/local/bin/auth-keys-any %u
      AuthorizedKeysCommandUser root

      Match User !ubuntu,!root,*
          ForceCommand /usr/local/bin/boxcutter-ssh-orchestrator
          AllowTcpForwarding no
          X11Forwarding no
      SSHEOF

      # Enable orchestrator service (but don't start — no config yet)
      systemctl enable boxcutter-orchestrator

      # Signal build complete
      touch /opt/boxcutter-build-complete
      echo "Build bootstrap complete."

runcmd:
  - bash /opt/boxcutter-build-bootstrap.sh
USERDATA
fi

cat > "${CIDATA}/meta-data" <<META
instance-id: build-${VM_TYPE}-$(date +%s)
local-hostname: build-${VM_TYPE}
META

# Network config for the build VM
cat > "${CIDATA}/network-config" <<NETCFG
version: 2
ethernets:
  nodeif:
    match:
      macaddress: "${BUILD_MAC}"
    addresses:
      - ${BUILD_IP}/${HOST_BRIDGE_CIDR}
    routes:
      - to: default
        via: ${HOST_BRIDGE_IP}
    nameservers:
      addresses:
        - 8.8.8.8
        - 8.8.4.4
NETCFG

ISO="${BUILD_DIR}/build-cloud-init.iso"
genisoimage -output "$ISO" -volid cidata -joliet -rock \
  "${CIDATA}/user-data" "${CIDATA}/meta-data" "${CIDATA}/network-config" 2>/dev/null

# --- Create standalone QCOW2 (not COW-backed) ---
echo ""
echo "--- Creating build VM disk ---"
DISK="${BUILD_DIR}/build-${VM_TYPE}.qcow2"
qemu-img create -f qcow2 -b "$UBUNTU_IMG" -F qcow2 "$DISK" "$DISK_SIZE"

# --- Boot the build VM ---
echo ""
echo "--- Booting build VM (this will take several minutes) ---"
echo "    Cloud-init will install all packages and tools..."

# Ensure the build TAP exists
if ! ip link show "$BUILD_TAP" &>/dev/null; then
  sudo ip tuntap add dev "$BUILD_TAP" mode tap user "$(whoami)"
  sudo ip link set "$BUILD_TAP" master "$BRIDGE_DEVICE" 2>/dev/null || true
  sudo ip link set "$BUILD_TAP" up
fi

qemu-system-x86_64 \
  -enable-kvm \
  -cpu host \
  -smp "$BUILD_VCPU" \
  -m "$BUILD_RAM" \
  -drive "file=${DISK},format=qcow2,if=virtio" \
  -drive "file=${ISO},format=raw,if=virtio" \
  -netdev "tap,id=net0,ifname=${BUILD_TAP},script=no,downscript=no" \
  -device "virtio-net-pci,netdev=net0,mac=${BUILD_MAC}" \
  -display none \
  -serial "file:${BUILD_DIR}/console.log" \
  -daemonize \
  -pidfile "${IMAGES_DIR}/build-${VM_TYPE}.pid"

BUILD_PID=$(cat "${IMAGES_DIR}/build-${VM_TYPE}.pid")
echo "  Build VM started (PID ${BUILD_PID})"

# --- Wait for cloud-init to finish ---
echo "  Waiting for cloud-init to complete..."
SSH_OPTS="-i ${BUILD_DIR}/build-key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR"
MAX_WAIT=600  # 10 minutes
ELAPSED=0

while [ $ELAPSED -lt $MAX_WAIT ]; do
  if ssh $SSH_OPTS ubuntu@${BUILD_IP} "test -f /opt/boxcutter-build-complete" 2>/dev/null; then
    echo "  Cloud-init complete!"
    break
  fi
  sleep 10
  ELAPSED=$((ELAPSED + 10))
  echo "  ... waiting (${ELAPSED}s)"
done

if [ $ELAPSED -ge $MAX_WAIT ]; then
  echo "ERROR: Cloud-init did not complete within ${MAX_WAIT}s"
  echo "Console log:"
  tail -50 "${BUILD_DIR}/console.log"
  exit 1
fi

# --- Install the inject-config script into the image ---
echo "  Installing inject-config script..."
scp $SSH_OPTS "${BUILD_DIR}/boxcutter-inject-config.sh" ubuntu@${BUILD_IP}:/tmp/
ssh $SSH_OPTS ubuntu@${BUILD_IP} "sudo cp /tmp/boxcutter-inject-config.sh /opt/boxcutter-inject-config.sh && sudo chmod 755 /opt/boxcutter-inject-config.sh"

# --- Clean up instance-specific state ---
echo ""
echo "--- Cleaning up instance state ---"
ssh $SSH_OPTS ubuntu@${BUILD_IP} <<'CLEANUP' || true
sudo bash -c '
  # Remove cloud-init state so it re-runs on next boot
  cloud-init clean --logs 2>/dev/null || rm -rf /var/lib/cloud/

  # Remove machine-id (regenerated on boot)
  truncate -s 0 /etc/machine-id
  rm -f /var/lib/dbus/machine-id

  # Remove SSH host keys (regenerated on boot)
  rm -f /etc/ssh/ssh_host_*

  # Remove any secrets or config (these come from cloud-init at deploy time)
  rm -rf /etc/boxcutter/secrets/*
  rm -f /etc/boxcutter/boxcutter.yaml

  # Remove Tailscale state (logout first to deregister device from tailnet)
  tailscale logout 2>/dev/null || true
  systemctl stop tailscaled 2>/dev/null || true
  rm -rf /var/lib/tailscale/*

  # Remove build artifacts
  rm -f /opt/boxcutter-payload.tar.gz
  rm -rf /opt/boxcutter-payload
  rm -f /opt/boxcutter-build-bootstrap.sh
  rm -f /opt/boxcutter-build-complete

  # Remove build SSH key
  rm -f /home/ubuntu/.ssh/authorized_keys
  truncate -s 0 /home/ubuntu/.ssh/authorized_keys

  # Clean apt cache
  apt-get clean
  rm -rf /var/lib/apt/lists/*

  # Clear logs
  journalctl --flush --rotate --vacuum-time=1s 2>/dev/null || true
  find /var/log -type f -exec truncate -s 0 {} \; 2>/dev/null || true

  # Clear bash history
  truncate -s 0 /root/.bash_history 2>/dev/null || true
  truncate -s 0 /home/ubuntu/.bash_history 2>/dev/null || true

  sync
'
CLEANUP

# --- Shut down the build VM ---
echo ""
echo "--- Shutting down build VM ---"
ssh $SSH_OPTS ubuntu@${BUILD_IP} "sudo poweroff" 2>/dev/null || true

# Wait for QEMU to exit
WAIT=0
while kill -0 "$BUILD_PID" 2>/dev/null && [ $WAIT -lt 30 ]; do
  sleep 1
  WAIT=$((WAIT + 1))
done
kill "$BUILD_PID" 2>/dev/null || true
rm -f "${IMAGES_DIR}/build-${VM_TYPE}.pid"

# Clean up TAP
sudo ip link del "$BUILD_TAP" 2>/dev/null || true

# --- Convert to standalone QCOW2 (collapse COW layers + compress) ---
echo ""
echo "--- Converting to standalone QCOW2 ---"
OUTPUT_QCOW2="${IMAGES_DIR}/${VM_TYPE}-image.qcow2"
qemu-img convert -c -O qcow2 "$DISK" "$OUTPUT_QCOW2"
echo "  QCOW2: ${OUTPUT_QCOW2} ($(du -h "$OUTPUT_QCOW2" | cut -f1))"

# --- Compress with zstd ---
echo "--- Compressing with zstd ---"
OUTPUT_ZST="${OUTPUT_QCOW2}.zst"
zstd -f -T0 "$OUTPUT_QCOW2" -o "$OUTPUT_ZST"
echo "  Compressed: ${OUTPUT_ZST} ($(du -h "$OUTPUT_ZST" | cut -f1))"

echo ""
echo "=== Image build complete ==="
echo "  QCOW2: ${OUTPUT_QCOW2}"
echo "  Compressed: ${OUTPUT_ZST}"
echo ""
echo "Push to OCI registry:"
echo "  oras push ghcr.io/AndrewBudd/boxcutter/${VM_TYPE}:VERSION \\"
echo "    --artifact-type application/vnd.boxcutter.vm.v1 \\"
echo "    ${OUTPUT_ZST}:application/vnd.boxcutter.vm.qcow2.v1+zstd"
