# I built a "spin up a VM in 1 second" system with Firecracker, Tailscale, and some shell scripts

I wanted something simple: type one command, and get a fresh dev environment. Not a container — a real VM with its own kernel, accessible from anywhere on my Tailscale network. And I wanted it to be fast. Like, blink-and-it's-done fast.

Here's what it looks like:

```
$ ssh boxcutter new

VM ready: bold-fox
Connect: ssh 100.64.1.42
```

That's it. A new VM is running, on my Tailscale network, reachable from any of my devices, in about a second (plus a few more for Tailscale to connect). I can `ssh bold-fox` and I'm in. No username required — it figures out who I am from my SSH key.

The whole thing is about 800 lines of bash, 60 lines of C, a couple of small Go services, and zero external services beyond Tailscale. I call it Boxcutter.

## The stack

Boxcutter is three layers deep:

**Physical host** — a desktop machine under my desk running Ubuntu 24.04. 16 cores, 58GB RAM. It runs a single QEMU virtual machine.

**Node VM** — that QEMU VM. It runs Ubuntu with Firecracker installed, and manages all the microVMs. It has 12 vCPUs and 48GB of RAM allocated to it. It's also a Tailscale node.

**Firecracker microVMs** — the actual dev environments. Each one is a lightweight VM with 4 vCPUs, 8GB RAM, and a full Ubuntu userland with Node, Ruby, git, and all the usual dev tools. Each one joins Tailscale automatically at boot.

Why the middle layer? Firecracker needs KVM and specific kernel features. I could run it directly on the host, but the QEMU VM gives me clean isolation — all the Firecracker state, networking, and disk images live inside a single VM that I can snapshot, migrate, or nuke without touching the host.

## The tricks that make it fast

### Instant disk: device-mapper snapshots

The golden rootfs image is about 3.7GB. Copying it for every new VM would take 30+ seconds. Instead, I use Linux device-mapper snapshots — the same COW mechanism that powers Docker's storage.

```bash
# Create a sparse COW file (instant — just metadata)
truncate -s 50G cow.img

# Set up the snapshot device
echo "0 ${sectors} snapshot ${base_loop} ${cow_loop} P 8" | dmsetup create bc-bold-fox
```

The COW file starts at effectively zero bytes and only grows as the VM writes to disk. Creating a VM takes ~0.25 seconds.

### Instant network: kernel ip= boot parameter

Most VMs wait for DHCP. Firecracker VMs don't need to. The kernel `ip=` parameter sets up networking before init even starts:

```
ip=10.0.0.2::10.0.0.1:255.255.255.252:bold-fox:eth0:off:8.8.8.8
```

That one kernel argument gives the VM its IP address, gateway, netmask, hostname, interface, and DNS server. The network is up by the time systemd starts. No DHCP server, no waiting, no network manager.

### Instant boot: Firecracker

Firecracker boots a Linux kernel in about 125ms. It's not a container runtime — it's a virtual machine monitor that creates real VMs with hardware-level isolation. But it's stripped down to the essentials: no BIOS, no PCI bus emulation, no USB, no graphics. Just CPU, memory, disk, and network.

The combination of device-mapper snapshots + kernel `ip=` + Firecracker's fast boot means a VM goes from "nothing" to "accepting SSH connections" in about one second.

## Networking: same IP, different TAPs

Every VM gets the same IP address: `10.0.0.2`. This sounds insane, but it works because each VM gets its own isolated TAP device with a point-to-point link. There is no shared bridge.

```
Node VM
  ├── tap-bold-fox   10.0.0.1 ↔ 10.0.0.2  (mark: 41022)
  ├── tap-calm-otter 10.0.0.1 ↔ 10.0.0.2  (mark: 8193)
  └── tap-wild-heron 10.0.0.1 ↔ 10.0.0.2  (mark: 55471)
```

The trick is **fwmark-based policy routing**. Each VM gets a unique integer "mark" derived from CRC32 of its name. When packets arrive from a VM's TAP, iptables marks them. The mark is saved to Linux's connection tracking (conntrack), so when return traffic comes back from the internet, the kernel restores the mark and routes the packet back to the correct TAP.

```bash
# Mark packets from this VM's TAP
iptables -t mangle -I PREROUTING 2 -i tap-bold-fox -j MARK --set-mark 41022

# Route marked return traffic back to the correct TAP
ip rule add fwmark 41022 lookup 41022
ip route add 10.0.0.2 dev tap-bold-fox table 41022
```

This eliminates all IP pool management — no bridge, no DHCP, no IP allocation, no MAC conflicts. Each VM is completely isolated at Layer 2.

### Tailscale overlay

