#!/bin/bash
# Build the golden rootfs ext4 image for Firecracker microVMs.
#
# Two-phase approach:
#   Phase 1: debootstrap a minimal Ubuntu rootfs (this script)
#   Phase 2: boot as Firecracker VM, SSH in, run provision.sh
#
# Usage:
#   boxcutter-ctl golden build         # Phase 1 only (base image)
#   boxcutter-ctl golden provision     # Phase 2 (boot + provision)
set -e

BOXCUTTER_HOME="/var/lib/boxcutter"
GOLDEN_DIR="${BOXCUTTER_HOME}/golden"
OUTPUT="${GOLDEN_DIR}/rootfs.ext4"
SIZE="${1:-25G}"

echo "=== Building Firecracker golden rootfs (Phase 1: base) ==="
echo "Output: ${OUTPUT}"
echo "Size:   ${SIZE}"
echo ""

WORK=$(mktemp -d)
cleanup() {
  umount "${WORK}/mnt" 2>/dev/null || true
  rm -rf "${WORK}" 2>/dev/null || true
}
trap cleanup EXIT

# Create ext4 image
echo "Creating ${SIZE} ext4 image..."
truncate -s "$SIZE" "${WORK}/rootfs.ext4"
mkfs.ext4 -F -q "${WORK}/rootfs.ext4"

# Mount
mkdir -p "${WORK}/mnt"
mount -o loop "${WORK}/rootfs.ext4" "${WORK}/mnt"

# Debootstrap Ubuntu Noble (minimal + essentials for SSH/network)
echo "Running debootstrap (Ubuntu Noble)... this takes a few minutes."
debootstrap \
  --include=systemd,systemd-sysv,dbus,openssh-server,sudo,curl,wget,jq,git,iproute2,iputils-ping,ca-certificates,locales,gpgv,gnupg \
  noble "${WORK}/mnt" http://archive.ubuntu.com/ubuntu

echo "Debootstrap complete. Configuring base system..."

# --- Serial console ---
mkdir -p "${WORK}/mnt/etc/systemd/system/getty.target.wants"
ln -sf /lib/systemd/system/serial-getty@.service \
  "${WORK}/mnt/etc/systemd/system/getty.target.wants/serial-getty@ttyS0.service"

# --- Network: kernel ip= handles everything, just enable SSH ---
# No systemd-networkd needed — Firecracker VMs get their IP from kernel boot args
mkdir -p "${WORK}/mnt/etc/systemd/system/multi-user.target.wants"
ln -sf /lib/systemd/system/ssh.service \
  "${WORK}/mnt/etc/systemd/system/multi-user.target.wants/ssh.service" 2>/dev/null || true

# --- Hostname ---
echo "boxcutter-vm" > "${WORK}/mnt/etc/hostname"
cat > "${WORK}/mnt/etc/hosts" << 'EOF'
127.0.0.1 localhost
127.0.1.1 boxcutter-vm
EOF

# --- fstab ---
echo "/dev/vda / ext4 defaults 0 1" > "${WORK}/mnt/etc/fstab"

# --- Set hostname from kernel ip= parameter ---
cat > "${WORK}/mnt/etc/systemd/system/set-hostname.service" << 'SVCEOF'
[Unit]
Description=Set hostname from kernel ip= parameter
DefaultDependencies=no
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/bin/bash -c 'H=$(cat /proc/cmdline | grep -oP "ip=[^:]*::[^:]*:[^:]*:\K[^:]+"); [ -n "$H" ] && echo "$H" > /etc/hostname && hostname "$H" && sed -i "s/127.0.1.1.*/127.0.1.1 $H/" /etc/hosts'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
SVCEOF
ln -sf /etc/systemd/system/set-hostname.service \
  "${WORK}/mnt/etc/systemd/system/multi-user.target.wants/set-hostname.service"

