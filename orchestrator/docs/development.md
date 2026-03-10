# Orchestrator Development

## Building

```bash
cd orchestrator && go build -o boxcutter-orchestrator ./cmd/orchestrator/
cd orchestrator && go build -o boxcutter-ssh-orchestrator ./cmd/ssh/
```

## Deploying

Build, copy to the running orchestrator VM, and restart:

```bash
cd orchestrator && GOOS=linux GOARCH=amd64 go build -o boxcutter-orchestrator ./cmd/orchestrator/
scp -o StrictHostKeyChecking=no boxcutter-orchestrator ubuntu@192.168.50.2:/tmp/
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2 \
  "sudo mv /tmp/boxcutter-orchestrator /usr/local/bin/ && sudo systemctl restart boxcutter-orchestrator"
```

For the SSH ForceCommand binary:

```bash
cd orchestrator && GOOS=linux GOARCH=amd64 go build -o boxcutter-ssh-orchestrator ./cmd/ssh/
scp -o StrictHostKeyChecking=no boxcutter-ssh-orchestrator ubuntu@192.168.50.2:/tmp/
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2 \
  "sudo mv /tmp/boxcutter-ssh-orchestrator /usr/local/bin/"
```

No restart needed for the SSH binary — it's invoked fresh on each SSH connection.

You can also use Tailscale hostnames instead of bridge IPs if MagicDNS is set up.

## Debugging

```bash
# Logs
ssh ubuntu@192.168.50.2 "sudo journalctl -u boxcutter-orchestrator -f"

# API
curl http://192.168.50.2:8801/api/vms
curl http://192.168.50.2:8801/api/nodes
curl http://192.168.50.2:8801/api/health
```

## SSH Access

```bash
make ssh-orchestrator
# or
ssh -o StrictHostKeyChecking=no ubuntu@192.168.50.2
```

## Systemd

| Service | Binary | Port | Description |
|---------|--------|------|-------------|
| `boxcutter-orchestrator` | `boxcutter-orchestrator` | `:8801` | Scheduling, state, MQTT |
