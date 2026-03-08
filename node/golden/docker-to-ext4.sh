#!/bin/bash
# Build the golden rootfs from a Dockerfile and convert to ext4 for Firecracker.
#
# Usage:
#   docker-to-ext4.sh [SIZE]    # default: 4G
#
# This replaces the old two-phase build (debootstrap + provision).
# The Dockerfile defines everything; this script just converts the
# container filesystem to an ext4 image.
set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BOXCUTTER_HOME="/var/lib/boxcutter"
GOLDEN_DIR="${BOXCUTTER_HOME}/golden"
OUTPUT="${GOLDEN_DIR}/rootfs.ext4"
SIZE="${1:-4G}"
IMAGE_NAME="boxcutter-golden"

echo "=== Building golden image from Dockerfile ==="
echo "Output: ${OUTPUT}"
echo "Size:   ${SIZE}"
echo ""

# --- Phase 1: Docker build ---
echo "Building container image..."
docker build -t "$IMAGE_NAME" "$SCRIPT_DIR" 2>&1 | tail -5
echo ""

# --- Phase 2: Export container filesystem ---
WORK=$(mktemp -d)
cleanup() {
  umount "${WORK}/mnt" 2>/dev/null || true
  rm -rf "${WORK}" 2>/dev/null || true
  docker rm -f "${IMAGE_NAME}-export" 2>/dev/null || true
}
trap cleanup EXIT

echo "Exporting container filesystem..."
docker create --name "${IMAGE_NAME}-export" "$IMAGE_NAME" /bin/true 2>/dev/null
docker export "${IMAGE_NAME}-export" > "${WORK}/rootfs.tar"
docker rm -f "${IMAGE_NAME}-export" >/dev/null 2>&1

# --- Phase 3: Create ext4 image and populate ---
echo "Creating ${SIZE} ext4 image..."
truncate -s "$SIZE" "${WORK}/rootfs.ext4"
mkfs.ext4 -F -q "${WORK}/rootfs.ext4"

mkdir -p "${WORK}/mnt"
mount -o loop "${WORK}/rootfs.ext4" "${WORK}/mnt"

echo "Extracting filesystem..."
tar xf "${WORK}/rootfs.tar" -C "${WORK}/mnt" 2>/dev/null

# Clean Docker artifacts
rm -f "${WORK}/mnt/.dockerenv"

# Replace Docker bind-mounted files with our versions
for f in resolv.conf hostname hosts; do
  if [ -f "${WORK}/mnt/etc/${f}.boxcutter" ]; then
    rm -f "${WORK}/mnt/etc/${f}"
    mv "${WORK}/mnt/etc/${f}.boxcutter" "${WORK}/mnt/etc/${f}"
  fi
done

# Ensure essential directories exist (Docker export may skip empty ones)
mkdir -p "${WORK}/mnt"/{dev,proc,sys,run,tmp}
chmod 1777 "${WORK}/mnt/tmp"

# Unmount
umount "${WORK}/mnt"

# --- Phase 4: Version and install ---
mkdir -p "$GOLDEN_DIR"

GOLDEN_SHA=$(sha256sum "${WORK}/rootfs.ext4" | cut -c1-12)
VERSIONED="${GOLDEN_DIR}/${GOLDEN_SHA}.ext4"

mv "${WORK}/rootfs.ext4" "$VERSIONED"

# Symlink rootfs.ext4 -> current version
ln -sf "${GOLDEN_SHA}.ext4" "$OUTPUT"
echo "$GOLDEN_SHA" > "${GOLDEN_DIR}/current-version"

ACTUAL_SIZE=$(du -h "$VERSIONED" | cut -f1)
echo ""
echo "=== Golden image built ==="
echo "Version: ${GOLDEN_SHA}"
echo "Path:    ${VERSIONED}"
echo "Size:    ${ACTUAL_SIZE} used / ${SIZE} max"
echo ""
