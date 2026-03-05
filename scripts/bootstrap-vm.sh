#!/bin/bash
set -e

apt-get update && apt-get install -y \
  build-essential git curl wget jq tmux \
  libssl-dev libreadline-dev zlib1g-dev \
  postgresql-16 redis-server

systemctl enable postgresql redis-server

# GitHub CLI
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) \
  signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] \
  https://cli.github.com/packages stable main" \
  | tee /etc/apt/sources.list.d/github-cli.list
apt-get update && apt-get install -y gh

# Create dev user with passwordless sudo
useradd -m -s /bin/bash -G sudo dev || true
echo "dev ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/dev
chmod 440 /etc/sudoers.d/dev

# mise (polyglot version manager)
su - dev -c 'curl https://mise.run | sh'
su - dev -c 'echo "eval \"\$(~/.local/bin/mise activate bash)\"" >> ~/.bashrc'
su - dev -c '~/.local/bin/mise use -g ruby@3.2'
su - dev -c '~/.local/bin/mise use -g node@22'
su - dev -c '~/.local/bin/mise use -g node@20'

# Claude Code
su - dev -c 'source ~/.bashrc && npm install -g @anthropic-ai/claude-code'

# Default .services declaration file
cat > /home/dev/.services << 'EOF'
# Declare services as name=port, one per line
# These are auto-discovered by boxcutter-proxy-sync
# and exposed as https://<name>.<vm-name>.vm.lan
# example=3000
EOF
chown dev:dev /home/dev/.services

echo "Bootstrap complete."
