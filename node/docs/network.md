# Node Network Architecture

This document describes the networking inside node VMs: how Firecracker VMs are isolated, identified, and connected.

## Per-TAP fwmark Routing

**Key insight: every Firecracker VM has the same IP address (10.0.0.2).** There is no shared bridge inside nodes, no IP pool, no DHCP. Each VM gets its own isolated TAP device with a point-to-point /30 link. Linux fwmark-based policy routing directs return traffic to the correct TAP.

```
Node side:  10.0.0.1 peer 10.0.0.2  on tap-<name>
VM side:    10.0.0.2 gateway 10.0.0.1   (same for every VM)
```

### Mark Allocation

Marks are derived from the VM name using CRC32:

```go
mark = crc32(name) % 65535 + 1
```

Range is 1-65535 (avoids 0, fits 16-bit). Collisions are resolved by rehashing with a numeric suffix (`name_1`, `name_2`, ...).

### Per-TAP Setup (on VM start)

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

Teardown reverses all of these on VM stop.

### How Return Traffic Finds the Right TAP

1. **Outbound:** Packet arrives on `tap-bold-fox` → mangle PREROUTING marks it with 41022 → CONNMARK saves 41022 to conntrack → NATed and forwarded
2. **Return:** Reply arrives on uplink → mangle PREROUTING restores mark 41022 from conntrack → ip rule matches fwmark 41022 → routes via table 41022 → delivered to `tap-bold-fox`

### One-Time Infrastructure (boxcutter-net)

Run at boot before any VMs start:

```bash
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.ipv4.tcp_fwmark_accept=1        # critical for vmid

iptables -t mangle -A PREROUTING -j CONNMARK --restore-mark
iptables -t mangle -A POSTROUTING -m mark ! --mark 0 -j CONNMARK --save-mark
iptables -t nat -A POSTROUTING -s 10.0.0.2/32 -o <uplink> -j MASQUERADE
iptables -A FORWARD -i <uplink> -m state --state RELATED,ESTABLISHED -j ACCEPT
```

### ip rule priority

Formula: `10000 + (mark % 20000)`, range 10000-29999. Must be below 32766 (main table priority).

### VM Isolation

- No shared L2 domain (no bridge) — each VM has its own TAP
- Each VM only sees its point-to-point link to 10.0.0.1
- No route between TAPs; VMs cannot reach each other
- All VMs use the same fixed MAC (`AA:FC:00:00:00:01`) — safe because no shared Layer 2

### VM Network Configuration

VMs get their network config via the kernel `ip=` boot parameter (no DHCP):

```
ip=10.0.0.2::10.0.0.1:255.255.255.252:bold-fox:eth0:off:8.8.8.8
```

## vmid and fwmark

vmid identifies VMs by reading the fwmark from each TCP connection:

1. Packets from a VM arrive on its TAP and get marked in mangle PREROUTING
2. `net.ipv4.tcp_fwmark_accept=1` causes accepted TCP sockets to inherit the packet's fwmark
3. vmid's `markListener` reads the mark via `getsockopt(SOL_SOCKET, SO_MARK)`
4. The mark is injected into the request context via `http.Server.ConnContext`
5. Identity middleware calls `registry.LookupMark(mark)` to find the VM record

**Critical:** Without `tcp_fwmark_accept=1`, accepted sockets always have mark 0.

## SSH to VMs (socat SO_BINDTODEVICE)

Since all VMs are 10.0.0.2, SSH is bound to a specific TAP device:

```bash
ssh -o "ProxyCommand=socat - TCP:10.0.0.2:22,so-bindtodevice=tap-bold-fox" \
    -i /var/lib/boxcutter/ssh/id_ed25519 dev@10.0.0.2
```

`SO_BINDTODEVICE` forces the TCP connection to the specified TAP. Requires root or `CAP_NET_RAW`.

## Paranoid Mode Networking

Paranoid VMs cannot reach the internet directly:

```bash
# Block direct internet
iptables -I FORWARD -i tap-bold-fox ! -d 10.0.0.0/24 -j DROP

# Allow traffic to the proxy
iptables -I FORWARD -i tap-bold-fox -d 10.0.0.1/32 -p tcp --dport 8080 -j ACCEPT
```

Rule ordering: proxy ACCEPT is inserted before DROP. Traffic to 10.0.0.1 on other ports (like vmid on 80) goes through INPUT, not FORWARD, so it's unaffected.

Inside paranoid VMs, `/etc/profile.d/boxcutter-proxy.sh` sets `HTTP_PROXY` and `HTTPS_PROXY` to `http://10.0.0.1:8080`.

## Sentinel Token Flow

