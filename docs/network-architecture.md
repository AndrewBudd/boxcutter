# Boxcutter Network Architecture

This document describes the complete networking stack for Boxcutter: how traffic flows from the physical host down to individual Firecracker microVMs, how VMs are identified and isolated, and how the forward proxy and credential system work.

## Overview

Boxcutter runs Firecracker microVMs inside a QEMU/KVM Node VM on a physical Linux host. The networking has three layers, each with its own subnet and purpose:

```
┌──────────────────────────────────────────────────────────────────┐
│  Physical Host (Ubuntu 24.04)                                    │
│                                                                  │
│  enp34s0 ──→ internet (NAT)                                      │
│  tap-node0 (192.168.50.1/30) ──→ Node VM                        │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │  Node VM (QEMU/KVM) — 192.168.50.2                        │  │
│  │  Tailscale: 100.x.x.x (hostname: boxcutter)               │  │
│  │                                                            │  │
│  │  Services:                                                 │  │
│  │    vmid          :80   (VM identity & token broker)        │  │
│  │    boxcutter-proxy :8080 (MITM forward proxy)              │  │
│  │    derper         :443  (Tailscale DERP relay)              │  │
│  │    Caddy          :8880/:8443                               │  │
│  │                                                            │  │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐     │  │
│  │  │ tap-bold-fox │  │ tap-calm-ott │  │ tap-wild-her │     │  │
│  │  │ 10.0.0.1 ←──→  │ 10.0.0.1 ←──→  │ 10.0.0.1 ←──→     │  │
│  │  │  peer .2     │  │  peer .2     │  │  peer .2     │     │  │
│  │  │ mark: 41022  │  │ mark: 8193   │  │ mark: 55471  │     │  │
│  │  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘     │  │
│  │         │                 │                 │              │  │
│  │  ┌──────┴───────┐  ┌──────┴───────┐  ┌──────┴───────┐     │  │
│  │  │  bold-fox    │  │  calm-otter  │  │  wild-heron  │     │  │
│  │  │  Firecracker │  │  Firecracker │  │  Firecracker │     │  │
│  │  │  10.0.0.2    │  │  10.0.0.2    │  │  10.0.0.2    │     │  │
│  │  │  gw 10.0.0.1 │  │  gw 10.0.0.1 │  │  gw 10.0.0.1 │     │  │
│  │  │  TS:100.x.x  │  │  TS:100.x.x  │  │  TS:100.x.x  │     │  │
│  │  └──────────────┘  └──────────────┘  └──────────────┘     │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

**Key insight: every VM has the same IP address (10.0.0.2).** There is no shared bridge, no IP pool, no DHCP. Each VM gets its own isolated TAP device with a point-to-point /30 link. Linux fwmark-based policy routing directs return traffic to the correct TAP.

## Layer 1: Physical Host to Node VM

```
Physical Host                    Node VM
192.168.50.1/30  ←── tap-node0 ──→  192.168.50.2/30
```

The physical host creates a TAP device (`tap-node0`) and assigns it a point-to-point address on the 192.168.50.0/30 subnet. The Node VM (QEMU) gets 192.168.50.2. NAT masquerade on the host's physical NIC gives the Node VM internet access.

The host also adds a route for VM traffic: `ip route replace 10.0.0.2/32 via 192.168.50.2` — since all VMs share 10.0.0.2, this single route covers all of them.

**Files:**
- `host/boxcutter.env` — subnet and interface configuration
- `host/setup.sh` — creates TAP, NAT rules, and VM route

## Layer 2: Node VM to Firecracker VMs (per-TAP fwmark routing)

This is the core of the networking design. Each VM gets its own TAP with identical addressing:

```
Node side:  10.0.0.1 peer 10.0.0.2  on tap-<name>
VM side:    10.0.0.2 gateway 10.0.0.1   (same for every VM)
```

Since every VM has the same IP, the Node VM uses **Linux fwmark policy routing** to distinguish them. Each VM is assigned a unique integer "mark" at creation time.

### Mark allocation

Marks are derived from the VM name using CRC32:

```bash
mark=$(printf '%s' "$name" | cksum | awk '{print $1 % 65535 + 1}')
```

Range is 1–65535 (avoids 0, fits 16-bit). If a collision occurs with an existing VM, the name is rehashed with a numeric suffix (`name_1`, `name_2`, ...) until a unique mark is found. Marks are stored in each VM's `vm.json`.

### Per-TAP setup (on VM start)

When a VM starts, `setup_vm_tap()` runs:

```bash
# 1. Create the TAP with point-to-point addressing
ip tuntap add dev tap-bold-fox mode tap
ip addr add 10.0.0.1 peer 10.0.0.2 dev tap-bold-fox
ip link set tap-bold-fox up

