# Boxcutter golden image — minimal Firecracker microVM rootfs.
#
# This produces a bootable systemd environment with SSH, Tailscale, and
# the infrastructure needed for boxcutter VM management. No dev tools
# are included — users install what they need after VM creation.
#
# Build: docker build -t boxcutter-golden node/golden/
# Convert to ext4: node/golden/docker-to-ext4.sh
FROM ubuntu:noble

ENV DEBIAN_FRONTEND=noninteractive

# Core packages — minimal set for a bootable VM with SSH and networking
RUN apt-get update && apt-get install -y --no-install-recommends \
    systemd systemd-sysv dbus \
    openssh-server sudo \
    curl wget jq git tmux \
    iproute2 iputils-ping \
    ca-certificates locales \
    gcc libc6-dev \
    kmod \
    && rm -rf /var/lib/apt/lists/*

# --- Serial console ---
RUN mkdir -p /etc/systemd/system/getty.target.wants && \
    ln -sf /lib/systemd/system/serial-getty@.service \
      /etc/systemd/system/getty.target.wants/serial-getty@ttyS0.service

# --- SSH ---
RUN mkdir -p /etc/systemd/system/multi-user.target.wants && \
    ln -sf /lib/systemd/system/ssh.service \
      /etc/systemd/system/multi-user.target.wants/ssh.service

# --- Hostname (overridden at boot via kernel ip= parameter) ---
# Note: /etc/hostname and /etc/hosts are Docker bind mounts during build.
# We write to .boxcutter files and the conversion script replaces them.
RUN echo "boxcutter-vm" > /etc/hostname.boxcutter && \
    printf '127.0.0.1 localhost\n127.0.1.1 boxcutter-vm\n' > /etc/hosts.boxcutter

# --- fstab (root only — tools.img mounted by dedicated service) ---
RUN echo "/dev/vda / ext4 defaults 0 1" > /etc/fstab && \
    mkdir -p /opt/boxcutter/tools

# --- tools.img mount service (mounts squashfs before tapegun) ---
RUN printf '[Unit]\n\
Description=Mount boxcutter tools squashfs\n\
DefaultDependencies=no\n\
After=local-fs.target\n\
Before=boxcutter-tapegun.service\n\
ConditionPathExists=/dev/vdb\n\
\n\
[Service]\n\
Type=oneshot\n\
RemainAfterExit=yes\n\
ExecStart=/bin/mount -t squashfs -o ro /dev/vdb /opt/boxcutter/tools\n\
ExecStop=/bin/umount /opt/boxcutter/tools\n\
\n\
[Install]\n\
WantedBy=local-fs.target\n' > /etc/systemd/system/boxcutter-tools-mount.service && \
    mkdir -p /etc/systemd/system/local-fs.target.wants && \
    ln -sf /etc/systemd/system/boxcutter-tools-mount.service \
      /etc/systemd/system/local-fs.target.wants/boxcutter-tools-mount.service

# --- Auto-grow root filesystem to fill disk on first boot ---
COPY config/boxcutter-growfs.service /etc/systemd/system/boxcutter-growfs.service
RUN mkdir -p /etc/systemd/system/local-fs.target.wants && \
    ln -sf /etc/systemd/system/boxcutter-growfs.service \
      /etc/systemd/system/local-fs.target.wants/boxcutter-growfs.service

# --- Set hostname from kernel ip= parameter at boot ---
COPY config/set-hostname.service /etc/systemd/system/set-hostname.service
RUN ln -sf /etc/systemd/system/set-hostname.service \
      /etc/systemd/system/multi-user.target.wants/set-hostname.service

# --- Entropy: Firecracker lacks hardware RNG, mask slow random-seed ---
RUN systemctl mask systemd-random-seed.service
COPY config/seed-entropy.service /etc/systemd/system/seed-entropy.service
RUN mkdir -p /etc/systemd/system/sysinit.target.wants && \
    ln -sf /etc/systemd/system/seed-entropy.service \
      /etc/systemd/system/sysinit.target.wants/seed-entropy.service

# --- Static DNS (no systemd-resolved for fast boot) ---
# Note: /etc/resolv.conf is a Docker bind mount during build, can't rm.
# We write to a temp file and the docker-to-ext4.sh script replaces it
# after export. During build, Docker's own DNS works fine.
RUN printf 'nameserver 8.8.8.8\nnameserver 8.8.4.4\n' > /etc/resolv.conf.boxcutter

# --- dev user (uid 1000) with passwordless sudo ---
# Ubuntu noble base image has 'ubuntu' at UID 1000; remove it first
RUN userdel -r ubuntu 2>/dev/null || true && \
    useradd -m -s /bin/bash -G sudo -u 1000 dev && \
    echo "dev:dev" | chpasswd && \
    echo "dev ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/dev && \
    chmod 440 /etc/sudoers.d/dev && \
    echo "root:root" | chpasswd

# --- Empty SSH directory (keys injected at VM creation time, never baked) ---
RUN mkdir -p /home/dev/.ssh && \
    touch /home/dev/.ssh/authorized_keys && \
    chown -R 1000:1000 /home/dev/.ssh && \
    chmod 700 /home/dev/.ssh && \
    chmod 600 /home/dev/.ssh/authorized_keys

# --- NSS catchall: any SSH username maps to dev ---
COPY nss_catchall.c /tmp/nss_catchall.c
RUN gcc -shared -fPIC -o /usr/lib/x86_64-linux-gnu/libnss_catchall.so.2 /tmp/nss_catchall.c && \
    rm /tmp/nss_catchall.c && \
    sed -i 's/^passwd:.*/passwd:         files catchall/' /etc/nsswitch.conf && \
    sed -i 's/^shadow:.*/shadow:         files catchall/' /etc/nsswitch.conf

