#!/bin/bash
# Build and package cloud-init ISO for a Boxcutter VM.
#
# Usage:
#   bash host/provision.sh node [NAME] [--rebuild]         Build a Node VM ISO (from source)
#   bash host/provision.sh orchestrator [--rebuild]         Build the Orchestrator VM ISO (from source)
#   bash host/provision.sh node [NAME] --from-image        Generate slim config-only ISO for a pulled image
#   bash host/provision.sh orchestrator --from-image       Generate slim config-only ISO for a pulled image
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="${SCRIPT_DIR}/.."
source "${SCRIPT_DIR}/boxcutter.env"

IMAGES_DIR="${REPO_DIR}/.images"
# Find bootstrap bundle: check BOXCUTTER_BUNDLE env, then SUDO_USER's home, then HOME
if [ -n "${BOXCUTTER_BUNDLE:-}" ]; then
  BUNDLE_DIR="$BOXCUTTER_BUNDLE"
elif [ -n "${SUDO_USER:-}" ] && [ -d "$(eval echo ~${SUDO_USER})/.boxcutter" ]; then
  BUNDLE_DIR="$(eval echo ~${SUDO_USER})/.boxcutter"
else
  BUNDLE_DIR="${HOME}/.boxcutter"
fi

VM_TYPE="${1:-}"
shift || true

if [ "$VM_TYPE" != "node" ] && [ "$VM_TYPE" != "orchestrator" ]; then
  echo "Usage: bash host/provision.sh <node|orchestrator> [options]"
  echo ""
  echo "  node [NAME] [--rebuild]     Build a Node VM (NAME defaults to boxcutter-node-1)"
  echo "  orchestrator [--rebuild]    Build the Orchestrator VM"
  exit 1
fi

mkdir -p "$IMAGES_DIR"

# --- Parse args ---
REBUILD=false
FROM_IMAGE=false
VM_NAME=""
for arg in "$@"; do
  case "$arg" in
    --rebuild) REBUILD=true ;;
    --from-image) FROM_IMAGE=true ;;
    *) [ -z "$VM_NAME" ] && VM_NAME="$arg" ;;
  esac
done

# --- Validate bundle ---
if [ ! -f "${BUNDLE_DIR}/boxcutter.yaml" ]; then
  echo "Error: bootstrap bundle not found at ${BUNDLE_DIR}/"
  echo "Expected: ${BUNDLE_DIR}/boxcutter.yaml + ${BUNDLE_DIR}/secrets/"
  exit 1
fi

# --- Find SSH key ---
SSH_PUBKEY=""
REAL_HOME="${HOME}"
if [ -n "${SUDO_USER:-}" ]; then
  REAL_HOME="$(eval echo ~${SUDO_USER})"
fi
for keyfile in "${REAL_HOME}/.ssh/id_ed25519.pub" "${REAL_HOME}/.ssh/id_rsa.pub" ~/.ssh/id_ed25519.pub ~/.ssh/id_rsa.pub; do
  [ -f "$keyfile" ] && { SSH_PUBKEY=$(cat "$keyfile"); break; }
done
[ -z "$SSH_PUBKEY" ] && { echo "Error: no SSH public key found"; exit 1; }

# --- Shared: download Ubuntu base image ---
UBUNTU_IMG="${IMAGES_DIR}/ubuntu-noble-cloudimg-amd64.img"
ensure_ubuntu_img() {
  [ -f "$UBUNTU_IMG" ] && return
  echo "  Downloading Ubuntu cloud image..."
  wget -q --show-progress -O "$UBUNTU_IMG" \
    https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
}