# --- Fix slow boot: entropy + random seed ---
# Firecracker VMs lack hardware RNG; systemd-random-seed blocks boot waiting for entropy.
# Mask the service and seed /dev/urandom via a oneshot service instead.
chroot "${WORK}/mnt" systemctl mask systemd-random-seed.service
cat > "${WORK}/mnt/etc/systemd/system/seed-entropy.service" << 'SVCEOF'
[Unit]
Description=Seed entropy from urandom at boot
DefaultDependencies=no
Before=sysinit.target
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/bin/dd if=/dev/urandom of=/var/lib/systemd/random-seed bs=512 count=1 status=none
RemainAfterExit=yes

[Install]
WantedBy=sysinit.target
SVCEOF
ln -sf /etc/systemd/system/seed-entropy.service \
  "${WORK}/mnt/etc/systemd/system/sysinit.target.wants/seed-entropy.service"

# --- DNS (static — no systemd-resolved for fast boot) ---
rm -f "${WORK}/mnt/etc/resolv.conf"
cat > "${WORK}/mnt/etc/resolv.conf" << 'EOF'
nameserver 8.8.8.8
nameserver 8.8.4.4
EOF

# --- Create dev user with passwordless sudo ---
chroot "${WORK}/mnt" useradd -m -s /bin/bash -G sudo -u 1000 dev 2>/dev/null || true
chroot "${WORK}/mnt" bash -c 'echo "dev:dev" | chpasswd'
echo "dev ALL=(ALL) NOPASSWD:ALL" > "${WORK}/mnt/etc/sudoers.d/dev"
chmod 440 "${WORK}/mnt/etc/sudoers.d/dev"

# --- Set root password for serial console ---
chroot "${WORK}/mnt" bash -c 'echo "root:root" | chpasswd'

# --- Inject SSH keys ---
echo "Injecting SSH keys..."
mkdir -p "${WORK}/mnt/home/dev/.ssh"
touch "${WORK}/mnt/home/dev/.ssh/authorized_keys"

# Node's internal key (for boxcutter-ctl shell)
if [ -f /etc/boxcutter/secrets/node-ssh.pub ]; then
  cat /etc/boxcutter/secrets/node-ssh.pub >> "${WORK}/mnt/home/dev/.ssh/authorized_keys"
fi

# User-provided trusted keys
if [ -f /etc/boxcutter/secrets/authorized-keys ]; then
  echo "  Adding user-trusted keys from /etc/boxcutter/secrets/authorized-keys"
  cat /etc/boxcutter/secrets/authorized-keys >> "${WORK}/mnt/home/dev/.ssh/authorized_keys"
fi

chown -R 1000:1000 "${WORK}/mnt/home/dev/.ssh"
chmod 700 "${WORK}/mnt/home/dev/.ssh"
chmod 600 "${WORK}/mnt/home/dev/.ssh/authorized_keys"

# --- Accept any SSH username (maps to dev) ---
echo "Building NSS catchall module..."
cp "${SRC:-$(dirname "$0")/..}/golden/nss_catchall.c" "${WORK}/mnt/tmp/nss_catchall.c" 2>/dev/null || \
  cp "$(dirname "$0")/nss_catchall.c" "${WORK}/mnt/tmp/nss_catchall.c"
cp "${SRC:-$(dirname "$0")/..}/golden/vsock_listen.c" "${WORK}/mnt/tmp/vsock_listen.c" 2>/dev/null || \
  cp "$(dirname "$0")/vsock_listen.c" "${WORK}/mnt/tmp/vsock_listen.c"
chroot "${WORK}/mnt" bash -c 'apt-get install -y gcc libc6-dev >/dev/null 2>&1 && \
  gcc -shared -fPIC -o /usr/lib/x86_64-linux-gnu/libnss_catchall.so.2 /tmp/nss_catchall.c && \
  gcc -o /usr/local/bin/boxcutter-vsock-listen /tmp/vsock_listen.c && \
  rm /tmp/nss_catchall.c /tmp/vsock_listen.c && \
  apt-get remove -y gcc >/dev/null 2>&1 || true'
sed -i 's/^passwd:.*/passwd:         files catchall/' "${WORK}/mnt/etc/nsswitch.conf"
sed -i 's/^shadow:.*/shadow:         files catchall/' "${WORK}/mnt/etc/nsswitch.conf"