# 2. Mark all packets arriving from this TAP
iptables -t mangle -I PREROUTING 2 -i tap-bold-fox -j MARK --set-mark 41022

# 3. Policy route: return traffic with this mark goes back to this TAP
ip rule add fwmark 41022 lookup 41022 priority 21022
ip route add 10.0.0.2 dev tap-bold-fox table 41022
ip route add default via <uplink-gw> dev <uplink> table 41022

# 4. Allow forwarding from this TAP
iptables -I FORWARD -i tap-bold-fox -j ACCEPT
```

### Per-TAP teardown (on VM stop)

When a VM stops, `teardown_vm_tap()` reverses everything:

```bash
iptables -t mangle -D PREROUTING -i tap-bold-fox -j MARK --set-mark 41022
iptables -D FORWARD -i tap-bold-fox -j ACCEPT
ip rule del fwmark 41022 lookup 41022
ip route flush table 41022
ip link del tap-bold-fox
```

### One-time infrastructure (boxcutter-net, runs at boot)

Before any VMs start, `boxcutter-net` sets up shared rules:

```bash
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.ipv4.tcp_fwmark_accept=1        # see "vmid and fwmark" below

iptables -t mangle -A PREROUTING -j CONNMARK --restore-mark   # restore marks from conntrack
iptables -t mangle -A POSTROUTING -m mark ! --mark 0 -j CONNMARK --save-mark  # save to conntrack
iptables -t nat -A POSTROUTING -s 10.0.0.2/32 -o <uplink> -j MASQUERADE       # NAT all VMs
iptables -A FORWARD -i <uplink> -m state --state RELATED,ESTABLISHED -j ACCEPT
```

### How return traffic finds the right TAP

This is the critical piece. When a VM sends a packet to the internet:

1. **Outbound:** Packet arrives on `tap-bold-fox` → mangle PREROUTING marks it with 41022 → CONNMARK saves 41022 to the conntrack entry → packet is NATed and forwarded to the internet
2. **Return:** Reply packet arrives on the uplink → mangle PREROUTING restores mark 41022 from conntrack → ip rule matches fwmark 41022 → routes via table 41022 → delivered to `tap-bold-fox`

Without this, all return traffic would go to whichever TAP has a route for 10.0.0.2 in the main table — only the first VM would work.

### ip rule priority

The priority formula is `10000 + (mark % 20000)`, giving a range of 10000–29999. This must be below 32766 (the main routing table's priority) so that fwmark-specific rules are evaluated first.

### VM isolation

VMs are completely isolated from each other:
- No shared L2 domain (no bridge) — each VM has its own TAP
- Each VM only sees its point-to-point link to 10.0.0.1
- There is no route between TAPs; a packet from one VM cannot reach another

### MAC address

All VMs use the same fixed MAC (`AA:FC:00:00:00:01`). This is safe because there is no shared Layer 2 — each TAP is an independent point-to-point link.

### VM network configuration

VMs get their network config via the kernel `ip=` boot parameter, which configures networking before init even starts:

```
ip=10.0.0.2::10.0.0.1:255.255.255.252:bold-fox:eth0:off:8.8.8.8
     ^addr    ^gw       ^netmask        ^hostname ^if  ^none ^dns
