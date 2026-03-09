#!/bin/bash
# Build a .deb package for boxcutter-host.
#
# Usage:
#   bash host/build-deb.sh [VERSION]
#
# If VERSION is not provided, uses git describe.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="${SCRIPT_DIR}/.."

VERSION="${1:-$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)}"
# Strip leading 'v' from version
VERSION="${VERSION#v}"
# Ensure version starts with a digit (deb requirement)
if ! [[ "$VERSION" =~ ^[0-9] ]]; then
    VERSION="0.0.0+${VERSION}"
fi

echo "=== Building boxcutter-host ${VERSION} deb package ==="

# Build the Go binary
echo "--- Building binary ---"
cd "${SCRIPT_DIR}" && go build -ldflags "-X main.version=${VERSION}" -o boxcutter-host ./cmd/host/
echo "  Binary: $(ls -lh boxcutter-host | awk '{print $5}')"

# Create deb staging directory
STAGING=$(mktemp -d)
trap "rm -rf ${STAGING}" EXIT

PKG_DIR="${STAGING}/boxcutter-host_${VERSION}_amd64"
mkdir -p "${PKG_DIR}/DEBIAN"
mkdir -p "${PKG_DIR}/usr/local/bin"
mkdir -p "${PKG_DIR}/etc/systemd/system"
mkdir -p "${PKG_DIR}/usr/share/boxcutter"
mkdir -p "${PKG_DIR}/var/lib/boxcutter/.images"

# Install files
cp "${SCRIPT_DIR}/boxcutter-host" "${PKG_DIR}/usr/local/bin/"
cp "${SCRIPT_DIR}/boxcutter-host.service" "${PKG_DIR}/etc/systemd/system/"
cp "${SCRIPT_DIR}/mosquitto.conf" "${PKG_DIR}/usr/share/boxcutter/"
cp "${SCRIPT_DIR}/setup.sh" "${PKG_DIR}/usr/share/boxcutter/"
cp "${SCRIPT_DIR}/provision.sh" "${PKG_DIR}/usr/share/boxcutter/"
cp "${SCRIPT_DIR}/boxcutter.env" "${PKG_DIR}/usr/share/boxcutter/"

# Copy cloud-init templates
if [ -d "${REPO_DIR}/cloud-init" ]; then
    cp -r "${REPO_DIR}/cloud-init" "${PKG_DIR}/usr/share/boxcutter/"
fi

# Generate control file with version
sed "s/VERSION_PLACEHOLDER/${VERSION}/" "${SCRIPT_DIR}/debian/control" > "${PKG_DIR}/DEBIAN/control"
cp "${SCRIPT_DIR}/debian/postinst" "${PKG_DIR}/DEBIAN/postinst"
cp "${SCRIPT_DIR}/debian/prerm" "${PKG_DIR}/DEBIAN/prerm"
chmod 755 "${PKG_DIR}/DEBIAN/postinst" "${PKG_DIR}/DEBIAN/prerm"

# Build the package
OUTPUT_DIR="${REPO_DIR}/.release"
mkdir -p "${OUTPUT_DIR}"
dpkg-deb --build "${PKG_DIR}" "${OUTPUT_DIR}/boxcutter-host_${VERSION}_amd64.deb"

echo ""
echo "=== Package built ==="
echo "  ${OUTPUT_DIR}/boxcutter-host_${VERSION}_amd64.deb"
ls -lh "${OUTPUT_DIR}/boxcutter-host_${VERSION}_amd64.deb"

# Clean up binary
rm -f "${SCRIPT_DIR}/boxcutter-host"