# SSH: accept any user's keys via AuthorizedKeysCommand
cat > "${WORK}/mnt/usr/local/bin/auth-keys-any" << 'SCRIPT'
#!/bin/bash
cat /home/dev/.ssh/authorized_keys
SCRIPT
chmod 755 "${WORK}/mnt/usr/local/bin/auth-keys-any"

mkdir -p "${WORK}/mnt/etc/ssh/sshd_config.d"
cat > "${WORK}/mnt/etc/ssh/sshd_config.d/boxcutter.conf" << 'SSHEOF'
AuthorizedKeysCommand /usr/local/bin/auth-keys-any %u
AuthorizedKeysCommandUser root
SSHEOF

# --- Install Tailscale ---
echo "Installing Tailscale..."
chroot "${WORK}/mnt" bash -c 'curl -fsSL https://tailscale.com/install.sh | sh' 2>&1 | tail -1

# Configure tailscaled for Firecracker (userspace networking — no /dev/net/tun or iptables)
cat > "${WORK}/mnt/etc/default/tailscaled" << 'EOF'
PORT=0
FLAGS=--tun=userspace-networking
EOF

# Skip iptables cleanup on stop (not available in Firecracker)
mkdir -p "${WORK}/mnt/etc/systemd/system/tailscaled.service.d"
cat > "${WORK}/mnt/etc/systemd/system/tailscaled.service.d/firecracker.conf" << 'EOF'
[Service]
ExecStopPost=
EOF

# Enable tailscaled (Tailscale join is handled by the Node VM via SSH after boot —
# the auth key is never stored on the VM's disk)
chroot "${WORK}/mnt" systemctl enable tailscaled 2>/dev/null || true

# --- vsock listener for migration nudges ---
# (vsock_listen.c was compiled above alongside nss_catchall.c before gcc removal)

# Nudge script: re-establish tailscale network path after migration
cat > "${WORK}/mnt/usr/local/bin/boxcutter-nudge" << 'SCRIPT'
#!/bin/bash
# Called via vsock after snapshot migration to nudge tailscale.
# Force STUN re-probing to discover the new network path.
tailscale netcheck >/dev/null 2>&1 &
SCRIPT
chmod 755 "${WORK}/mnt/usr/local/bin/boxcutter-nudge"

# Systemd service for vsock listener
cat > "${WORK}/mnt/etc/systemd/system/boxcutter-vsock.service" << 'SVCEOF'
[Unit]
Description=Boxcutter vsock listener
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/boxcutter-vsock-listen
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
SVCEOF
chroot "${WORK}/mnt" systemctl enable boxcutter-vsock 2>/dev/null || true

# --- Services declaration file ---
cat > "${WORK}/mnt/home/dev/.services" << 'EOF'
# Declare services as name=port, one per line
# Auto-discovered by boxcutter-proxy-sync
# Accessible at http://<tailscale-ip>:<port>
# example=3000
EOF
chown 1000:1000 "${WORK}/mnt/home/dev/.services"

# --- Unmount and finalize ---
umount "${WORK}/mnt"

mkdir -p "$GOLDEN_DIR"

# Version the golden image by SHA256
GOLDEN_SHA=$(sha256sum "${WORK}/rootfs.ext4" | cut -c1-12)
VERSIONED="${GOLDEN_DIR}/${GOLDEN_SHA}.ext4"

mv "${WORK}/rootfs.ext4" "$VERSIONED"

# Symlink rootfs.ext4 → current version
ln -sf "${GOLDEN_SHA}.ext4" "$OUTPUT"

# Write version metadata
echo "$GOLDEN_SHA" > "${GOLDEN_DIR}/current-version"

ACTUAL_SIZE=$(du -h "$VERSIONED" | cut -f1)
echo ""
echo "=== Golden base image built ==="
echo "Version: ${GOLDEN_SHA}"
echo "Path:    ${VERSIONED}"
echo "Size:    ${ACTUAL_SIZE} used / ${SIZE} max"
echo ""
echo "Or create a VM directly:"
echo "  boxcutter-ctl create agent-1"
echo ""
