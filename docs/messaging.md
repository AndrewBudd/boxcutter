# Boxcutter Messaging System

The boxcutter messaging system provides a publish-subscribe messaging system for communication between VMs and external systems. It follows an SNS/SQS-style architecture with topics, queues, and subscriptions.

## SSH CLI Commands

All messaging commands are accessed through the SSH CLI:

```bash
ssh boxcutter@host msg <command> [args]
```

### Topics Commands

#### List Topics
Display all available topics in the system:
```bash
ssh boxcutter@host msg topics
```

#### Publish Message
Publish a message to a specific topic:
```bash
ssh boxcutter@host msg publish <topic> <message>
```
Example:
```bash
ssh boxcutter@host msg publish system-alerts "Server maintenance scheduled for tonight"
```

### Queues Commands

#### List Queues
Display all queues with their current message depth:
```bash
ssh boxcutter@host msg queues
```

#### Read Messages
Read messages from a specific queue:
```bash
ssh boxcutter@host msg read <queue>
```
Example:
```bash
ssh boxcutter@host msg read agent1.inbox
```

#### Inspect Queue
Show detailed information about a queue's depth:
```bash
ssh boxcutter@host msg inspect <queue>
```

### Subscriptions Commands

#### List Subscriptions
Display all topic-to-queue subscriptions:
```bash
ssh boxcutter@host msg subscriptions
```

#### Subscribe Queue to Topic
Subscribe a queue to receive messages from a topic:
```bash
ssh boxcutter@host msg subscribe <topic> <queue>
```
Example:
```bash
ssh boxcutter@host msg subscribe system-alerts agent1.inbox
```

### Direct Messaging Commands

#### Send Direct Message
Send a message directly to an agent's inbox queue:
```bash
ssh boxcutter@host msg send <agent> <message>
```
Example:
```bash
ssh boxcutter@host msg send webserver01 "Please restart the web service"
```

## Message Lifecycle

1. **Publish**: Messages are published to topics
2. **Fan-out**: Published messages are copied to all subscribed queues
3. **Queue Storage**: Messages remain in queues until consumed
4. **Read**: Consumers read messages from queues (messages become "in-flight")
5. **Acknowledge**: Consumers acknowledge messages to remove them from queues
6. **Timeout**: If not acknowledged within a timeframe, messages return to "pending" status

## Message Acknowledgment

To acknowledge (remove) messages from a queue, you need to use the HTTP API directly:

```bash
curl -X POST http://orchestrator:8801/api/queues/{queue_name}/ack \
  -H "Content-Type: application/json" \
  -d '{"message_ids": ["msg-1234567890"]}'
```

The SSH CLI does not currently expose an ack command, so direct API calls are required for message acknowledgment.