```
Paranoid VM                    Proxy (:8080)                vmid (admin socket)
    |                              |                              |
    |  GET api.github.com          |                              |
    |  Authorization: Bearer abc...|                              |
    |------------------------------>                              |
    |                              |  GET /internal/sentinel/abc..|
    |                              |------------------------------>
    |                              |  {"token": "ghp_real..."}    |
    |                              |<------------------------------
    |                              |                              |
    |                              |  GET api.github.com          |
    |                              |  Authorization: Bearer ghp_real...
    |                              |------------------------------> GitHub
```

Sentinel tokens are one-time-use and purged when the VM is deregistered.

## TLS Infrastructure

An internal CA provides TLS for MITM proxying and service authentication:

- **CA:** EC P-256, 10-year validity, at `/etc/boxcutter/ca.{crt,key}`
- **Leaf cert:** IP SAN `10.0.0.1`, at `/etc/boxcutter/leaf.{crt,key}`
- **Generation:** `node/scripts/boxcutter-tls` (idempotent)

The CA cert is injected into each VM's trust store at creation time so HTTPS through the MITM proxy works without TLS errors.

## DERP Relay

Local Tailscale DERP relay on each node for faster VM-to-VM connectivity:

- Listens on `10.0.0.1:443` with manual TLS certs (symlinked from the leaf cert)
- STUN on port 3478
- `--verify-clients` ensures only authenticated Tailscale nodes can use it

## Tailscale Overlay

Each Firecracker VM joins Tailscale at boot, getting a routable Tailscale IP. Uses userspace networking (Firecracker kernel lacks `CONFIG_TUN`).

- Node provisions Tailscale by SSHing into each VM and running `tailscale up` with an ephemeral auth key
- Auth key lives only on the node VM, never on VM disk images
- Ephemeral keys auto-remove nodes when they disconnect (on VM destroy)

## vsock (Migration Nudge)

After snapshot-based migration, the node agent connects to the VM's vsock device (guest_cid=3, port 52) and triggers `tailscale netcheck` to re-establish DERP connections through the new node.

- Guest side: `boxcutter-vsock-listen` (C binary) + `boxcutter-nudge` (shell script)
- Host side: `fcVsockNudge()` connects to the vsock UDS, sends `CONNECT 52\n`

## Packet Flow Examples

### Normal VM reaching the internet

```
1. VM (10.0.0.2) sends packet to 8.8.8.8
2. Packet exits VM's eth0, arrives on Node's tap-bold-fox
3. mangle PREROUTING: CONNMARK --restore-mark (no mark yet for new conn)
4. mangle PREROUTING: -i tap-bold-fox -> MARK --set-mark 41022
5. FORWARD: -i tap-bold-fox -> ACCEPT
6. mangle POSTROUTING: CONNMARK --save-mark (saves 41022 to conntrack)
7. nat POSTROUTING: MASQUERADE (10.0.0.2 -> Node's uplink IP)
8. Packet exits via uplink to internet

Return:
9.  Reply arrives on uplink
10. mangle PREROUTING: CONNMARK --restore-mark (restores 41022)
11. ip rule: fwmark 41022 -> lookup table 41022
12. table 41022: 10.0.0.2 dev tap-bold-fox
13. Packet delivered to tap-bold-fox -> VM receives reply
```

### vmid identifying a VM

```
1. VM (10.0.0.2) sends HTTP request to 10.0.0.1:80 (vmid)
2. Packet arrives on tap-bold-fox
3. mangle PREROUTING: marks packet with 41022
4. Packet delivered locally to vmid (INPUT, not FORWARD)
5. tcp_fwmark_accept=1 -> accepted socket inherits mark 41022
6. vmid reads SO_MARK=41022 via getsockopt
7. registry.LookupMark(41022) -> VMRecord{VMID: "bold-fox", ...}
8. Identity middleware attaches VM record to request context
```

## Summary of Ports

| Port | Service | Listener |
|------|---------|----------|
| 80 | vmid (metadata) | 169.254.169.254 (cloud metadata address) |
| 443 | DERP relay | `10.0.0.1` |
| 3478/udp | DERP STUN | Node VM |
| 8080 | Forward proxy | All interfaces |
| 8800 | Node agent API | Node VM |

## Key Files

| File | Role |
|------|------|
| `node/scripts/boxcutter-net` | One-time fwmark/NAT rules |
| `node/agent/internal/vm/` | Per-VM TAP setup/teardown (Go) |
| `node/scripts/boxcutter-tls` | CA + leaf cert generation |
| `node/vmid/` | VM identity service |
| `node/proxy/` | Forward proxy |
