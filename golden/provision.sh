#!/bin/bash
# Provision script — runs inside chroot to install dev tools into the golden image.
set -e

export DEBIAN_FRONTEND=noninteractive

# Add universe repo for wider package availability
cat > /etc/apt/sources.list << 'EOF'
deb http://archive.ubuntu.com/ubuntu noble main universe
deb http://archive.ubuntu.com/ubuntu noble-updates main universe
deb http://archive.ubuntu.com/ubuntu noble-security main universe
EOF

apt-get update
apt-get install -y \
  build-essential git curl wget jq tmux htop \
  libssl-dev libreadline-dev zlib1g-dev libyaml-dev libffi-dev \
  unzip tar sudo openssh-server

# GitHub CLI
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null
echo "deb [arch=$(dpkg --print-architecture) \
  signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] \
  https://cli.github.com/packages stable main" \
  | tee /etc/apt/sources.list.d/github-cli.list >/dev/null
apt-get update -qq
apt-get install -y gh

# Create dev user (UID 1000) with passwordless sudo
useradd -m -s /bin/bash -G sudo -u 1000 dev 2>/dev/null || true
echo "dev ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/dev
chmod 440 /etc/sudoers.d/dev
echo "dev:dev" | chpasswd

# mise (polyglot version manager)
su - dev -c 'curl -fsSL https://mise.run | sh' || true
su - dev -c 'echo "eval \"\$(~/.local/bin/mise activate bash)\"" >> ~/.bashrc' || true
su - dev -c 'echo "export PATH=\"\$HOME/.local/bin:\$HOME/.local/share/mise/shims:\$PATH\"" >> ~/.profile' || true
su - dev -c '~/.local/bin/mise use -g node@22' || true
su - dev -c '~/.local/bin/mise use -g ruby@3.2' || true

# Claude Code (requires node from mise)
su - dev -c 'export PATH="$HOME/.local/share/mise/shims:$PATH" && npm install -g @anthropic-ai/claude-code' || true

# Services declaration file
cat > /home/dev/.services << 'EOF'
# Declare services as name=port, one per line
# Auto-discovered by boxcutter-proxy-sync
# Exposed as https://<name>.<vm-name>.vm.lan
# example=3000
EOF
chown dev:dev /home/dev/.services

# Enable SSH
systemctl enable ssh 2>/dev/null || true

# Clean up
apt-get clean
rm -rf /var/lib/apt/lists/*

echo "Provision complete."
