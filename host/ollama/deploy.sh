#!/bin/bash
# Deploy the Ollama + LiteLLM inference stack to /opt/ollama-docker-compose/.
#
# Reads the OpenRouter API key from ~/.boxcutter/secrets/openrouter-api-key.
# Generates a random LiteLLM master key if one doesn't exist.
#
# Usage:
#   bash host/ollama/deploy.sh                 # Deploy and start
#   bash host/ollama/deploy.sh --pull-model     # Also pull qwen3-coder on both instances
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_DIR="/opt/ollama-docker-compose"
SECRET_FILE="$HOME/.boxcutter/secrets/openrouter-api-key"
PULL_MODEL=false

for arg in "$@"; do
    case "$arg" in
        --pull-model) PULL_MODEL=true ;;
        *) echo "Unknown option: $arg"; exit 1 ;;
    esac
done

echo "=== Deploying inference stack ==="

# Create deploy directory
sudo mkdir -p "$DEPLOY_DIR"

# Copy config files
sudo cp "$SCRIPT_DIR/docker-compose.yml" "$DEPLOY_DIR/docker-compose.yml"
sudo cp "$SCRIPT_DIR/litellm-config.yaml" "$DEPLOY_DIR/litellm-config.yaml"
echo "  copied docker-compose.yml and litellm-config.yaml"

# Build .env from secrets
ENV_FILE="$DEPLOY_DIR/.env"
if [ -f "$SECRET_FILE" ]; then
    OPENROUTER_KEY=$(cat "$SECRET_FILE" | tr -d '[:space:]')
    echo "  read OpenRouter API key from $SECRET_FILE"
else
    echo "  WARNING: $SECRET_FILE not found, OpenRouter fallback will not work"
    OPENROUTER_KEY="sk-or-v1-not-configured"
fi

# Generate or preserve LiteLLM master key
if [ -f "$ENV_FILE" ]; then
    EXISTING_MASTER=$(grep '^LITELLM_MASTER_KEY=' "$ENV_FILE" 2>/dev/null | cut -d= -f2-)
fi
if [ -n "$EXISTING_MASTER" ]; then
    MASTER_KEY="$EXISTING_MASTER"
    echo "  preserved existing LiteLLM master key"
else
    MASTER_KEY="sk-litellm-$(openssl rand -hex 16)"
    echo "  generated new LiteLLM master key"
fi

sudo tee "$ENV_FILE" > /dev/null <<EOF
OPENROUTER_API_KEY=${OPENROUTER_KEY}
LITELLM_MASTER_KEY=${MASTER_KEY}
EOF
sudo chmod 600 "$ENV_FILE"
echo "  wrote $ENV_FILE"

# Start the stack
echo ""
echo "=== Starting services ==="
cd "$DEPLOY_DIR"
sudo docker compose up -d
echo ""
sudo docker compose ps

# Pull model if requested
if [ "$PULL_MODEL" = "true" ]; then
    echo ""
    echo "=== Pulling qwen3-coder on both Ollama instances ==="
    echo "  ollama-a..."
    sudo docker exec ollama-a ollama pull qwen3-coder
    echo "  ollama-b..."
    sudo docker exec ollama-b ollama pull qwen3-coder
    echo "  model pull complete"
fi

echo ""
echo "Done. LiteLLM proxy available at http://localhost:11430"
echo "Health: curl http://localhost:11430/health"
