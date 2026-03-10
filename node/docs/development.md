# Node Development

## Building

```bash
# Node agent
cd node/agent && go build -o boxcutter-node ./cmd/node/

# VM identity
cd node/vmid && go build -o vmid ./cmd/vmid/

# MITM proxy
cd node/proxy && go build -o boxcutter-proxy ./cmd/proxy/
```

All binaries target `linux/amd64`. Cross-compilation works with `GOOS=linux GOARCH=amd64`.

## Tests

```bash
cd node/vmid && go test ./...
```

Tests exist for the vmid registry (mark allocation) and sentinel store (token management). Other node modules don't have tests yet — they're validated through integration testing on real VMs.

## Deploying

Cross-compile, copy to the running node VM, and restart:

```bash
# Node agent
cd node/agent && GOOS=linux GOARCH=amd64 go build -o boxcutter-node ./cmd/node/
scp -o StrictHostKeyChecking=no boxcutter-node ubuntu@192.168.50.3:/tmp/
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.3 \
  "sudo mv /tmp/boxcutter-node /usr/local/bin/ && sudo systemctl restart boxcutter-node"
```

Same pattern for `vmid` and `boxcutter-proxy`. Service names match binary names.

You can use Tailscale hostnames instead of bridge IPs if MagicDNS is set up:

```bash
scp boxcutter-node ubuntu@boxcutter-node-1:/tmp/
```

## Shell Scripts

Scripts live at `/usr/local/bin/` on nodes:

```bash
scp node/scripts/boxcutter-ctl ubuntu@192.168.50.3:/tmp/
ssh ubuntu@192.168.50.3 "sudo mv /tmp/boxcutter-ctl /usr/local/bin/ && sudo chmod +x /usr/local/bin/boxcutter-ctl"
```

## Golden Image

Rebuild the Firecracker guest rootfs (~2 min with Docker cache, ~8 min without):

```bash
# SSH into a node
ssh ubuntu@192.168.50.3

# Build golden image from Dockerfile
sudo boxcutter-ctl golden build
```

To distribute to all nodes, publish to OCI and set the head version:

```bash
# On the host
sudo boxcutter-host push-golden

# Via SSH to orchestrator
ssh boxcutter golden set-head <version>
```

## Debugging

```bash
# Node agent logs
ssh ubuntu@192.168.50.3 "sudo journalctl -u boxcutter-node -f"

# vmid logs
ssh ubuntu@192.168.50.3 "sudo journalctl -u vmid -f"

# Proxy logs
ssh ubuntu@192.168.50.3 "sudo journalctl -u boxcutter-proxy -f"

# Node agent API
curl http://192.168.50.3:8800/api/vms
curl http://192.168.50.3:8800/api/vms/<name>
curl http://192.168.50.3:8800/api/health
curl http://192.168.50.3:8800/api/golden/versions

# vmid admin socket
ssh ubuntu@192.168.50.3 \
  "sudo curl --unix-socket /run/vmid/admin.sock http://localhost/internal/vms"

# Firecracker VM management
ssh ubuntu@192.168.50.3
sudo boxcutter-ctl list                  # List running VMs
sudo boxcutter-ctl shell <name>          # Shell into a Firecracker VM
sudo boxcutter-ctl logs <name>           # View VM serial console log
```

## SSH Access

```bash
make ssh-node              # SSH to node-1
bash host/ssh.sh node 1    # node-1
bash host/ssh.sh node 2    # node-2
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.3   # direct
```

## Systemd Services

| Service | Binary | Port | Description |
|---------|--------|------|-------------|
| `vmid` | `vmid` | 169.254.169.254:80 + `/run/vmid/admin.sock` | VM identity + metadata |
| `boxcutter-proxy` | `boxcutter-proxy` | `:8080` | MITM proxy, sentinel tokens |
| `boxcutter-node` | `boxcutter-node` | `:8800` | Node agent, VM lifecycle API |
| `boxcutter-net` | (shell script) | — | Network setup (oneshot) |
| `boxcutter-derper` | `derper` | `:443` | Tailscale DERP relay |
