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
    curl wget jq git \
    iproute2 iputils-ping \
    ca-certificates locales \
    gcc libc6-dev \
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

# --- fstab ---
RUN echo "/dev/vda / ext4 defaults 0 1" > /etc/fstab

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
