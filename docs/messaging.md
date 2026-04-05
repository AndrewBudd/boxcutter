# VM-to-VM Messaging

Boxcutter provides a built-in message queue that lets VMs communicate with each other securely through the metadata service. Messages are routed through the control plane — VMs don't need direct network access to each other.

## API

All endpoints are on the metadata service at `http://169.254.169.254`. The sender's identity is derived automatically from the VM's network identity (fwmark) — no authentication tokens needed.

### Send a message

```
POST /messages/send
Content-Type: application/json

{
  "to": "target-vm-name",
  "body": "message content",
  "subject": "optional subject"
}
```

Response:
```json
{"id": "msg-1234567890", "status": "delivered"}
```

Status is `"delivered"` for same-node delivery or `"relayed"` for cross-node.

### Consume inbox

```
GET /messages
```

Returns all pending messages and marks them as **in-flight** for 30 seconds:

```json
[
  {
    "id": "msg-1234567890",
    "from": "sender-vm",
    "to": "my-vm",
    "subject": "optional subject",
    "body": "message content",
    "created_at": "2026-04-05T02:06:44Z",
    "in_flight": "2026-04-05T02:06:50Z"
  }
]
```

While in-flight, messages won't be returned by subsequent `GET /messages` calls. If not acknowledged within 30 seconds, they return to pending and will be redelivered.

### Acknowledge (delete) a message

```
DELETE /messages/{id}
```

Permanently removes the message. Returns `204 No Content`.

## Message lifecycle

```
PENDING ──GET /messages──> IN_FLIGHT ──DELETE /messages/{id}──> DELETED
                               │
                               │ 30s timeout (no ack)
                               │
                               └──> PENDING (redelivered)
```

1. A message arrives in the recipient's mailbox as **PENDING**
2. `GET /messages` returns it and marks it **IN_FLIGHT**
3. The consumer processes the message and calls `DELETE` to acknowledge it
4. If the consumer crashes or doesn't ack within 30 seconds, the message returns to **PENDING** and will be redelivered on the next `GET`

## Usage from inside a VM

```bash
# Send a message
curl -X POST http://169.254.169.254/messages/send \
  -H "Content-Type: application/json" \
  -d '{"to": "other-vm", "body": "hello", "subject": "greeting"}'

# Check inbox
curl -s http://169.254.169.254/messages | jq .

# Acknowledge a message
curl -X DELETE http://169.254.169.254/messages/msg-1234567890

# Simple consume-and-ack loop
while true; do
  MSGS=$(curl -sf http://169.254.169.254/messages)
  if [ "$MSGS" = "[]" ]; then
    sleep 5
    continue
  fi
  echo "$MSGS" | jq -c '.[]' | while read msg; do
    ID=$(echo "$msg" | jq -r .id)
    BODY=$(echo "$msg" | jq -r .body)
    FROM=$(echo "$msg" | jq -r .from)
    echo "From $FROM: $BODY"
    # Process the message...
    curl -sf -X DELETE "http://169.254.169.254/messages/$ID"
  done
done
```

## Cross-node routing

VMs on the same node get direct delivery (no network hops). VMs on different nodes are routed through the control plane:

```
Sender VM  →  vmid (metadata)  →  node agent  →  orchestrator  →  target node agent  →  target vmid  →  Recipient VM
```

This is transparent to the VMs — the same `POST /messages/send` works regardless of where the target VM is running.

## Migration and destruction

- **Migration**: When a VM migrates between nodes, its mailbox travels with it. Pending and in-flight messages are preserved (in-flight status is reset so messages get redelivered on the new node).
- **Destruction**: When a VM is destroyed, its mailbox is deleted.

## Error handling

| HTTP Status | Meaning |
|-------------|---------|
| 201 | Message sent successfully |
| 204 | Message acknowledged |
| 400 | Missing required field (`to` or `body`) |
| 404 | Target VM not found, or message ID not found |
| 502 | Cross-node relay failed (target node unreachable) |