VMs have internal-only IPs that are not routable from outside the Node VM. Instead, each VM joins Tailscale at boot, getting a stable Tailscale IP that's reachable from any device on your tailnet.

The physical host creates a TAP device with a point-to-point link (192.168.50.0/30) to the Node VM. Inside the Node VM, each Firecracker microVM gets its own TAP. NAT provides internet access for the VMs.

### VM isolation

VMs cannot communicate with each other — period. There is no shared Layer 2 domain. Each VM only sees a point-to-point link to 10.0.0.1 (the Node VM). There are no routes between TAPs. If two VMs need to communicate, they do it through Tailscale, which means it's subject to your tailnet's ACL policies.

### SSH to same-IP VMs

Since all VMs are 10.0.0.2, the Node VM can't simply `ssh 10.0.0.2`. Instead, SSH is bound to a specific TAP device using socat's `SO_BINDTODEVICE`:

```bash
ssh -o "ProxyCommand=socat - TCP:10.0.0.2:22,so-bindtodevice=tap-bold-fox" dev@10.0.0.2
```

This forces the connection onto the correct TAP, reaching the intended VM.

## VM identity via fwmark

With every VM at 10.0.0.2, the identity service (vmid) can't use source IP to identify callers. Instead, it reads the **fwmark** from each accepted TCP connection using `getsockopt(SO_MARK)`.

When a VM connects to vmid at 10.0.0.1:80, the packet arrives on its TAP and gets marked by iptables. The Linux sysctl `net.ipv4.tcp_fwmark_accept=1` causes accepted TCP sockets to inherit the packet's fwmark. vmid reads this mark and looks up the VM record.

This is a neat trick — the kernel's packet marking system becomes a VM identification mechanism, without any changes to the VM itself. The VM just connects to its gateway IP and gets identified automatically.

## Normal vs paranoid mode

VMs come in two flavors:

**Normal mode** — full internet access, real credentials. The simple case.

**Paranoid mode** — no direct internet. All outbound traffic must go through a MITM forward proxy. Credentials are wrapped in one-time sentinel tokens that never reach the VM in their real form.

