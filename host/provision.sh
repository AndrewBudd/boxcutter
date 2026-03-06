#!/bin/bash
# Build all binaries and package them + bootstrap bundle into the cloud-init ISO.
# This produces a self-contained Node VM that needs no host mounts after boot.
#
# Usage: bash host/provision.sh [--rebuild]
#   --rebuild: destroy existing VM disk and create fresh
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="${SCRIPT_DIR}/.."
source "${SCRIPT_DIR}/boxcutter.env"

IMAGES_DIR="${REPO_DIR}/.images"
BUNDLE_DIR="${HOME}/.boxcutter"

mkdir -p "$IMAGES_DIR"

# --- Validate bundle ---
if [ ! -f "${BUNDLE_DIR}/boxcutter.yaml" ]; then
  echo "Error: bootstrap bundle not found at ${BUNDLE_DIR}/"
  echo "Expected: ${BUNDLE_DIR}/boxcutter.yaml + ${BUNDLE_DIR}/secrets/"
  exit 1
fi

echo "=== Boxcutter Node VM Provisioning ==="
echo "Bundle: ${BUNDLE_DIR}"
echo ""

# --- Build Go binaries (cross-compile for the VM's arch) ---
echo "--- Building Go binaries ---"
BUILD_DIR=$(mktemp -d)
trap "rm -rf ${BUILD_DIR}" EXIT

