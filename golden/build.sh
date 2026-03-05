#!/bin/bash
# Build the golden rootfs ext4 image for Firecracker microVMs.
# Uses debootstrap to create an Ubuntu Noble rootfs, then provisions it.
set -e

BOXCUTTER_HOME="/var/lib/boxcutter"
GOLDEN_DIR="${BOXCUTTER_HOME}/golden"
OUTPUT="${GOLDEN_DIR}/rootfs.ext4"
SIZE="${1:-25G}"

# Find provision script
PROVISION=""
for p in "${GOLDEN_DIR}/provision.sh" "$(dirname "$0")/provision.sh" /mnt/boxcutter/golden/provision.sh; do
  [ -f "$p" ] && PROVISION="$p" && break
done

echo "=== Building Firecracker golden rootfs ==="
echo "Output: ${OUTPUT}"
echo "Size:   ${SIZE}"
echo ""

WORK=$(mktemp -d)
cleanup() {
  umount "${WORK}/mnt/proc" 2>/dev/null || true
  umount "${WORK}/mnt/sys" 2>/dev/null || true
  umount "${WORK}/mnt/dev" 2>/dev/null || true
  umount "${WORK}/mnt" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

# Create ext4 image
echo "Creating ${SIZE} ext4 image..."
truncate -s "$SIZE" "${WORK}/rootfs.ext4"
mkfs.ext4 -F -q "${WORK}/rootfs.ext4"

# Mount
mkdir -p "${WORK}/mnt"
mount -o loop "${WORK}/rootfs.ext4" "${WORK}/mnt"

# Debootstrap Ubuntu Noble
echo "Running debootstrap (Ubuntu Noble)... this takes a few minutes."
debootstrap \
  --include=systemd,systemd-sysv,dbus,openssh-server,sudo,curl,wget,jq,git,iproute2,iputils-ping,ca-certificates,locales \
  noble "${WORK}/mnt" http://archive.ubuntu.com/ubuntu

echo "Debootstrap complete. Configuring system..."

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

# Enable systemd-networkd and resolved
mkdir -p "${WORK}/mnt/etc/systemd/system/multi-user.target.wants"
ln -sf /lib/systemd/system/systemd-networkd.service \
  "${WORK}/mnt/etc/systemd/system/multi-user.target.wants/systemd-networkd.service"
ln -sf /lib/systemd/system/systemd-resolved.service \
  "${WORK}/mnt/etc/systemd/system/multi-user.target.wants/systemd-resolved.service"

# --- Hostname ---
echo "boxcutter-vm" > "${WORK}/mnt/etc/hostname"
cat > "${WORK}/mnt/etc/hosts" << 'EOF'
127.0.0.1 localhost
127.0.1.1 boxcutter-vm
EOF

# --- fstab ---
cat > "${WORK}/mnt/etc/fstab" << 'EOF'
/dev/vda / ext4 defaults 0 1
EOF

# --- DNS (for chroot, overridden by systemd-resolved at runtime) ---
echo "nameserver 8.8.8.8" > "${WORK}/mnt/etc/resolv.conf"

# --- Enable SSH ---
mkdir -p "${WORK}/mnt/etc/systemd/system/multi-user.target.wants"
ln -sf /lib/systemd/system/ssh.service \
  "${WORK}/mnt/etc/systemd/system/multi-user.target.wants/ssh.service" 2>/dev/null || true

# --- Run provision script in chroot ---
if [ -n "$PROVISION" ] && [ -f "$PROVISION" ]; then
  echo "Running provision script..."
  cp "$PROVISION" "${WORK}/mnt/tmp/provision.sh"
  chmod +x "${WORK}/mnt/tmp/provision.sh"
  mount --bind /proc "${WORK}/mnt/proc"
  mount --bind /sys "${WORK}/mnt/sys"
  mount --bind /dev "${WORK}/mnt/dev"
  chroot "${WORK}/mnt" /tmp/provision.sh
  umount "${WORK}/mnt/dev"
  umount "${WORK}/mnt/sys"
  umount "${WORK}/mnt/proc"
  rm -f "${WORK}/mnt/tmp/provision.sh"
fi

# --- Inject SSH public key for node → VM access ---
if [ -f "${BOXCUTTER_HOME}/ssh/id_ed25519.pub" ]; then
  echo "Injecting boxcutter SSH key..."
  mkdir -p "${WORK}/mnt/home/dev/.ssh"
  cp "${BOXCUTTER_HOME}/ssh/id_ed25519.pub" "${WORK}/mnt/home/dev/.ssh/authorized_keys"
  # dev user is UID 1000 (created by provision.sh)
  chown -R 1000:1000 "${WORK}/mnt/home/dev/.ssh"
  chmod 700 "${WORK}/mnt/home/dev/.ssh"
  chmod 600 "${WORK}/mnt/home/dev/.ssh/authorized_keys"
fi

# --- Unmount and finalize ---
umount "${WORK}/mnt"

# Move to final location (atomic)
mkdir -p "$GOLDEN_DIR"
mv "${WORK}/rootfs.ext4" "$OUTPUT"

ACTUAL_SIZE=$(du -h "$OUTPUT" | cut -f1)
echo ""
echo "=== Golden image built ==="
echo "Path: ${OUTPUT}"
echo "Size: ${ACTUAL_SIZE} used / ${SIZE} max"
echo ""
echo "Create a VM: boxcutter-ctl create agent-1"
