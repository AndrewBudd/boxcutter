#!/bin/bash
# Build and publish VM images to ghcr.io.
#
# This is the single entry point for the image pipeline. Run from the repo root:
#
#   ./host/publish-image.sh node          # build + push node image
#   ./host/publish-image.sh orchestrator  # build + push orchestrator image
#   ./host/publish-image.sh all           # build + push both
#   ./host/publish-image.sh node --build-only   # build without pushing
#   ./host/publish-image.sh node --push-only    # push previously built image
#
# Requirements: Go, qemu-system-x86_64, qemu-img, genisoimage, zstd, KVM, gh CLI (for push)
#
# This script handles sudo internally — run it as your normal user, not as root.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="${SCRIPT_DIR}/.."
IMAGES_DIR="${REPO_DIR}/.images"

# Ensure Go is available
export PATH="/usr/local/go/bin:$PATH"
if ! command -v go &>/dev/null; then
  echo "Error: Go not found. Install to /usr/local/go" >&2
  exit 1
fi

# Parse args
VM_TYPE="${1:-}"
BUILD=true
PUSH=true
TAG=""

shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --build-only) PUSH=false ;;
    --push-only)  BUILD=false ;;
    --tag)        shift; TAG="${1:-}" ;;
    --tag=*)      TAG="${1#--tag=}" ;;
  esac
  shift
done

if [ -z "$VM_TYPE" ] || [[ ! "$VM_TYPE" =~ ^(node|orchestrator|all)$ ]]; then
  echo "Usage: $0 <node|orchestrator|all> [--build-only] [--push-only] [--tag TAG]"
  exit 1
fi

# Determine tag from git
if [ -z "$TAG" ]; then
  TAG=$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo "latest")
fi

# Get gh token BEFORE sudo (gh auth context is per-user)
GH_TOKEN=""
if [ "$PUSH" = true ]; then
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    GH_TOKEN="$GITHUB_TOKEN"
  elif command -v gh &>/dev/null; then
    GH_TOKEN=$(gh auth token 2>/dev/null || true)
  fi
  if [ -z "$GH_TOKEN" ]; then
    echo "Error: No GitHub token for push. Run 'gh auth login' or set GITHUB_TOKEN." >&2
    exit 1
  fi
fi

# Expand "all" to both types
if [ "$VM_TYPE" = "all" ]; then
  TYPES=(node orchestrator)
else
  TYPES=("$VM_TYPE")
fi

for TYPE in "${TYPES[@]}"; do
  echo ""
  echo "========================================"
  echo "  ${TYPE} image — tag: ${TAG}"
  echo "========================================"

  IMAGE_PATH="${IMAGES_DIR}/${TYPE}-image.qcow2.zst"

  # --- Build ---
  if [ "$BUILD" = true ]; then
    echo ""
    echo "--- Building ${TYPE} image (requires sudo for KVM) ---"
    # build-image.sh may exit non-zero from cleanup (e.g. SSH disconnect on poweroff)
    # so check for the output file instead of relying on exit code
    sudo env PATH="$PATH" bash "${SCRIPT_DIR}/build-image.sh" "$TYPE" || true

    if [ ! -f "$IMAGE_PATH" ]; then
      echo "Error: Expected output not found: ${IMAGE_PATH}" >&2
      exit 1
    fi
    echo ""
    echo "Build complete: $(ls -lh "$IMAGE_PATH" | awk '{print $5}') ${IMAGE_PATH}"
  fi

  # --- Push ---
  if [ "$PUSH" = true ]; then
    if [ ! -f "$IMAGE_PATH" ]; then
      echo "Error: Image not found: ${IMAGE_PATH}" >&2
      echo "Run without --push-only to build first." >&2
      exit 1
    fi

    echo ""
    echo "--- Pushing ${TYPE} image to ghcr.io ---"

    # Build the host binary (for OCI push logic) — as current user, not root
    HOST_BIN="${REPO_DIR}/host/boxcutter-host"
    (cd "${REPO_DIR}/host" && go build -o boxcutter-host ./cmd/host/) 2>&1

    GITHUB_TOKEN="$GH_TOKEN" "$HOST_BIN" build-image "$TYPE" --push-only --tag "$TAG"
  fi
done

echo ""
echo "========================================"
echo "  Done!"
if [ "$PUSH" = true ]; then
  echo "  Tags: ${TAG}, latest"
  echo "  Registry: ghcr.io/andrewbudd/boxcutter"
fi
echo "========================================"