The proxy intercepts HTTPS (using an internal CA cert injected into each VM's trust store), scans Authorization headers for sentinel tokens, swaps them for real credentials on the fly, and forwards the request. The real credential exists only in the proxy's memory for the duration of the request.

```
Paranoid VM                    Proxy                       GitHub
    │ Authorization: Bearer     │                              │
    │   <sentinel>              │                              │
    │──────────────────────────→│  swap sentinel → real token  │
    │                           │  Authorization: Bearer       │
    │                           │    ghp_real...               │
    │                           │─────────────────────────────→│
```

## The "accept any SSH username" trick

I didn't want users to remember usernames. If you have an authorized SSH key, you should be able to just `ssh 100.64.1.42` — whatever username your client sends should work.

This is harder than it sounds. OpenSSH checks if a user exists *before* it runs `AuthorizedKeysCommand`. If you SSH as `budda` and there's no `budda` user, sshd rejects you before it even looks at your keys.

The fix is a custom NSS (Name Service Switch) module — 60 lines of C that makes Linux think every username exists:

```c
enum nss_status _nss_catchall_getpwnam_r(const char *name, struct passwd *result,
                                          char *buffer, size_t buflen, int *errnop) {
    if (is_system_user(name)) return NSS_STATUS_NOTFOUND;
    // Map any unknown user to dev (uid 1000)
    result->pw_uid = 1000;
    result->pw_gid = 1000;
    result->pw_dir = "/home/dev";
    result->pw_shell = "/bin/bash";
    return NSS_STATUS_SUCCESS;
}
```

When sshd looks up user "budda", the NSS module says "yes, that user exists, uid 1000, home directory /home/dev." Then `AuthorizedKeysCommand` returns the shared authorized_keys file, the key matches, and you're in. Everyone lands in the same `dev` account — isolation happens at the VM level, not the Unix user level.

I also had to implement `_nss_catchall_getspnam_r` for the shadow database, because PAM's `unix_chkpwd` queries shadow entries during authentication. Without it, sshd would accept the key but then PAM would reject the session.

## Tailscale MagicDNS hostnames

Each VM joins Tailscale with its generated name as the hostname. If you have MagicDNS enabled on your tailnet, you can `ssh bold-fox` from any device. No mDNS, no Avahi, no multicast — just Tailscale's DNS.

## The golden image

The base rootfs is built with `debootstrap` — the same tool Debian/Ubuntu use to bootstrap a root filesystem. Phase 1 creates a minimal system with SSH, systemd, Tailscale, and the NSS catchall module. Phase 2 boots it as a temporary Firecracker VM and installs dev tools via SSH.

The image includes some Firecracker-specific tweaks:

- **Entropy seeding:** Firecracker VMs lack a hardware RNG. `systemd-random-seed` blocks boot waiting for entropy. I mask it and seed `/dev/urandom` from a oneshot service instead.
- **No network manager:** systemd-networkd's DHCP client fights with the kernel `ip=` parameter. I just don't install it. Kernel networking is all you need.
- **Static DNS:** A simple `/etc/resolv.conf` pointing at 8.8.8.8. No `systemd-resolved`, no extra daemons.
- **Tailscale auto-join:** After a VM boots, the Node VM SSHes in over the internal network and runs `tailscale up` with an ephemeral auth key. The key lives only on the Node VM — never on VM disk images. When a VM is destroyed and disconnects, Tailscale automatically removes it from the tailnet.

## The control interface

The Node VM has a `boxcutter` SSH user with `ForceCommand` — it can't get a shell, it can only run the dispatch script. The dispatch script translates SSH commands to VM lifecycle operations:

```bash
ssh boxcutter new          # Create and start a VM
ssh boxcutter list         # List all VMs (with marks, modes, Tailscale IPs)
ssh boxcutter stop fox     # Stop a VM
ssh boxcutter destroy fox  # Destroy a VM (removes from Tailscale)
ssh boxcutter adduser gh   # Import SSH keys from GitHub
```

The same "accept any username" NSS trick runs on the Node VM too, so you don't need to specify a user.

## Direct service access

Since VMs have Tailscale IPs, any service running in a VM is directly reachable from anywhere on the tailnet. If `bold-fox` runs a web server on port 3000, hit it at `http://100.64.1.42:3000` from any device. Or with MagicDNS: `http://bold-fox:3000`. No port forwarding, no reverse proxy configuration.

## What I learned

**Every VM can have the same IP.** By giving each VM its own TAP with point-to-point addressing and using fwmark policy routing, you eliminate IP pool management entirely. It sounds wrong, but it works — and it's simpler than managing a bridge with unique IPs.

**Kernel parameters are underrated.** The `ip=` boot parameter eliminates an entire class of boot-time complexity. No DHCP server, no network manager, no waiting. The kernel just sets up the interface before init runs.

**Device-mapper is incredibly useful.** COW snapshots turn a 30-second file copy into a 0.25-second metadata operation. Docker uses the same mechanism under the hood, but you can use it directly with `dmsetup` for any block device.

**`tcp_fwmark_accept` is obscure but essential.** Accepted TCP sockets don't inherit the incoming packet's fwmark by default. You need `sysctl net.ipv4.tcp_fwmark_accept=1` for `getsockopt(SO_MARK)` to return anything useful. I spent hours debugging why vmid couldn't identify VMs before discovering this sysctl.

**NSS is the right layer for "fake users."** I tried several approaches before landing on a custom NSS module — PAM modules, nss-extrausers, on-the-fly user creation. The problem is always ordering: sshd checks user existence before anything else. NSS is the layer that answers "does this user exist?" and it's the only place where you can intercept that question early enough.

**Tailscale beats LAN bridging for dev environments.** My first version bridged VMs directly onto the LAN with real IPs. It worked but required wired Ethernet (can't bridge WiFi), reserved IP blocks, and VMs were only reachable locally. Tailscale makes VMs accessible from anywhere, works over WiFi, and adds proper identity and ACLs. The tradeoff is a few extra seconds of startup time for the Tailscale handshake.

**Firecracker's constraints are features.** No BIOS, no PCI, no USB means fewer things to configure and fewer things to break. The kernel boots in 125ms because there's nothing to probe. The tradeoff — no GPU, no USB passthrough — is fine for dev environments.

## The numbers

- **VM creation:** ~0.25 seconds (COW snapshot + Firecracker config generation)
- **VM boot to SSH-ready:** ~1 second (internal network)
- **Tailscale connection:** ~3-5 seconds additional
- **Golden image build:** ~5 minutes (debootstrap + provision)
- **Per-VM overhead:** ~30MB RSS for the Firecracker process
- **Disk per VM:** Starts at ~0 bytes, grows with writes
- **Total codebase:** ~800 lines of bash, ~60 lines of C, ~500 lines of Go, no dependencies beyond standard Linux tools + Tailscale

The whole thing runs on a single machine under my desk. No cloud, no Kubernetes, no container registry, no orchestrator. Just Linux doing what Linux does well, with Tailscale making it accessible from everywhere.

---

*Boxcutter is open source and runs on standard Linux tools: QEMU, KVM, Firecracker, device-mapper, socat, Tailscale, and bash.*