(cd "${REPO_DIR}/vmid" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/vmid" ./cmd/vmid/)
echo "  vmid"

(cd "${REPO_DIR}/proxy" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-proxy" ./cmd/proxy/)
echo "  boxcutter-proxy"

(cd "${REPO_DIR}/node" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-node" ./cmd/node/)
echo "  boxcutter-node"

(cd "${REPO_DIR}/orchestrator" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-orchestrator" ./cmd/orchestrator/)
echo "  boxcutter-orchestrator"

(cd "${REPO_DIR}/orchestrator" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-ssh-orchestrator" ./cmd/ssh/)
echo "  boxcutter-ssh-orchestrator"

# --- Package payload ---
echo ""
echo "--- Packaging payload ---"
PAYLOAD_DIR="${BUILD_DIR}/payload"
mkdir -p "${PAYLOAD_DIR}/bin"
mkdir -p "${PAYLOAD_DIR}/scripts"
mkdir -p "${PAYLOAD_DIR}/systemd"
mkdir -p "${PAYLOAD_DIR}/config"
mkdir -p "${PAYLOAD_DIR}/golden"
mkdir -p "${PAYLOAD_DIR}/bundle/secrets"

# Binaries
cp "${BUILD_DIR}/vmid" "${PAYLOAD_DIR}/bin/"
cp "${BUILD_DIR}/boxcutter-proxy" "${PAYLOAD_DIR}/bin/"
cp "${BUILD_DIR}/boxcutter-node" "${PAYLOAD_DIR}/bin/"
cp "${BUILD_DIR}/boxcutter-orchestrator" "${PAYLOAD_DIR}/bin/"
cp "${BUILD_DIR}/boxcutter-ssh-orchestrator" "${PAYLOAD_DIR}/bin/"

# Shell scripts
for script in boxcutter-ctl boxcutter-net boxcutter-proxy-sync boxcutter-ssh \
              boxcutter-gateway boxcutter-names boxcutter-tls boxcutter-setup; do
  if [ -f "${REPO_DIR}/scripts/${script}" ]; then
    cp "${REPO_DIR}/scripts/${script}" "${PAYLOAD_DIR}/scripts/"
  fi
done

# Systemd units
cp "${REPO_DIR}"/systemd/*.service "${PAYLOAD_DIR}/systemd/"

# Config files
cp "${REPO_DIR}/config/Caddyfile" "${PAYLOAD_DIR}/config/" 2>/dev/null || true

# Golden image build scripts
cp "${REPO_DIR}/golden/build.sh" "${PAYLOAD_DIR}/golden/"
cp "${REPO_DIR}/golden/provision.sh" "${PAYLOAD_DIR}/golden/"
cp "${REPO_DIR}/golden/nss_catchall.c" "${PAYLOAD_DIR}/golden/"

# Bootstrap bundle
cp "${BUNDLE_DIR}/boxcutter.yaml" "${PAYLOAD_DIR}/bundle/"
cp "${BUNDLE_DIR}"/secrets/* "${PAYLOAD_DIR}/bundle/secrets/" 2>/dev/null || true

# Install script (modified to work from payload, not 9p)
cp "${REPO_DIR}/install.sh" "${PAYLOAD_DIR}/"

# Create tarball
PAYLOAD_TAR="${BUILD_DIR}/boxcutter-payload.tar.gz"
tar czf "$PAYLOAD_TAR" -C "${PAYLOAD_DIR}" .
PAYLOAD_SIZE=$(du -h "$PAYLOAD_TAR" | cut -f1)
echo "  Payload: ${PAYLOAD_SIZE}"

# --- Find SSH key ---
SSH_PUBKEY=""
for keyfile in ~/.ssh/id_ed25519.pub ~/.ssh/id_rsa.pub; do
  if [ -f "$keyfile" ]; then
    SSH_PUBKEY=$(cat "$keyfile")
    break
  fi
done
[ -z "$SSH_PUBKEY" ] && { echo "Error: no SSH public key found"; exit 1; }

# --- Generate cloud-init ISO ---
echo ""
echo "--- Generating cloud-init ISO ---"
CIDATA_DIR="${BUILD_DIR}/cidata"
mkdir -p "${CIDATA_DIR}"

# Encode payload as base64 for cloud-init write_files
PAYLOAD_B64=$(base64 -w0 "$PAYLOAD_TAR")

cat > "${CIDATA_DIR}/user-data" <<USERDATA
#cloud-config

hostname: boxcutter-node
manage_etc_hosts: true

users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ${SSH_PUBKEY}

package_update: true

packages:
  - jq
  - curl
  - wget
  - debootstrap
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

  - path: /opt/boxcutter-bootstrap.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      # Bootstrap script — unpacks payload and installs everything.
      # Runs once at first boot via cloud-init. After this, the VM is self-contained.
      set -e

      PAYLOAD_DIR="/opt/boxcutter-payload"
      mkdir -p "\$PAYLOAD_DIR"
      tar xzf /opt/boxcutter-payload.tar.gz -C "\$PAYLOAD_DIR"

      # Install Go binaries
      install -m 755 "\$PAYLOAD_DIR/bin/"* /usr/local/bin/

      # Install shell scripts
      for script in "\$PAYLOAD_DIR/scripts/"*; do
        [ -f "\$script" ] || continue
        install -m 755 "\$script" /usr/local/bin/
      done

      # Install systemd services
      cp "\$PAYLOAD_DIR/systemd/"*.service /etc/systemd/system/
      systemctl daemon-reload

      # Config files staged for after package install
      mkdir -p /etc/caddy/sites

      # Install bootstrap bundle
      mkdir -p /etc/boxcutter/secrets
      cp "\$PAYLOAD_DIR/bundle/boxcutter.yaml" /etc/boxcutter/
      for f in "\$PAYLOAD_DIR/bundle/secrets/"*; do
        [ -f "\$f" ] || continue
        dest="/etc/boxcutter/secrets/\$(basename "\$f")"
        cp "\$f" "\$dest"
      done
      chmod 600 /etc/boxcutter/secrets/*

      # Install golden image scripts
      BOXCUTTER_HOME="/var/lib/boxcutter"
      mkdir -p "\$BOXCUTTER_HOME/kernel" "\$BOXCUTTER_HOME/vms" "\$BOXCUTTER_HOME/golden"
      cp "\$PAYLOAD_DIR/golden/"* "\$BOXCUTTER_HOME/golden/"
      chmod +x "\$BOXCUTTER_HOME/golden/build.sh" "\$BOXCUTTER_HOME/golden/provision.sh"

      # Download Firecracker
      if ! command -v firecracker &>/dev/null; then
        FC_VERSION="v1.12.0"
        ARCH=\$(uname -m)
        curl -sL "https://github.com/firecracker-microvm/firecracker/releases/download/\${FC_VERSION}/firecracker-\${FC_VERSION}-\${ARCH}.tgz" | tar xz -C /tmp
        mv "/tmp/release-\${FC_VERSION}-\${ARCH}/firecracker-\${FC_VERSION}-\${ARCH}" /usr/local/bin/firecracker
        chmod +x /usr/local/bin/firecracker
        rm -rf "/tmp/release-\${FC_VERSION}-\${ARCH}"
      fi

      # Download Firecracker kernel
      if [ ! -f "\$BOXCUTTER_HOME/kernel/vmlinux" ]; then
        ARCH=\$(uname -m)
        curl -sL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/\${ARCH}/vmlinux-6.1.128" \
          -o "\$BOXCUTTER_HOME/kernel/vmlinux"
      fi

      # Install Caddy
      if ! command -v caddy &>/dev/null; then
        apt-get install -y debian-keyring debian-archive-keyring apt-transport-https
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
          | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
          | tee /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -o Dpkg::Options::="--force-confnew" caddy
      fi
      # Copy our Caddyfile AFTER install to avoid conffile prompt, then restart
      # so Caddy uses ports 8880/8443 instead of 80 (which vmid needs)
      cp "\$PAYLOAD_DIR/config/Caddyfile" /etc/caddy/Caddyfile 2>/dev/null || true
      systemctl enable caddy
      systemctl restart caddy

      # Install Tailscale
      if ! command -v tailscale &>/dev/null; then
        curl -fsSL https://tailscale.com/install.sh | sh
      fi
      systemctl enable tailscaled
      systemctl start tailscaled

      # Install Go (needed for derper build)
      if ! command -v go &>/dev/null; then
        GO_VERSION="1.22.5"
        curl -sL "https://go.dev/dl/go\${GO_VERSION}.linux-amd64.tar.gz" | tar xz -C /usr/local
        echo 'export PATH=\$PATH:/usr/local/go/bin' > /etc/profile.d/golang.sh
        export PATH=\$PATH:/usr/local/go/bin
      fi

      # Build DERP relay
      if ! command -v derper &>/dev/null; then
        export PATH=\$PATH:/usr/local/go/bin
        GOPATH=/root/go go install tailscale.com/cmd/derper@latest 2>/dev/null || true
        [ -f /root/go/bin/derper ] && mv /root/go/bin/derper /usr/local/bin/derper
      fi

      # Run boxcutter-setup (validates bundle, generates missing secrets, joins Tailscale, generates vmid config)
      /usr/local/bin/boxcutter-setup

      # Create default proxy allowlist
      if [ ! -f /etc/boxcutter/proxy-allowlist.conf ]; then
        cat > /etc/boxcutter/proxy-allowlist.conf <<'ALEOF'
      # Egress allowlist for paranoid mode VMs
      *.github.com
      github.com
      *.githubusercontent.com
      api.github.com
      *.npmjs.org
      registry.npmjs.org
      *.rubygems.org
      ALEOF
      fi

      # Set up DERP cert symlinks
      if command -v derper &>/dev/null; then
        mkdir -p /etc/boxcutter/derp-certs
        ln -sf /etc/boxcutter/secrets/leaf.crt /etc/boxcutter/derp-certs/10.0.0.1.crt
        ln -sf /etc/boxcutter/secrets/leaf.key /etc/boxcutter/derp-certs/10.0.0.1.key
      fi

      # Set up network infrastructure
      /usr/local/bin/boxcutter-net up

      # Ensure device nodes
      [ -e /dev/loop-control ] || mknod /dev/loop-control c 10 237
      for i in \$(seq 0 7); do
        [ -b /dev/loop\$i ] || mknod -m 660 /dev/loop\$i b 7 \$i
      done
      mkdir -p /dev/net
      [ -e /dev/net/tun ] || mknod /dev/net/tun c 10 200
      chmod 0666 /dev/net/tun
      [ -e /dev/kvm ] || mknod /dev/kvm c 10 232
      chmod 660 /dev/kvm
      chgrp kvm /dev/kvm 2>/dev/null || true

      # Set up SSH control interface
      useradd -r -m -s /bin/bash boxcutter 2>/dev/null || true
      echo "boxcutter ALL=(ALL) NOPASSWD: /usr/local/bin/boxcutter-ctl, /usr/bin/tee -a /etc/boxcutter/secrets/authorized-keys, /usr/bin/sort -u /etc/boxcutter/secrets/authorized-keys -o /etc/boxcutter/secrets/authorized-keys" > /etc/sudoers.d/boxcutter
      chmod 440 /etc/sudoers.d/boxcutter
      mkdir -p /home/boxcutter/.ssh
      if [ -f /etc/boxcutter/secrets/authorized-keys ]; then
        cp /etc/boxcutter/secrets/authorized-keys /home/boxcutter/.ssh/authorized_keys
      fi
      touch /home/boxcutter/.ssh/authorized_keys
      chmod 700 /home/boxcutter/.ssh
      chmod 600 /home/boxcutter/.ssh/authorized_keys
      chown -R boxcutter:boxcutter /home/boxcutter/.ssh

      # NSS catchall for any-user SSH
      BOXCUTTER_UID=\$(id -u boxcutter)
      BOXCUTTER_GID=\$(id -g boxcutter)
      cp "\$BOXCUTTER_HOME/golden/nss_catchall.c" /tmp/nss_catchall_node.c
      sed -i "s/result->pw_uid = 1000/result->pw_uid = \${BOXCUTTER_UID}/" /tmp/nss_catchall_node.c
      sed -i "s/result->pw_gid = 1000/result->pw_gid = \${BOXCUTTER_GID}/" /tmp/nss_catchall_node.c
      sed -i 's|/home/dev|/home/boxcutter|g' /tmp/nss_catchall_node.c
      sed -i 's/"avahi",/"avahi", "ubuntu", "caddy",/' /tmp/nss_catchall_node.c
      apt-get install -y gcc libc6-dev > /dev/null 2>&1
      gcc -shared -fPIC -o /usr/lib/x86_64-linux-gnu/libnss_catchall.so.2 /tmp/nss_catchall_node.c
      rm /tmp/nss_catchall_node.c
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
          ForceCommand /usr/local/bin/boxcutter-ssh
          AllowTcpForwarding no
          X11Forwarding no
      SSHEOF
      systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true

      # Enable services
      systemctl enable boxcutter-net
      systemctl enable vmid
      systemctl enable boxcutter-proxy 2>/dev/null || true
      systemctl enable boxcutter-proxy-sync 2>/dev/null || true
      systemctl enable boxcutter-derper 2>/dev/null || true
      systemctl enable boxcutter-node 2>/dev/null || true
      systemctl enable boxcutter-orchestrator 2>/dev/null || true

      # Start services
      systemctl start vmid
      systemctl start boxcutter-proxy 2>/dev/null || true
      systemctl start boxcutter-node 2>/dev/null || true
      systemctl start boxcutter-orchestrator 2>/dev/null || true
      systemctl start caddy 2>/dev/null || true

      # Cleanup payload
      rm -rf /opt/boxcutter-payload /opt/boxcutter-payload.tar.gz

      echo ""
      echo "============================================"
      echo "  Boxcutter Node VM provisioned!"
      echo "============================================"

runcmd:
  - bash /opt/boxcutter-bootstrap.sh
USERDATA

# Meta-data
cat > "${CIDATA_DIR}/meta-data" <<META
instance-id: boxcutter-$(date +%s)
local-hostname: boxcutter-node
META

# Network config
sed -e "s|NODE_IP_PLACEHOLDER|${NODE_IP}|" \
    -e "s|NODE_CIDR_PLACEHOLDER|${HOST_TAP_CIDR}|" \
    -e "s|HOST_TAP_IP_PLACEHOLDER|${HOST_TAP_IP}|" \
    -e "s|NODE_MAC_PLACEHOLDER|${NODE_MAC}|" \
    "${REPO_DIR}/cloud-init/network-config" > "${CIDATA_DIR}/network-config"

genisoimage -output "${IMAGES_DIR}/cloud-init.iso" \
  -volid cidata -joliet -rock \
  "${CIDATA_DIR}/user-data" "${CIDATA_DIR}/meta-data" "${CIDATA_DIR}/network-config" \
  2>/dev/null

echo "  Cloud-init ISO created"

# --- Handle VM disk ---
NODE_DISK_FILE="${IMAGES_DIR}/node.qcow2"
UBUNTU_IMG="${IMAGES_DIR}/ubuntu-noble-cloudimg-amd64.img"

if [ "${1:-}" = "--rebuild" ] || [ ! -f "$NODE_DISK_FILE" ]; then
  echo ""
  echo "--- Creating fresh VM disk ---"
  [ ! -f "$UBUNTU_IMG" ] && {
    echo "  Downloading Ubuntu cloud image..."
    wget -q --show-progress -O "$UBUNTU_IMG" \
      https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
  }
  rm -f "$NODE_DISK_FILE"
  qemu-img create -f qcow2 -b "$UBUNTU_IMG" -F qcow2 "$NODE_DISK_FILE" "$NODE_DISK"
  echo "  VM disk: ${NODE_DISK_FILE}"
fi

echo ""
echo "=== Provisioning complete ==="
echo ""
echo "Launch with: make launch-daemon"
echo "SSH:         ssh ubuntu@${NODE_IP}  (wait ~2-3 min for cloud-init)"
echo ""
echo "Cloud-init will:"
echo "  1. Install system packages"
echo "  2. Unpack boxcutter binaries + bundle"
echo "  3. Run boxcutter-setup (join Tailscale, generate certs, configure vmid)"
echo "  4. Start all services"
echo ""
