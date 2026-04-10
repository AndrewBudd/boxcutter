#!/bin/bash
# Rebuild tools.img (squashfs) and optionally deploy to running node VMs.
#
# Usage:
#   bash host/rebuild-tools.sh              Build tools.img only
#   bash host/rebuild-tools.sh --deploy     Build + deploy to all running nodes
#   bash host/rebuild-tools.sh --deploy N   Build + deploy to node N only
#
# This is a fast-path alternative to full `make provision-node` when only the
# tapegun daemon, boxcutter CLI, or plugin files have changed.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="${SCRIPT_DIR}/.."
source "${SCRIPT_DIR}/boxcutter.env"

BUILD_DIR="${REPO_DIR}/.build/tools"
TOOLS_IMG="/var/lib/boxcutter/tools.img"
DEPLOY=false
DEPLOY_NODE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --deploy)
            DEPLOY=true
            if [[ "${2:-}" =~ ^[0-9]+$ ]]; then
                DEPLOY_NODE="$2"
                shift
            fi
            shift
            ;;
        *)
            echo "Unknown option: $1"
            echo "Usage: bash host/rebuild-tools.sh [--deploy [NODE_NUMBER]]"
            exit 1
            ;;
    esac
done

echo "=== Rebuilding tools.img ==="

# Check dependencies
for cmd in mksquashfs go; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "ERROR: $cmd not found. This script must run on the host."
        exit 1
    fi
done

# Clean and create staging directory
rm -rf "$BUILD_DIR"
mkdir -p "${BUILD_DIR}/stage/bin" "${BUILD_DIR}/stage/etc/systemd"

# Build boxcutter CLI
echo "  compiling boxcutter CLI..."
(cd "${REPO_DIR}/tools" && CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -o "${BUILD_DIR}/stage/bin/boxcutter" ./cmd/boxcutter/)

# Copy tapegun daemon and service
echo "  copying boxcutter-tapegun..."
cp "${REPO_DIR}/node/golden/config/boxcutter-tapegun" "${BUILD_DIR}/stage/bin/boxcutter-tapegun"
chmod 755 "${BUILD_DIR}/stage/bin/boxcutter-tapegun"
cp "${REPO_DIR}/node/golden/config/boxcutter-tapegun.service" "${BUILD_DIR}/stage/etc/systemd/boxcutter-tapegun.service"

# Include tapegun-guest plugin if present
if [ -d "${REPO_DIR}/plugins/tapegun-guest" ]; then
    mkdir -p "${BUILD_DIR}/stage/plugins"
    cp -r "${REPO_DIR}/plugins/tapegun-guest" "${BUILD_DIR}/stage/plugins/tapegun-guest"
    echo "  included tapegun-guest plugin"
fi

# Include channel MCP server if present
if [ -d "${REPO_DIR}/channel" ]; then
    mkdir -p "${BUILD_DIR}/stage/channel"
    cp -r "${REPO_DIR}/channel/." "${BUILD_DIR}/stage/channel/"
    echo "  included boxcutter-channel MCP server"
fi

# Build squashfs
echo "  creating squashfs..."
mksquashfs "${BUILD_DIR}/stage" "${BUILD_DIR}/tools.img" -noappend -comp gzip -quiet
echo "  tools.img: $(du -h "${BUILD_DIR}/tools.img" | cut -f1)"

# Install locally on the host
sudo cp "${BUILD_DIR}/tools.img" "$TOOLS_IMG"
echo "  installed to $TOOLS_IMG"

# Deploy to nodes if requested
if [ "$DEPLOY" = "true" ]; then
    echo ""
    echo "=== Deploying to nodes ==="

    deploy_to_node() {
        local node_num="$1"
        local node_octet=$((NODE_IP_OFFSET + node_num))
        local node_ip="${NODE_SUBNET}.${node_octet}"

        echo "  node-${node_num} (${node_ip})..."
        if ! ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=3 \
                ubuntu@"${node_ip}" true 2>/dev/null; then
            echo "    SKIP: unreachable"
            return 1
        fi

        scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -q \
            "${BUILD_DIR}/tools.img" ubuntu@"${node_ip}":/tmp/tools.img
        ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            ubuntu@"${node_ip}" "sudo cp /tmp/tools.img ${TOOLS_IMG} && rm /tmp/tools.img"
        echo "    deployed — restart running VMs to pick up changes"
    }

    if [ -n "$DEPLOY_NODE" ]; then
        deploy_to_node "$DEPLOY_NODE"
    else
        # Discover running nodes from cluster state
        CLUSTER_FILE="/var/lib/boxcutter/cluster.json"
        if [ -f "$CLUSTER_FILE" ]; then
            NODE_NUMS=$(jq -r 'to_entries[] | select(.value.type == "node") | .key | ltrimstr("boxcutter-node-")' "$CLUSTER_FILE" 2>/dev/null)
        fi

        if [ -z "${NODE_NUMS:-}" ]; then
            # Fallback: probe IPs sequentially
            NODE_NUMS=""
            for n in $(seq 1 10); do
                node_octet=$((NODE_IP_OFFSET + n))
                node_ip="${NODE_SUBNET}.${node_octet}"
                if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=1 \
                        ubuntu@"${node_ip}" true 2>/dev/null; then
                    NODE_NUMS="${NODE_NUMS} ${n}"
                fi
            done
        fi

        if [ -z "${NODE_NUMS:-}" ]; then
            echo "  no reachable nodes found"
        else
            for n in $NODE_NUMS; do
                deploy_to_node "$n" || true
            done
        fi
    fi

    echo ""
    echo "NOTE: Running Firecracker VMs see tools.img as a read-only block device."
    echo "They must be restarted to pick up the new tools.img."
fi

echo ""
echo "Done."