# --- vsock listener (for migration nudges) ---
COPY vsock_listen.c /tmp/vsock_listen.c
RUN gcc -o /usr/local/bin/boxcutter-vsock-listen /tmp/vsock_listen.c && \
    rm /tmp/vsock_listen.c

# --- Remove gcc (only needed for compilation above) ---
RUN apt-get remove -y gcc libc6-dev && \
    apt-get autoremove -y && \
    rm -rf /var/lib/apt/lists/*

# --- GitHub CLI ---
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list && \
    apt-get update -qq && apt-get install -y gh && \
    rm -rf /var/lib/apt/lists/*

# --- Dev tools: mise, Node.js, Ruby ---
# Ruby builds from source and requires build-essential + dev libraries.
# gcc was removed earlier (only needed for NSS/vsock compilation), so reinstall here.
RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends \
      build-essential libssl-dev libreadline-dev zlib1g-dev libyaml-dev libffi-dev && \
    rm -rf /var/lib/apt/lists/* && \
    su - dev -c 'curl -fsSL https://mise.run | sh' && \
    su - dev -c '~/.local/bin/mise use -g node@22' && \
    su - dev -c '~/.local/bin/mise use -g ruby@3.2' && \
    apt-get remove -y build-essential libssl-dev libreadline-dev zlib1g-dev libyaml-dev libffi-dev && \
    apt-get autoremove -y && \
    rm -rf /var/lib/apt/lists/*

# --- Claude Code (native installer) ---
RUN su - dev -c 'curl -fsSL https://claude.ai/install.sh | bash'

# --- OpenCode (open-source coding agent for local inference) ---
RUN su - dev -c 'npm install -g @anthropic-ai/opencode' || \
    su - dev -c 'curl -fsSL https://opencode.ai/install | bash' || \
    echo "WARNING: OpenCode install failed — will install at first boot"

# --- PATH: ensure ~/.local/bin and mise shims are available for non-interactive SSH ---
# .bashrc has a guard that exits early for non-interactive shells. The PATH export
# must be inserted BEFORE this guard (line 2) so non-interactive SSH commands like
# "ssh vm 'gh pr list'" can find tools. mise activate goes at the end (interactive only).
RUN sed -i '2i export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"' \
      /home/dev/.bashrc && \
    printf 'eval "$($HOME/.local/bin/mise activate bash)"\n' \
      >> /home/dev/.bashrc && \
    printf 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"\n' \
      >> /home/dev/.profile && \
    chown 1000:1000 /home/dev/.bashrc /home/dev/.profile

# --- SSH: accept any username's keys via AuthorizedKeysCommand ---
COPY config/auth-keys-any /usr/local/bin/auth-keys-any
COPY config/boxcutter-sshd.conf /etc/ssh/sshd_config.d/boxcutter.conf
RUN chmod 755 /usr/local/bin/auth-keys-any

# --- Tailscale (userspace networking for Firecracker) ---
RUN curl -fsSL https://tailscale.com/install.sh | sh 2>&1 | tail -1
COPY config/tailscaled-defaults /etc/default/tailscaled
RUN mkdir -p /etc/systemd/system/tailscaled.service.d
COPY config/tailscaled-firecracker.conf /etc/systemd/system/tailscaled.service.d/firecracker.conf
RUN systemctl enable tailscaled 2>/dev/null || true

# --- vsock service + nudge script ---
COPY config/boxcutter-nudge /usr/local/bin/boxcutter-nudge
COPY config/boxcutter-vsock.service /etc/systemd/system/boxcutter-vsock.service
RUN chmod 755 /usr/local/bin/boxcutter-nudge && \
    systemctl enable boxcutter-vsock 2>/dev/null || true

# --- Metadata service (fetch SSH keys + CA cert from node on boot) ---
COPY config/boxcutter-fetch-metadata /usr/local/bin/boxcutter-fetch-metadata
COPY config/boxcutter-metadata.service /etc/systemd/system/boxcutter-metadata.service
RUN chmod 755 /usr/local/bin/boxcutter-fetch-metadata && \
    ln -sf /etc/systemd/system/boxcutter-metadata.service \
      /etc/systemd/system/multi-user.target.wants/boxcutter-metadata.service

# --- Boxcutter tools (symlinks to tools.img, mounted by boxcutter-tools-mount.service) ---
RUN mkdir -p /opt/boxcutter/tools && \
    ln -sf /opt/boxcutter/tools/bin/boxcutter /usr/local/bin/boxcutter && \
    ln -sf /opt/boxcutter/tools/bin/boxcutter-tapegun /usr/local/bin/boxcutter-tapegun && \
    printf '[Unit]\n\
Description=Tapegun agent communication daemon\n\
After=network-online.target boxcutter-tools-mount.service\n\
Wants=network-online.target\n\
Requires=boxcutter-tools-mount.service\n\
\n\
[Service]\n\
Type=simple\n\
User=dev\n\
Group=dev\n\
ExecStart=/opt/boxcutter/tools/bin/boxcutter-tapegun\n\
Restart=always\n\
RestartSec=5\n\
\n\
[Install]\n\
WantedBy=multi-user.target\n' > /etc/systemd/system/boxcutter-tapegun.service && \
    ln -sf /etc/systemd/system/boxcutter-tapegun.service \
      /etc/systemd/system/multi-user.target.wants/boxcutter-tapegun.service

# --- Claude Code plugin directory (tapegun-guest plugin injected by docker-to-ext4.sh) ---
RUN mkdir -p /home/dev/.claude/plugins && chown -R 1000:1000 /home/dev/.claude

# --- GitHub token refresh (infrastructure — lets VMs get GitHub tokens from vmid) ---
COPY gh-token-refresh.sh /usr/local/bin/gh-token-refresh
COPY gh-token-refresh.service /etc/systemd/system/gh-token-refresh.service
COPY gh-token-refresh.timer /etc/systemd/system/gh-token-refresh.timer
RUN chmod +x /usr/local/bin/gh-token-refresh && \
    systemctl enable gh-token-refresh.timer 2>/dev/null || true

# --- Services declaration file ---
RUN printf '# Declare services as name=port, one per line\n# example=3000\n' > /home/dev/.services && \
    chown 1000:1000 /home/dev/.services

# --- Clean up ---
RUN apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
