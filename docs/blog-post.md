# I built a "spin up a VM in 0.25 seconds" system with Firecracker and some shell scripts

I wanted something simple: SSH into a machine, type one command, and get a fresh dev environment. Not a container — a real VM with its own kernel, its own IP on my LAN, that I can SSH into like any other machine. And I wanted it to be fast. Like, blink-and-it's-done fast.

Here's what it looks like:

```
$ ssh 192.168.2.100 new

VM ready: bold-fox
Connect: ssh 192.168.2.200
```

That's it. A new VM is running, on my LAN, with a real IP, in about a second. I can `ssh bold-fox.local` and I'm in. No username required — it figures out who I am from my SSH key.

The whole thing is about 800 lines of bash, 60 lines of C, and zero external services. I call it Boxcutter.

## The stack

Boxcutter is three layers deep:

**Physical host** — a desktop machine under my desk running Ubuntu 24.04. 16 cores, 58GB RAM. It runs a single QEMU virtual machine.

**Node VM** — that QEMU VM. It runs Ubuntu with Firecracker installed, and manages all the microVMs. It has 12 vCPUs and 48GB of RAM allocated to it.

**Firecracker microVMs** — the actual dev environments. Each one is a lightweight VM with 4 vCPUs, 8GB RAM, and a full Ubuntu userland with Node, Ruby, git, and all the usual dev tools.

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
ip=192.168.2.200::192.168.2.1:255.255.255.0:bold-fox:eth0:off:8.8.8.8
```

That one kernel argument gives the VM its IP address, gateway, netmask, hostname, interface, and DNS server. The network is up by the time systemd starts. No DHCP server, no waiting, no network manager.

### Instant boot: Firecracker

Firecracker boots a Linux kernel in about 125ms. It's not a container runtime — it's a virtual machine monitor that creates real VMs with hardware-level isolation. But it's stripped down to the essentials: no BIOS, no PCI bus emulation, no USB, no graphics. Just CPU, memory, disk, and network.

The combination of device-mapper snapshots + kernel `ip=` + Firecracker's fast boot means a VM goes from "nothing" to "accepting SSH connections" in about one second.

## Networking: real LAN IPs

Early on I tried a NAT approach — give VMs private IPs and forward ports through the Node VM. It worked but created problems: you needed a bastion hop to reach VMs, SSH sessions lost ctrl+c, and every service needed explicit port mapping.

The solution was simpler: bridge everything to the LAN.

```
Physical NIC (enp34s0)  ─┬──  br0 (host bridge)
                          └──  tap-node0 → Node VM
                                    │
Node VM NIC (ens3)      ─┬──  brvm0 (VM bridge)
                          ├──  tap-bold-fox → 192.168.2.200
                          └──  tap-calm-otter → 192.168.2.201
```

Each Firecracker VM gets a TAP device on the Node VM's bridge, which connects to the host's bridge, which connects to the physical NIC. VMs get real LAN IPs from a reserved pool (192.168.2.200-250). They're directly reachable from any machine on the network.

## The "accept any SSH username" trick

I didn't want users to remember usernames. If you have an authorized SSH key, you should be able to just `ssh 192.168.2.200` — whatever username your client sends should work.

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

## mDNS hostnames

Each VM runs Avahi (the mDNS daemon). A systemd oneshot service parses the hostname from the kernel `ip=` parameter and sets it at boot:

```bash
H=$(cat /proc/cmdline | grep -oP "ip=[^:]*::[^:]*:[^:]*:\K[^:]+")
[ -n "$H" ] && hostname "$H"
```

This means `bold-fox` announces itself as `bold-fox.local` on the network. If your LAN router passes multicast traffic, you can `ssh bold-fox.local` from any machine.

## The golden image

The base rootfs is built with `debootstrap` — the same tool Debian/Ubuntu use to bootstrap a root filesystem. Phase 1 creates a minimal system with SSH, systemd, Avahi, and the NSS catchall module. Phase 2 boots it as a temporary Firecracker VM and installs dev tools via SSH.

The image includes some Firecracker-specific tweaks:

- **Entropy seeding:** Firecracker VMs lack a hardware RNG. `systemd-random-seed` blocks boot waiting for entropy. I mask it and seed `/dev/urandom` from a oneshot service instead.
- **No network manager:** systemd-networkd's DHCP client fights with the kernel `ip=` parameter. I just don't install it. Kernel networking is all you need.
- **Static DNS:** A simple `/etc/resolv.conf` pointing at 8.8.8.8. No `systemd-resolved`, no extra daemons.

## The control interface

The Node VM has a `boxcutter` SSH user with `ForceCommand` — it can't get a shell, it can only run the dispatch script. The dispatch script translates SSH commands to VM lifecycle operations:

```bash
ssh 192.168.2.100 new          # Create and start a VM
ssh 192.168.2.100 list         # List all VMs
ssh 192.168.2.100 stop fox     # Stop a VM
ssh 192.168.2.100 destroy fox  # Destroy a VM
ssh 192.168.2.100 adduser gh   # Import SSH keys from GitHub
```

The same "accept any username" NSS trick runs on the Node VM too, so you don't need to specify a user — just `ssh 192.168.2.100`.

## Direct service access

Since VMs have real LAN IPs, any service running in a VM is directly reachable. If `bold-fox` runs a web server on port 3000, hit it at `http://192.168.2.200:3000` from any machine on the network. No port forwarding, no reverse proxy configuration. The VM is a first-class network citizen.

## What I learned

**Kernel parameters are underrated.** The `ip=` boot parameter eliminates an entire class of boot-time complexity. No DHCP server, no network manager, no waiting. The kernel just sets up the interface before init runs.

**Device-mapper is incredibly useful.** COW snapshots turn a 30-second file copy into a 0.25-second metadata operation. Docker uses the same mechanism under the hood, but you can use it directly with `dmsetup` for any block device.

**NSS is the right layer for "fake users."** I tried several approaches before landing on a custom NSS module — PAM modules, nss-extrausers, on-the-fly user creation. The problem is always ordering: sshd checks user existence before anything else. NSS is the layer that answers "does this user exist?" and it's the only place where you can intercept that question early enough.

**Bridge networking beats NAT for dev environments.** NAT adds complexity at every layer — port forwarding, bastion hops, broken ctrl+c in nested SSH sessions. Bridging VMs to the LAN makes them first-class network citizens. The setup is slightly more complex upfront, but everything downstream gets simpler.

**Firecracker's constraints are features.** No BIOS, no PCI, no USB means fewer things to configure and fewer things to break. The kernel boots in 125ms because there's nothing to probe. The tradeoff — no GPU, no USB passthrough — is fine for dev environments.

## The numbers

- **VM creation:** ~0.25 seconds (COW snapshot + Firecracker config generation)
- **VM boot to SSH-ready:** ~1 second
- **Golden image build:** ~5 minutes (debootstrap + provision)
- **Per-VM overhead:** ~30MB RSS for the Firecracker process
- **Disk per VM:** Starts at ~0 bytes, grows with writes
- **Total codebase:** ~800 lines of bash, ~60 lines of C, no dependencies beyond standard Linux tools

The whole thing runs on a single machine under my desk. No cloud, no Kubernetes, no container registry, no orchestrator. Just Linux, doing what Linux does well.

---

*Boxcutter is open source and runs entirely on standard Linux tools: QEMU, KVM, Firecracker, device-mapper, bridge-utils, Avahi, and bash.*