```

No DHCP, no network manager, instant connectivity.

**Files:**
- `scripts/boxcutter-net` — one-time infrastructure setup
- `scripts/boxcutter-ctl` — per-TAP setup/teardown in `setup_vm_tap()` / `teardown_vm_tap()`

## SSH to VMs (socat SO_BINDTODEVICE)

Since all VMs are 10.0.0.2, the Node VM can't simply `ssh 10.0.0.2` — it would connect to whichever TAP the kernel happens to route through. Instead, SSH is bound to a specific TAP device using socat:

```bash
ssh -o "ProxyCommand=socat - TCP:10.0.0.2:22,so-bindtodevice=tap-bold-fox" \
    -i /var/lib/boxcutter/ssh/id_ed25519 dev@10.0.0.2
```

`SO_BINDTODEVICE` forces the TCP connection to use the specified TAP, reaching the correct VM. This requires root (or `CAP_NET_RAW`), which is fine since `boxcutter-ctl` runs as root.

This is wrapped in the `vm_ssh()` helper function used throughout `boxcutter-ctl`.

## vmid: VM Identity via fwmark

vmid is the VM identity and token broker. It listens on port 80 on the Node VM. VMs reach it at `http://10.0.0.1/` (their TAP gateway address).

### The identification problem

All VMs are 10.0.0.2, so vmid can't identify them by source IP. Instead, it reads the **fwmark** from each accepted TCP connection.

### How it works

1. Packets from a VM arrive on its TAP and get marked in mangle PREROUTING (e.g., mark 41022)
2. The sysctl `net.ipv4.tcp_fwmark_accept=1` causes accepted TCP sockets to inherit the packet's fwmark
3. vmid's custom `markListener` reads the mark via `getsockopt(SOL_SOCKET, SO_MARK)` on each accepted connection
4. The mark is injected into the request context via `http.Server.ConnContext`
5. The identity middleware calls `registry.LookupMark(mark)` to find the VM record

```go
// In vmid's main.go:
func readSOMark(conn net.Conn) int {
    raw.Control(func(fd uintptr) {
        syscall.Syscall6(syscall.SYS_GETSOCKOPT,
            fd, syscall.SOL_SOCKET, syscall.SO_MARK, ...)
    })
}

// ConnContext injects the mark:
ConnContext: func(ctx context.Context, c net.Conn) context.Context {
    if mc, ok := c.(*markConn); ok {
        return middleware.WithMark(ctx, mc.mark)
    }
    return ctx
}
```

**Critical sysctl:** `net.ipv4.tcp_fwmark_accept=1` must be set (in `boxcutter-net`). Without it, accepted sockets always have mark 0 regardless of the packet's fwmark.

### vmid endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /` | Metadata root (VM ID, available endpoints) |
| `GET /identity` | VM record (ID, mark, mode) |
| `GET /token` | ES256 JWT for the requesting VM |
| `GET /token/github` | GitHub App installation token |
| `GET /.well-known/jwks.json` | JWKS public key (no auth required) |

Admin socket (`/run/vmid/admin.sock`):

| Endpoint | Description |
|----------|-------------|
| `POST /internal/vms` | Register a VM (with mark + mode) |
| `DELETE /internal/vms/{id}` | Deregister (purges sentinels) |
| `GET /internal/vms` | List registered VMs |
| `GET /internal/sentinel/{sentinel}` | Swap sentinel for real token |

**Files:**
- `vmid/cmd/vmid/main.go` — markListener, markConn, SO_MARK reading
- `vmid/internal/middleware/identity.go` — mark-based VM lookup
- `vmid/internal/registry/registry.go` — VM registry with byMark index
- `vmid/internal/api/metadata.go` — VM-facing endpoints
- `vmid/internal/api/admin.go` — admin endpoints + sentinel swap

## Normal vs Paranoid Mode

VMs are created with a mode: `normal` (default) or `paranoid`.

### Normal mode

- Full direct internet access via NAT
- Real credentials returned from vmid token endpoints
- No proxy required

### Paranoid mode

Paranoid VMs cannot reach the internet directly. All outbound traffic must go through the forward proxy.

**iptables rules (added on start, removed on stop):**