# ======================================================================
# NODE VM
# ======================================================================
build_node() {
  local NAME="${VM_NAME:-boxcutter-node-1}"
  # Derive per-node networking from name (boxcutter-node-N)
  local NODE_NUM="${NAME##*-}"
  if ! [[ "$NODE_NUM" =~ ^[0-9]+$ ]]; then
    echo "Error: node name must end with a number (e.g. boxcutter-node-1)"
    exit 1
  fi
  local NODE_OCTET=$((NODE_IP_OFFSET + NODE_NUM))
  local THIS_NODE_IP="${NODE_SUBNET}.${NODE_OCTET}"
  local THIS_NODE_MAC="$(printf '52:54:00:00:00:%02x' "$NODE_OCTET")"
  echo "=== Provisioning Node VM: ${NAME} ==="
  echo "  Bridge IP: ${THIS_NODE_IP}, MAC: ${THIS_NODE_MAC}"
  echo "Bundle: ${BUNDLE_DIR}"
  echo ""

  # --- Build Go binaries ---
  echo "--- Building Go binaries ---"
  BUILD_DIR=$(mktemp -d)
  trap "rm -rf ${BUILD_DIR}" EXIT

  (cd "${REPO_DIR}/node/vmid" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/vmid" ./cmd/vmid/)
  echo "  vmid"
  (cd "${REPO_DIR}/node/proxy" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-proxy" ./cmd/proxy/)
  echo "  boxcutter-proxy"
  (cd "${REPO_DIR}/node/agent" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-node" ./cmd/node/)
  echo "  boxcutter-node"

  # --- Package payload ---
  echo ""
  echo "--- Packaging payload ---"
  local PD="${BUILD_DIR}/payload"
  mkdir -p "${PD}/bin" "${PD}/scripts" "${PD}/systemd" "${PD}/config" "${PD}/golden" "${PD}/bundle/secrets"

  cp "${BUILD_DIR}/vmid" "${BUILD_DIR}/boxcutter-proxy" "${BUILD_DIR}/boxcutter-node" "${PD}/bin/"

  for script in boxcutter-ctl boxcutter-net boxcutter-proxy-sync boxcutter-ssh boxcutter-tls boxcutter-setup; do
    [ -f "${REPO_DIR}/node/scripts/${script}" ] && cp "${REPO_DIR}/node/scripts/${script}" "${PD}/scripts/"
  done

  cp "${REPO_DIR}"/node/systemd/*.service "${PD}/systemd/"
  cp "${REPO_DIR}/node/config/Caddyfile" "${PD}/config/" 2>/dev/null || true
  cp "${REPO_DIR}"/node/golden/build.sh "${REPO_DIR}"/node/golden/provision.sh "${REPO_DIR}"/node/golden/nss_catchall.c "${REPO_DIR}"/node/golden/vsock_listen.c "${PD}/golden/"

  # Template node-specific values into boxcutter.yaml
  sed -e "s|BRIDGE_IP_PLACEHOLDER|${THIS_NODE_IP}|g" \
      -e "s|ORCHESTRATOR_URL_PLACEHOLDER|http://${ORCH_IP}:8801|g" \
      -e "s|HOSTNAME_PLACEHOLDER|${NAME}|g" \
      "${BUNDLE_DIR}/boxcutter.yaml" > "${PD}/bundle/boxcutter.yaml"
  cp "${BUNDLE_DIR}"/secrets/* "${PD}/bundle/secrets/" 2>/dev/null || true

  local PAYLOAD_TAR="${BUILD_DIR}/payload.tar.gz"
  tar czf "$PAYLOAD_TAR" -C "${PD}" .
  echo "  Payload: $(du -h "$PAYLOAD_TAR" | cut -f1)"

  # --- Cloud-init ---
  echo ""
  echo "--- Generating cloud-init ISO ---"
  local CIDATA="${BUILD_DIR}/cidata"
  mkdir -p "${CIDATA}"
  local PAYLOAD_B64=$(base64 -w0 "$PAYLOAD_TAR")

  cat > "${CIDATA}/user-data" <<USERDATA
#cloud-config

hostname: ${NAME}
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
      set -e
      PD="/opt/boxcutter-payload"
      mkdir -p "\$PD"
      tar xzf /opt/boxcutter-payload.tar.gz -C "\$PD"

      install -m 755 "\$PD/bin/"* /usr/local/bin/
      for s in "\$PD/scripts/"*; do [ -f "\$s" ] && install -m 755 "\$s" /usr/local/bin/; done
      cp "\$PD/systemd/"*.service /etc/systemd/system/
      systemctl daemon-reload

      mkdir -p /etc/caddy/sites
      mkdir -p /etc/boxcutter/secrets
      cp "\$PD/bundle/boxcutter.yaml" /etc/boxcutter/
      for f in "\$PD/bundle/secrets/"*; do [ -f "\$f" ] && cp "\$f" "/etc/boxcutter/secrets/\$(basename "\$f")"; done
      chmod 600 /etc/boxcutter/secrets/*

      BOXCUTTER_HOME="/var/lib/boxcutter"
      mkdir -p "\$BOXCUTTER_HOME/kernel" "\$BOXCUTTER_HOME/vms" "\$BOXCUTTER_HOME/golden"
      cp "\$PD/golden/"* "\$BOXCUTTER_HOME/golden/"
      chmod +x "\$BOXCUTTER_HOME/golden/build.sh" "\$BOXCUTTER_HOME/golden/provision.sh"

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
      cp "\$PD/config/Caddyfile" /etc/caddy/Caddyfile 2>/dev/null || true
      systemctl enable caddy
      systemctl restart caddy

      # Tailscale
      if ! command -v tailscale &>/dev/null; then curl -fsSL https://tailscale.com/install.sh | sh; fi
      systemctl enable tailscaled
      systemctl start tailscaled

      # ORAS CLI (for golden image OCI pulls)
      if ! command -v oras &>/dev/null; then
        ORAS_VERSION="1.2.0"
        curl -sLO "https://github.com/oras-project/oras/releases/download/v\${ORAS_VERSION}/oras_\${ORAS_VERSION}_linux_amd64.tar.gz"
        tar xzf "oras_\${ORAS_VERSION}_linux_amd64.tar.gz" -C /usr/local/bin oras
        rm -f "oras_\${ORAS_VERSION}_linux_amd64.tar.gz"
      fi

      # zstd (for golden image decompression) + mosquitto client tools (for debugging)
      apt-get install -y -qq zstd mosquitto-clients 2>/dev/null || true

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

      # Cluster SSH key: allows nodes to SSH to each other for migration
      if [ -f /etc/boxcutter/secrets/cluster-ssh.key ]; then
        mkdir -p /root/.ssh
        cp /etc/boxcutter/secrets/cluster-ssh.key /root/.ssh/cluster-ssh.key
        chmod 600 /root/.ssh/cluster-ssh.key
        printf '%s\n' 'Host 192.168.50.*' '  IdentityFile /root/.ssh/cluster-ssh.key' '  User ubuntu' '  StrictHostKeyChecking no' '  UserKnownHostsFile /dev/null' > /root/.ssh/config
        chmod 600 /root/.ssh/config
        # Add cluster pubkey to ubuntu's authorized_keys
        if [ -f /etc/boxcutter/secrets/cluster-ssh.key.pub ]; then
          cat /etc/boxcutter/secrets/cluster-ssh.key.pub >> /home/ubuntu/.ssh/authorized_keys
        fi
      fi

      # boxcutter-setup (generates secrets, joins Tailscale, generates vmid config)
      /usr/local/bin/boxcutter-setup

      # Proxy allowlist
      [ -f /etc/boxcutter/proxy-allowlist.conf ] || cat > /etc/boxcutter/proxy-allowlist.conf <<'ALEOF'
      *.github.com
      github.com
      *.githubusercontent.com
      api.github.com
      *.npmjs.org
      registry.npmjs.org
      *.rubygems.org
      ALEOF

      # DERP certs
      if command -v derper &>/dev/null; then
        mkdir -p /etc/boxcutter/derp-certs
        ln -sf /etc/boxcutter/secrets/leaf.crt /etc/boxcutter/derp-certs/10.0.0.1.crt
        ln -sf /etc/boxcutter/secrets/leaf.key /etc/boxcutter/derp-certs/10.0.0.1.key
      fi

      # Network + device nodes
      /usr/local/bin/boxcutter-net up
      [ -e /dev/loop-control ] || mknod /dev/loop-control c 10 237
      for i in \$(seq 0 7); do [ -b /dev/loop\$i ] || mknod -m 660 /dev/loop\$i b 7 \$i; done
      mkdir -p /dev/net
      [ -e /dev/net/tun ] || mknod /dev/net/tun c 10 200; chmod 0666 /dev/net/tun
      [ -e /dev/kvm ] || mknod /dev/kvm c 10 232; chmod 660 /dev/kvm
      chgrp kvm /dev/kvm 2>/dev/null || true

      # Enable + start services
      systemctl enable boxcutter-net vmid boxcutter-proxy boxcutter-proxy-sync boxcutter-derper boxcutter-node 2>/dev/null || true
      systemctl start vmid boxcutter-proxy boxcutter-node caddy 2>/dev/null || true

      rm -rf /opt/boxcutter-payload /opt/boxcutter-payload.tar.gz
      echo ""
      echo "============================================"
      echo "  Node VM ${NAME} provisioned!"
      echo "============================================"

runcmd:
  - bash /opt/boxcutter-bootstrap.sh
USERDATA

  cat > "${CIDATA}/meta-data" <<META
instance-id: ${NAME}-$(date +%s)
local-hostname: ${NAME}
META

  sed -e "s|NODE_IP_PLACEHOLDER|${THIS_NODE_IP}|" \
      -e "s|NODE_CIDR_PLACEHOLDER|${HOST_BRIDGE_CIDR}|" \
      -e "s|HOST_TAP_IP_PLACEHOLDER|${HOST_BRIDGE_IP}|" \
      -e "s|NODE_MAC_PLACEHOLDER|${THIS_NODE_MAC}|" \
      "${REPO_DIR}/cloud-init/network-config" > "${CIDATA}/network-config"

  local ISO="${IMAGES_DIR}/${NAME}-cloud-init.iso"
  genisoimage -output "$ISO" -volid cidata -joliet -rock \
    "${CIDATA}/user-data" "${CIDATA}/meta-data" "${CIDATA}/network-config" 2>/dev/null
  echo "  Cloud-init ISO: ${ISO}"

  # --- VM disk ---
  local DISK="${IMAGES_DIR}/${NAME}.qcow2"
  if [ "$REBUILD" = true ] || [ ! -f "$DISK" ]; then
    echo ""
    echo "--- Creating fresh VM disk ---"
    ensure_ubuntu_img
    rm -f "$DISK"
    qemu-img create -f qcow2 -b "$UBUNTU_IMG" -F qcow2 "$DISK" "$NODE_DISK"
    echo "  VM disk: ${DISK}"
  fi

  echo ""
  echo "=== Node provisioning complete ==="
  echo "Launch with: make launch-node NAME=${NAME}"
}

# ======================================================================
# ORCHESTRATOR VM
# ======================================================================
build_orchestrator() {
  echo "=== Provisioning Orchestrator VM ==="
  echo "Bundle: ${BUNDLE_DIR}"
  echo ""

  # --- Build Go binaries ---
  echo "--- Building Go binaries ---"
  BUILD_DIR=$(mktemp -d)
  trap "rm -rf ${BUILD_DIR}" EXIT

  (cd "${REPO_DIR}/orchestrator" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-orchestrator" ./cmd/orchestrator/)
  echo "  boxcutter-orchestrator"
  (cd "${REPO_DIR}/orchestrator" && GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/boxcutter-ssh-orchestrator" ./cmd/ssh/)
  echo "  boxcutter-ssh-orchestrator"

  # --- Package payload ---
  echo ""
  echo "--- Packaging payload ---"
  local PD="${BUILD_DIR}/payload"
  mkdir -p "${PD}/bin" "${PD}/scripts" "${PD}/systemd" "${PD}/bundle/secrets"

  cp "${BUILD_DIR}/boxcutter-orchestrator" "${BUILD_DIR}/boxcutter-ssh-orchestrator" "${PD}/bin/"
  cp "${REPO_DIR}/orchestrator/scripts/boxcutter-names" "${PD}/scripts/"
  cp "${REPO_DIR}/orchestrator/systemd/boxcutter-orchestrator.service" "${PD}/systemd/"

  # Include nss_catchall.c for SSH any-user support
  cp "${REPO_DIR}/node/golden/nss_catchall.c" "${PD}/"

  # Bundle (orchestrator only needs tailscale key + authorized keys)
  cp "${BUNDLE_DIR}/boxcutter.yaml" "${PD}/bundle/"
  for secret in tailscale-node-authkey authorized-keys; do
    [ -f "${BUNDLE_DIR}/secrets/${secret}" ] && cp "${BUNDLE_DIR}/secrets/${secret}" "${PD}/bundle/secrets/"
  done

  local PAYLOAD_TAR="${BUILD_DIR}/payload.tar.gz"
  tar czf "$PAYLOAD_TAR" -C "${PD}" .
  echo "  Payload: $(du -h "$PAYLOAD_TAR" | cut -f1)"

  # --- Cloud-init ---
  echo ""
  echo "--- Generating cloud-init ISO ---"
  local CIDATA="${BUILD_DIR}/cidata"
  mkdir -p "${CIDATA}"
  local PAYLOAD_B64=$(base64 -w0 "$PAYLOAD_TAR")

  cat > "${CIDATA}/user-data" <<USERDATA
#cloud-config

hostname: boxcutter
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
  - openssh-server
  - ca-certificates

write_files:
  - path: /opt/boxcutter-payload.tar.gz
    encoding: b64
    content: ${PAYLOAD_B64}
    permissions: '0644'

  - path: /opt/boxcutter-bootstrap.sh
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

      # Bundle
      mkdir -p /etc/boxcutter/secrets /var/lib/boxcutter
      cp "\$PD/bundle/boxcutter.yaml" /etc/boxcutter/
      for f in "\$PD/bundle/secrets/"*; do [ -f "\$f" ] && cp "\$f" "/etc/boxcutter/secrets/\$(basename "\$f")"; done
      chmod 600 /etc/boxcutter/secrets/* 2>/dev/null || true

      # Tailscale
      if ! command -v tailscale &>/dev/null; then curl -fsSL https://tailscale.com/install.sh | sh; fi
      systemctl enable tailscaled
      systemctl start tailscaled

      # Join Tailscale as "boxcutter" (the orchestrator hostname)
      if [ -f /etc/boxcutter/secrets/tailscale-node-authkey ]; then
        TS_KEY=\$(cat /etc/boxcutter/secrets/tailscale-node-authkey | tr -d '[:space:]')
        tailscale up --authkey="\$TS_KEY" --hostname=boxcutter
      fi

      # SSH control interface
      useradd -r -m -s /bin/bash boxcutter 2>/dev/null || true
      mkdir -p /home/boxcutter/.ssh
      [ -f /etc/boxcutter/secrets/authorized-keys ] && \
        cp /etc/boxcutter/secrets/authorized-keys /home/boxcutter/.ssh/authorized_keys
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
      systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true

      # Enable + start orchestrator
      systemctl enable boxcutter-orchestrator
      systemctl start boxcutter-orchestrator

      rm -rf /opt/boxcutter-payload /opt/boxcutter-payload.tar.gz
      echo ""
      echo "============================================"
      echo "  Orchestrator VM provisioned!"
      echo "  Tailscale hostname: boxcutter"
      echo "============================================"

runcmd:
  - bash /opt/boxcutter-bootstrap.sh
USERDATA

  cat > "${CIDATA}/meta-data" <<META
instance-id: boxcutter-orch-$(date +%s)
local-hostname: boxcutter
META

  # Orchestrator uses its own network config (different IP/MAC)
  sed -e "s|NODE_IP_PLACEHOLDER|${ORCH_IP}|" \
      -e "s|NODE_CIDR_PLACEHOLDER|${HOST_BRIDGE_CIDR}|" \
      -e "s|HOST_TAP_IP_PLACEHOLDER|${HOST_BRIDGE_IP}|" \
      -e "s|NODE_MAC_PLACEHOLDER|${ORCH_MAC}|" \
      "${REPO_DIR}/cloud-init/network-config" > "${CIDATA}/network-config"

  local ISO="${IMAGES_DIR}/orchestrator-cloud-init.iso"
  genisoimage -output "$ISO" -volid cidata -joliet -rock \
    "${CIDATA}/user-data" "${CIDATA}/meta-data" "${CIDATA}/network-config" 2>/dev/null
  echo "  Cloud-init ISO: ${ISO}"

  # --- VM disk ---
  local DISK="${IMAGES_DIR}/orchestrator.qcow2"
  if [ "$REBUILD" = true ] || [ ! -f "$DISK" ]; then
    echo ""
    echo "--- Creating fresh VM disk ---"
    ensure_ubuntu_img
    rm -f "$DISK"
    qemu-img create -f qcow2 -b "$UBUNTU_IMG" -F qcow2 "$DISK" "$ORCH_DISK"
    echo "  VM disk: ${DISK}"
  fi

  echo ""
  echo "=== Orchestrator provisioning complete ==="
  echo "Launch with: make launch-orchestrator"
}

# ======================================================================
# FROM-IMAGE: slim config-only cloud-init ISO (no binary build needed)
# ======================================================================
build_from_image_node() {
  local NAME="${VM_NAME:-boxcutter-node-1}"
  local NODE_NUM="${NAME##*-}"
  if ! [[ "$NODE_NUM" =~ ^[0-9]+$ ]]; then
    echo "Error: node name must end with a number (e.g. boxcutter-node-1)"
    exit 1
  fi
  local NODE_OCTET=$((NODE_IP_OFFSET + NODE_NUM))
  local THIS_NODE_IP="${NODE_SUBNET}.${NODE_OCTET}"
  local THIS_NODE_MAC="$(printf '52:54:00:00:00:%02x' "$NODE_OCTET")"

  echo "=== Provisioning Node VM (from image): ${NAME} ==="
  echo "  Bridge IP: ${THIS_NODE_IP}, MAC: ${THIS_NODE_MAC}"

  local CIDATA=$(mktemp -d)

  # Slim user-data: just configure networking, hostname, secrets
  cat > "${CIDATA}/user-data" <<USERDATA
#cloud-config
hostname: ${NAME}
manage_etc_hosts: true
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ${SSH_PUBKEY}

write_files:
  - path: /etc/boxcutter/boxcutter.yaml
    content: |
$(sed 's/^/      /' "${BUNDLE_DIR}/boxcutter.yaml")
    permissions: '0600'
$(for secret in "${BUNDLE_DIR}/secrets/"*; do
  [ -f "$secret" ] || continue
  local sname=$(basename "$secret")
  printf "  - path: /etc/boxcutter/secrets/%s\n" "$sname"
  printf "    content: |\n"
  sed 's/^/      /' "$secret"
  printf "    permissions: '0600'\n"
done)
  - path: /opt/boxcutter-config.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -e
      # Set hostname
      hostnamectl set-hostname ${NAME}

      # Configure boxcutter.yaml with real values
      sed -i "s|hostname:.*HOSTNAME_PLACEHOLDER|hostname: ${NAME}|" /etc/boxcutter/boxcutter.yaml
      sed -i "s|bridge_ip:.*|bridge_ip: ${THIS_NODE_IP}|" /etc/boxcutter/boxcutter.yaml
      sed -i "s|url:.*ORCHESTRATOR_URL_PLACEHOLDER|url: http://${ORCH_IP}:8801|" /etc/boxcutter/boxcutter.yaml

      # Run boxcutter-setup if available (generates derived secrets, joins Tailscale)
      if [ -x /usr/local/bin/boxcutter-setup ]; then
        /usr/local/bin/boxcutter-setup
      fi

      # Restart services to pick up new config
      systemctl restart boxcutter-node 2>/dev/null || true

runcmd:
  - bash /opt/boxcutter-config.sh
USERDATA

  cat > "${CIDATA}/meta-data" <<META
instance-id: ${NAME}-$(date +%s)
local-hostname: ${NAME}
META

  sed -e "s|NODE_IP_PLACEHOLDER|${THIS_NODE_IP}|" \
      -e "s|NODE_CIDR_PLACEHOLDER|${HOST_BRIDGE_CIDR}|" \
      -e "s|HOST_TAP_IP_PLACEHOLDER|${HOST_BRIDGE_IP}|" \
      -e "s|NODE_MAC_PLACEHOLDER|${THIS_NODE_MAC}|" \
      "${REPO_DIR}/cloud-init/network-config" > "${CIDATA}/network-config"

  local ISO="${IMAGES_DIR}/${NAME}-cloud-init.iso"
  genisoimage -output "$ISO" -volid cidata -joliet -rock \
    "${CIDATA}/user-data" "${CIDATA}/meta-data" "${CIDATA}/network-config" 2>/dev/null
  echo "  Cloud-init ISO: ${ISO}"

  # Create COW disk from base image
  local BASE="${IMAGES_DIR}/node-base.qcow2"
  local DISK="${IMAGES_DIR}/${NAME}.qcow2"
  if [ ! -f "$BASE" ]; then
    echo "Error: base image not found at ${BASE}"
    echo "Pull with: boxcutter-host pull node"
    exit 1
  fi
  if [ "$REBUILD" = true ] || [ ! -f "$DISK" ]; then
    qemu-img create -f qcow2 -b "$BASE" -F qcow2 "$DISK" "$NODE_DISK"
    echo "  VM disk: ${DISK} (COW on ${BASE})"
  fi

  echo ""
  echo "=== Node provisioning (from image) complete ==="
  rm -rf "$CIDATA"
}

build_from_image_orchestrator() {
  echo "=== Provisioning Orchestrator VM (from image) ==="

  local CIDATA=$(mktemp -d)

  cat > "${CIDATA}/user-data" <<USERDATA
#cloud-config
hostname: boxcutter
manage_etc_hosts: true
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - ${SSH_PUBKEY}

write_files:
  - path: /etc/boxcutter/boxcutter.yaml
    content: |
$(sed 's/^/      /' "${BUNDLE_DIR}/boxcutter.yaml")
    permissions: '0600'
$(for secret in "${BUNDLE_DIR}/secrets/"*; do
  [ -f "$secret" ] || continue
  local sname=$(basename "$secret")
  printf "  - path: /etc/boxcutter/secrets/%s\n" "$sname"
  printf "    content: |\n"
  sed 's/^/      /' "$secret"
  printf "    permissions: '0600'\n"
done)
  - path: /opt/boxcutter-config.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -e
      hostnamectl set-hostname boxcutter

      # Configure boxcutter.yaml with real values
      sed -i "s|hostname:.*HOSTNAME_PLACEHOLDER|hostname: boxcutter|" /etc/boxcutter/boxcutter.yaml
      sed -i "s|bridge_ip:.*BRIDGE_IP_PLACEHOLDER|bridge_ip: ${ORCH_IP}|" /etc/boxcutter/boxcutter.yaml
      sed -i "s|url:.*ORCHESTRATOR_URL_PLACEHOLDER|url: http://${ORCH_IP}:8801|" /etc/boxcutter/boxcutter.yaml

      # Sync authorized keys to boxcutter SSH user
      if id boxcutter &>/dev/null && [ -f /etc/boxcutter/secrets/authorized-keys ]; then
        mkdir -p /home/boxcutter/.ssh
        cp /etc/boxcutter/secrets/authorized-keys /home/boxcutter/.ssh/authorized_keys
        chown -R boxcutter:boxcutter /home/boxcutter/.ssh
        chmod 700 /home/boxcutter/.ssh
        chmod 600 /home/boxcutter/.ssh/authorized_keys
      fi

      # Cluster SSH key for migration
      if [ -f /etc/boxcutter/secrets/cluster-ssh.key ]; then
        mkdir -p /root/.ssh
        cp /etc/boxcutter/secrets/cluster-ssh.key /root/.ssh/cluster-ssh.key
        chmod 600 /root/.ssh/cluster-ssh.key
        printf '%s\n' 'Host 192.168.50.*' '  IdentityFile /root/.ssh/cluster-ssh.key' '  User ubuntu' '  StrictHostKeyChecking no' '  UserKnownHostsFile /dev/null' > /root/.ssh/config
        chmod 600 /root/.ssh/config
      fi

      # Join Tailscale
      if command -v tailscale &>/dev/null && [ -f /etc/boxcutter/secrets/tailscale-node-authkey ]; then
        NODE_KEY=\$(cat /etc/boxcutter/secrets/tailscale-node-authkey | tr -d '[:space:]')
        tailscale up --authkey="\$NODE_KEY" --hostname=boxcutter
      fi

      systemctl restart boxcutter-orchestrator 2>/dev/null || true

runcmd:
  - bash /opt/boxcutter-config.sh
USERDATA

  cat > "${CIDATA}/meta-data" <<META
instance-id: boxcutter-orch-$(date +%s)
local-hostname: boxcutter
META

  local ORCH_NET_IP="${CLOUD_INIT_IP:-${ORCH_IP}}"
  local ORCH_NET_MAC="${CLOUD_INIT_MAC:-${ORCH_MAC}}"
  sed -e "s|NODE_IP_PLACEHOLDER|${ORCH_NET_IP}|" \
      -e "s|NODE_CIDR_PLACEHOLDER|${HOST_BRIDGE_CIDR}|" \
      -e "s|HOST_TAP_IP_PLACEHOLDER|${HOST_BRIDGE_IP}|" \
      -e "s|NODE_MAC_PLACEHOLDER|${ORCH_NET_MAC}|" \
      "${REPO_DIR}/cloud-init/network-config" > "${CIDATA}/network-config"

  local ISO="${CLOUD_INIT_OUTPUT:-${IMAGES_DIR}/orchestrator-cloud-init.iso}"
  genisoimage -output "$ISO" -volid cidata -joliet -rock \
    "${CIDATA}/user-data" "${CIDATA}/meta-data" "${CIDATA}/network-config" 2>/dev/null
  echo "  Cloud-init ISO: ${ISO}"

  local BASE="${IMAGES_DIR}/orchestrator-base.qcow2"
  local DISK="${IMAGES_DIR}/orchestrator.qcow2"
  if [ ! -f "$BASE" ]; then
    echo "Error: base image not found at ${BASE}"
    echo "Pull with: boxcutter-host pull orchestrator"
    exit 1
  fi
  if [ "$REBUILD" = true ] || [ ! -f "$DISK" ]; then
    qemu-img create -f qcow2 -b "$BASE" -F qcow2 "$DISK" "$ORCH_DISK"
    echo "  VM disk: ${DISK} (COW on ${BASE})"
  fi

  echo ""
  echo "=== Orchestrator provisioning (from image) complete ==="
  rm -rf "$CIDATA"
}

# ======================================================================
# Dispatch
# ======================================================================
if [ "$FROM_IMAGE" = true ]; then
  case "$VM_TYPE" in
    node)         build_from_image_node ;;
    orchestrator) build_from_image_orchestrator ;;
  esac
else
  case "$VM_TYPE" in
    node)         build_node ;;
    orchestrator) build_orchestrator ;;
  esac
fi
