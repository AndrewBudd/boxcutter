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

# --- Network: DHCP on eth0 via systemd-networkd ---
mkdir -p "${WORK}/mnt/etc/systemd/network"
cat > "${WORK}/mnt/etc/systemd/network/20-eth0.network" << 'EOF'
[Match]
Name=eth0

[Network]
DHCP=yes
EOF

# Enable systemd-networkd and SSH (not resolved — use static DNS for fast boot)
mkdir -p "${WORK}/mnt/etc/systemd/system/multi-user.target.wants"
for svc in systemd-networkd ssh; do
  ln -sf "/lib/systemd/system/${svc}.service" \
    "${WORK}/mnt/etc/systemd/system/multi-user.target.wants/${svc}.service" 2>/dev/null || true
done

# --- Hostname ---
echo "boxcutter-vm" > "${WORK}/mnt/etc/hostname"
cat > "${WORK}/mnt/etc/hosts" << 'EOF'
127.0.0.1 localhost
127.0.1.1 boxcutter-vm
EOF

# --- fstab ---
echo "/dev/vda / ext4 defaults 0 1" > "${WORK}/mnt/etc/fstab"

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
nameserver 192.168.137.1
nameserver 8.8.8.8
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
if [ -f "${BOXCUTTER_HOME}/ssh/id_ed25519.pub" ]; then
  cat "${BOXCUTTER_HOME}/ssh/id_ed25519.pub" >> "${WORK}/mnt/home/dev/.ssh/authorized_keys"
fi

# User-provided trusted keys
if [ -f /etc/boxcutter/authorized_keys ]; then
  echo "  Adding user-trusted keys from /etc/boxcutter/authorized_keys"
  cat /etc/boxcutter/authorized_keys >> "${WORK}/mnt/home/dev/.ssh/authorized_keys"
fi

chown -R 1000:1000 "${WORK}/mnt/home/dev/.ssh"
chmod 700 "${WORK}/mnt/home/dev/.ssh"
chmod 600 "${WORK}/mnt/home/dev/.ssh/authorized_keys"

# --- Services declaration file ---
cat > "${WORK}/mnt/home/dev/.services" << 'EOF'
# Declare services as name=port, one per line
# Auto-discovered by boxcutter-proxy-sync
# Exposed as https://<name>.<vm-name>.vm.lan
# example=3000
EOF
chown 1000:1000 "${WORK}/mnt/home/dev/.services"

# --- Unmount and finalize ---
umount "${WORK}/mnt"

mkdir -p "$GOLDEN_DIR"
mv "${WORK}/rootfs.ext4" "$OUTPUT"

ACTUAL_SIZE=$(du -h "$OUTPUT" | cut -f1)
echo ""
echo "=== Golden base image built ==="
echo "Path: ${OUTPUT}"
echo "Size: ${ACTUAL_SIZE} used / ${SIZE} max"
echo ""
echo "The base image has Ubuntu + SSH + networking."
echo "To install dev tools, boot as a VM and provision:"
echo "  boxcutter-ctl golden provision"
echo ""
echo "Or create a VM directly (tools can be added later):"
echo "  boxcutter-ctl create agent-1"
