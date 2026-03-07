// Package mqtt provides MQTT connectivity for the node agent.
// Connects to the broker on the host bridge IP and handles
// golden image head notifications and node status publishing.
package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const (
	TopicGoldenHead = "boxcutter/golden/head"
	TopicNodeStatus = "boxcutter/node/%s/status"
	TopicNodeImages = "boxcutter/node/%s/images"
	TopicVMEvents   = "boxcutter/events/vm"

	defaultBrokerAddr = "tcp://192.168.50.1:1883"
)

// GoldenHeadHandler is called when the golden head version changes.
type GoldenHeadHandler func(version string)

// Client wraps an MQTT connection for the node agent.
type Client struct {
	client   paho.Client
	nodeID   string
	onGolden GoldenHeadHandler
	mu       sync.Mutex
}

// Config for the MQTT client.
type Config struct {
	BrokerAddr string // tcp://host:port (default: tcp://192.168.50.1:1883)
	NodeID     string // hostname of this node
	OnGolden   GoldenHeadHandler
}

// Connect creates and connects an MQTT client.
func Connect(cfg Config) (*Client, error) {
	if cfg.BrokerAddr == "" {
		cfg.BrokerAddr = defaultBrokerAddr
	}

	c := &Client{
		nodeID:   cfg.NodeID,
		onGolden: cfg.OnGolden,
	}

	opts := paho.NewClientOptions().
		AddBroker(cfg.BrokerAddr).
		SetClientID(fmt.Sprintf("boxcutter-node-%s", cfg.NodeID)).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(_ paho.Client) {
			log.Printf("mqtt: connected to %s", cfg.BrokerAddr)
			c.subscribe()
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			log.Printf("mqtt: connection lost: %v", err)
		})

	c.client = paho.NewClient(opts)

	// Single connect call — paho's ConnectRetry + AutoReconnect handle retries
	go func() {
		token := c.client.Connect()
		token.Wait()
		if token.Error() != nil {
			log.Printf("mqtt: connect failed: %v (auto-retry enabled)", token.Error())
		}
	}()

	return c, nil
}

func (c *Client) subscribe() {
	if c.onGolden != nil {
		token := c.client.Subscribe(TopicGoldenHead, 1, func(_ paho.Client, msg paho.Message) {
			version := string(msg.Payload())
			if version != "" {
				log.Printf("mqtt: golden head updated to %s", version)
				c.onGolden(version)
			}
		})
		token.Wait()
		if token.Error() != nil {
			log.Printf("mqtt: subscribe %s failed: %v", TopicGoldenHead, token.Error())
		}
	}
}

// NodeStatus is published periodically to report node health.
type NodeStatus struct {
	Hostname        string `json:"hostname"`
	RAMTotalMIB     int    `json:"ram_total_mib"`
	RAMAllocatedMIB int    `json:"ram_allocated_mib"`
	VMsRunning      int    `json:"vms_running"`
	GoldenReady     bool   `json:"golden_ready"`
	GoldenHead      string `json:"golden_head,omitempty"`
	Status          string `json:"status"`
	UpdatedAt       string `json:"updated_at"`
}

// PublishStatus publishes the node's current status as a retained message.
func (c *Client) PublishStatus(status *NodeStatus) {
	if !c.client.IsConnected() {
		return
	}
	status.UpdatedAt = time.Now().Format(time.RFC3339)
	data, _ := json.Marshal(status)
	topic := fmt.Sprintf(TopicNodeStatus, c.nodeID)
	c.client.Publish(topic, 1, true, data)
}

// PublishImages publishes the list of golden image versions on this node.
func (c *Client) PublishImages(versions []string) {
	if !c.client.IsConnected() {
		return
	}
	data, _ := json.Marshal(map[string]interface{}{
		"versions":   versions,
		"updated_at": time.Now().Format(time.RFC3339),
	})
	topic := fmt.Sprintf(TopicNodeImages, c.nodeID)
	c.client.Publish(topic, 1, true, data)
}

// PublishVMEvent publishes a VM lifecycle event.
func (c *Client) PublishVMEvent(event map[string]string) {
	if !c.client.IsConnected() {
		return
	}
	event["node_id"] = c.nodeID
	event["timestamp"] = time.Now().Format(time.RFC3339)
	data, _ := json.Marshal(event)
	c.client.Publish(TopicVMEvents, 1, false, data)
}

// Close disconnects the client.
func (c *Client) Close() {
	c.client.Disconnect(1000)
}

// IsConnected returns whether the client is currently connected.
func (c *Client) IsConnected() bool {
	return c.client.IsConnected()
}

// BrokerAddrFromEnv returns the broker address, checking MQTT_BROKER env first.
func BrokerAddrFromEnv() string {
	if addr := os.Getenv("MQTT_BROKER"); addr != "" {
		return addr
	}
	return defaultBrokerAddr
}