```bash
# Block direct internet (anything not destined for 10.0.0.0/24)
iptables -I FORWARD -i tap-bold-fox ! -d 10.0.0.0/24 -j DROP

# Allow traffic to the proxy (10.0.0.1:8080)
iptables -I FORWARD -i tap-bold-fox -d 10.0.0.1/32 -p tcp --dport 8080 -j ACCEPT
```

Rule ordering matters: the proxy ACCEPT is inserted before the DROP, so proxy traffic is allowed while everything else is blocked. (Traffic to 10.0.0.1 on other ports — like vmid on port 80 — goes through INPUT, not FORWARD, so it's unaffected by these rules.)

**Proxy environment:** A script is injected into the VM at `/etc/profile.d/boxcutter-proxy.sh`:

```bash
export HTTP_PROXY=http://10.0.0.1:8080
export HTTPS_PROXY=http://10.0.0.1:8080
export NO_PROXY=10.0.0.1,localhost,127.0.0.1
```

**Sentinel tokens:** When a paranoid VM requests a GitHub token from vmid, it receives a sentinel token (256-bit random hex) instead of the real credential. The real token is held in vmid's in-memory sentinel store. When the VM makes an API call through the proxy, the proxy intercepts the Authorization header, recognizes the sentinel, swaps it for the real token via vmid's admin socket, and forwards the request with the real credential. The sentinel is one-time-use and is purged when the VM is deregistered.

## Forward Proxy (boxcutter-proxy)

A Go binary using `elazarl/goproxy`. Listens on `:8080` (reachable from all VMs at `10.0.0.1:8080`).

### Capabilities

1. **MITM HTTPS** — uses the internal CA cert (`/etc/boxcutter/ca.{crt,key}`) to intercept and inspect HTTPS traffic
2. **Sentinel token swapping** — scans `Authorization` headers, resolves sentinels via `GET /internal/sentinel/{value}` on vmid's admin socket, replaces sentinel with real token before forwarding
3. **Egress allowlist** — in paranoid mode, restricts which domains VMs can reach (configured in `/etc/boxcutter/proxy-allowlist.conf`)

### Sentinel swap flow

```
Paranoid VM                    Proxy (:8080)                vmid (admin socket)
    │                              │                              │
    │  GET api.github.com          │                              │
    │  Authorization: Bearer abc...│                              │
    │─────────────────────────────→│                              │
    │                              │  GET /internal/sentinel/abc..│
    │                              │─────────────────────────────→│
    │                              │  {"token": "ghp_real..."}    │
    │                              │←─────────────────────────────│
    │                              │                              │
    │                              │  GET api.github.com          │
    │                              │  Authorization: Bearer ghp_real...
    │                              │─────────────────────────────→ GitHub
```

**Files:**
- `proxy/cmd/proxy/main.go` — proxy binary
- `systemd/boxcutter-proxy.service` — systemd unit

## TLS Infrastructure

An internal CA provides TLS for MITM proxying and service authentication.

- **CA:** EC P-256, 10-year validity, at `/etc/boxcutter/ca.{crt,key}`
- **Leaf cert:** IP SAN `10.0.0.1`, at `/etc/boxcutter/leaf.{crt,key}`
- **Generation:** `scripts/boxcutter-tls` (idempotent, called from `install.sh`)

The CA cert is injected into each VM's trust store at creation time:

```bash
# In cmd_create:
cp /etc/boxcutter/ca.crt <mounted-rootfs>/usr/local/share/ca-certificates/boxcutter-ca.crt
chroot <mounted-rootfs> update-ca-certificates
```

This allows VMs to trust HTTPS connections through the MITM proxy without TLS errors.

## DERP Relay

A local Tailscale DERP relay runs on the Node VM for faster VM-to-VM Tailscale connectivity:

- Listens on `10.0.0.1:443` with manual TLS certs (symlinked from the leaf cert)
- STUN on port 3478
- `--verify-clients` ensures only authenticated Tailscale nodes can use it

For paranoid VMs, the DERP relay can be configured as the only relay (`OmitDefaultRegions: true`), preventing Tailscale traffic from traversing public DERP servers.

**Files:**
- `systemd/boxcutter-derper.service` — systemd unit

## Tailscale Overlay

Each VM joins Tailscale at boot, getting a routable Tailscale IP accessible from any device on the tailnet. Tailscale uses userspace networking (the Firecracker kernel lacks `CONFIG_TUN`).

- The Node VM provisions Tailscale by SSHing into each VM over the internal TAP and running `tailscale up` with an ephemeral auth key
- The auth key lives only on the Node VM (`/etc/boxcutter/tailscale-authkey`), never on VM disk images
- `tailscale serve --bg --tcp 22 tcp://localhost:22` proxies SSH over Tailscale
- Ephemeral keys auto-remove nodes when they disconnect (on VM destroy)

## Complete packet flow examples

### Normal VM reaching the internet

```
1. VM (10.0.0.2) sends packet to 8.8.8.8
2. Packet exits VM's eth0, arrives on Node's tap-bold-fox
3. mangle PREROUTING: CONNMARK --restore-mark (no mark yet for new conn)
4. mangle PREROUTING: -i tap-bold-fox → MARK --set-mark 41022
5. FORWARD: -i tap-bold-fox → ACCEPT
6. mangle POSTROUTING: CONNMARK --save-mark (saves 41022 to conntrack)
7. nat POSTROUTING: MASQUERADE (10.0.0.2 → Node's uplink IP)
8. Packet exits via uplink to internet

Return:
9.  Reply arrives on uplink
10. mangle PREROUTING: CONNMARK --restore-mark (restores 41022)
11. ip rule: fwmark 41022 → lookup table 41022
12. table 41022: 10.0.0.2 dev tap-bold-fox
13. Packet delivered to tap-bold-fox → VM receives reply
```

### Paranoid VM reaching GitHub through proxy

```
1. VM (10.0.0.2) connects to 10.0.0.1:8080 (proxy)
   - This is local delivery (INPUT chain), not forwarded
   - FORWARD DROP rule doesn't apply (destination is 10.0.0.0/24)
2. Proxy receives request: GET https://api.github.com/repos/...
   Authorization: Bearer <sentinel>
3. Proxy resolves sentinel via vmid admin socket → gets real token
4. Proxy forwards to api.github.com with real token
5. Response flows back through proxy to VM
```

### vmid identifying a VM

```
1. VM (10.0.0.2) sends HTTP request to 10.0.0.1:80 (vmid)
2. Packet arrives on tap-bold-fox
3. mangle PREROUTING: marks packet with 41022
4. Packet delivered locally to vmid (INPUT, not FORWARD)
5. tcp_fwmark_accept=1 → accepted socket inherits mark 41022
6. vmid reads SO_MARK=41022 via getsockopt
7. registry.LookupMark(41022) → VMRecord{VMID: "bold-fox", ...}
8. Identity middleware attaches VM record to request context
9. Handler returns VM-specific response
```

## Summary of subnets and ports

| Subnet/Address | Purpose |
|---|---|
| `192.168.50.0/30` | Host ↔ Node VM link |
| `10.0.0.0/30` (per TAP) | Node VM ↔ each Firecracker VM (point-to-point) |
| `100.x.x.x` | Tailscale overlay (external access) |

| Port | Service | Listener |
|---|---|---|
| 80 | vmid (metadata) | Node VM, all interfaces |
| 443 | DERP relay | Node VM (`10.0.0.1`) |
| 3478/udp | DERP STUN | Node VM |
| 8080 | Forward proxy | Node VM, all interfaces |
| 8880 | Caddy HTTP | Node VM |
| 8443 | Caddy HTTPS | Node VM |

| File | Role |
|---|---|
| `host/boxcutter.env` | Host ↔ Node subnet config |
| `host/setup.sh` | Host-side TAP + NAT + VM route |
| `scripts/boxcutter-net` | Node-side one-time fwmark/NAT rules |
| `scripts/boxcutter-ctl` | Per-VM TAP setup/teardown, `vm_ssh()` |
| `scripts/boxcutter-tls` | CA + leaf cert generation |
| `vmid/` | VM identity service (mark-based) |
| `proxy/` | Forward proxy (sentinel swapping) |
