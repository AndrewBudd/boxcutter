#!/bin/bash
# Build a custom Firecracker kernel with nf_tables support.
#
# Usage:
#   ./build-kernel.sh [kernel-version]    # default: 6.1.128
#
# Prerequisites: gcc, make, flex, bison, libelf-dev, bc, libssl-dev
#
# Output: vmlinux in this directory
set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
KERNEL_VERSION="${1:-6.1.128}"
KERNEL_MAJOR="${KERNEL_VERSION%%.*}"
KERNEL_SRC="linux-${KERNEL_VERSION}"
KERNEL_TARBALL="${KERNEL_SRC}.tar.xz"
KERNEL_URL="https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_MAJOR}.x/${KERNEL_TARBALL}"
CONFIG_FILE="${SCRIPT_DIR}/microvm-kernel-x86_64-6.1.config"
NPROC=$(nproc)

cd "$SCRIPT_DIR"

echo "=== Building Firecracker kernel ${KERNEL_VERSION} ==="
echo "Config: ${CONFIG_FILE}"
echo "CPUs:   ${NPROC}"
echo ""

# --- Download kernel source ---
if [ ! -f "$KERNEL_TARBALL" ]; then
    echo "Downloading kernel source..."
    curl -fSL -o "$KERNEL_TARBALL" "$KERNEL_URL"
fi

# --- Extract ---
if [ ! -d "$KERNEL_SRC" ]; then
    echo "Extracting..."
    tar xf "$KERNEL_TARBALL"
fi

# --- Configure ---
echo "Configuring..."
cp "$CONFIG_FILE" "${KERNEL_SRC}/.config"
cd "$KERNEL_SRC"

# Resolve any new config options (answer defaults for anything not in our config)
make olddefconfig

# --- Build ---
echo "Building vmlinux (${NPROC} threads)..."
make -j"$NPROC" vmlinux

# --- Install ---
cp vmlinux "${SCRIPT_DIR}/vmlinux"
cd "$SCRIPT_DIR"

echo ""
echo "=== Kernel built ==="
echo "Output: ${SCRIPT_DIR}/vmlinux"
ls -lh "${SCRIPT_DIR}/vmlinux"

# Verify nf_tables is compiled in
if grep -q "nf_tables" "${KERNEL_SRC}/vmlinux" 2>/dev/null; then
    echo "nf_tables: built-in ✓"
else
    echo "Warning: nf_tables may not be compiled in — check config"
fi
