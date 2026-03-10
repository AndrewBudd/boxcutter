# Host Bridge Network

The host creates and manages the Layer 1 bridge network that connects all QEMU VMs.

## Bridge Setup

```
Physical Host                         QEMU VMs
192.168.50.1/24  ── br-boxcutter ──→  Orchestrator: 192.168.50.2
                                 ──→  Node-1:       192.168.50.3
                                 ──→  Node-2:       192.168.50.4
                                 ──→  Node-N:       192.168.50.(2+N)
```

A Linux bridge device (`br-boxcutter`) with IP `192.168.50.1/24`. Each QEMU VM gets its own TAP device attached to the bridge:

- **Orchestrator:** `tap-orch`, MAC `52:54:00:00:00:02`, IP `192.168.50.2`
- **Node-1:** `tap-node1`, MAC `52:54:00:00:00:03`, IP `192.168.50.3`
- **Node-N:** `tap-nodeN`, MAC auto-derived, IP `192.168.50.(2+N)`

NAT masquerade on the host's physical NIC gives all VMs internet access. The bridge setup is managed by `boxcutter-host` and is idempotent (recreated on every boot).

## Configuration

`host/boxcutter.env` controls the network layout:

```bash
HOST_INTERFACE=enp34s0      # Physical NIC for NAT (auto-detected if not set)
BRIDGE_DEVICE=br-boxcutter
BRIDGE_IP=192.168.50.1
BRIDGE_CIDR=192.168.50.0/24
ORCH_IP=192.168.50.2
NODE_IP_OFFSET=3            # First node at 192.168.50.3
```

## MQTT Broker

Mosquitto runs on the host at `192.168.50.1:1883`, used for golden image version notifications (orchestrator publishes, nodes subscribe).

## Key Files

| File | Purpose |
|------|---------|
| `host/internal/bridge/bridge.go` | Bridge/TAP/NAT setup (Go, idempotent) |
| `host/boxcutter.env` | Network and resource configuration |
| `host/mosquitto.conf` | MQTT broker configuration |
