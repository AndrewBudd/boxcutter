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
# Ensure mise shims are in PATH for non-interactive SSH commands
# (.bashrc has a guard that exits early for non-interactive shells,
#  and .profile is only sourced for login shells)
su - dev -c 'sed -i "2i export PATH=\"\$HOME/.local/bin:\$HOME/.local/share/mise/shims:\$PATH\"" ~/.bashrc' || true
su - dev -c '~/.local/bin/mise use -g node@22' || true
su - dev -c '~/.local/bin/mise use -g ruby@3.2' || true

# Claude Code (native installer, no node dependency)
su - dev -c 'curl -fsSL https://claude.ai/install.sh | bash' || true

# Services declaration file
cat > /home/dev/.services << 'EOF'
# Declare services as name=port, one per line
# Auto-discovered by boxcutter-proxy-sync
# Accessible at http://<vm-ip>:<port>
# example=3000
EOF
chown dev:dev /home/dev/.services

# gh-token-refresh: auto-refresh GitHub token from vmid metadata service
cat > /usr/local/bin/gh-token-refresh << 'SCRIPT'
#!/bin/bash
set -euo pipefail
METADATA="http://169.254.169.254"
resp=$(curl -sf "$METADATA/token/github" 2>/dev/null) || {
    echo "vmid: GitHub token not available (service may not be configured)" >&2
    exit 0
}
token=$(echo "$resp" | jq -r '.token // empty')
[ -z "$token" ] && { echo "vmid: no token in response" >&2; exit 1; }
mkdir -p "$HOME/.config/gh"
cat > "$HOME/.config/gh/hosts.yml" <<EOF
github.com:
    oauth_token: $token
    user: x-access-token
    git_protocol: https
EOF
# Also configure git credential for HTTPS clones/pushes
git config --global credential.helper '!f() { echo "username=x-access-token"; echo "password='$token'"; }; f'
echo "vmid: GitHub token refreshed (expires: $(echo "$resp" | jq -r '.expires_at // "unknown"'))"
SCRIPT
chmod +x /usr/local/bin/gh-token-refresh

# Systemd timer to refresh GitHub token every 30 minutes
cat > /etc/systemd/system/gh-token-refresh.service << 'EOF'
[Unit]
Description=Refresh GitHub token from vmid
[Service]
Type=oneshot
User=dev
ExecStart=/usr/local/bin/gh-token-refresh
Environment=HOME=/home/dev
EOF

cat > /etc/systemd/system/gh-token-refresh.timer << 'EOF'
[Unit]
Description=Refresh GitHub token every 30 minutes
[Timer]
OnBootSec=10s
OnUnitActiveSec=30min
AccuracySec=1min
[Install]
WantedBy=timers.target
EOF

systemctl enable gh-token-refresh.timer 2>/dev/null || true

# Enable SSH
systemctl enable ssh 2>/dev/null || true

# Clean up
apt-get clean
rm -rf /var/lib/apt/lists/*

echo "Provision complete."
