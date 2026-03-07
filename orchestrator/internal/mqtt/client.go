// Package mqtt provides MQTT connectivity for the orchestrator.
// Publishes golden head version and subscribes to node status.
package mqtt

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const (
	TopicGoldenHead = "boxcutter/golden/head"
	TopicVMEvents   = "boxcutter/events/vm"

	defaultBrokerAddr = "tcp://192.168.50.1:1883"
)

// Client wraps an MQTT connection for the orchestrator.
type Client struct {
	client paho.Client
}

// Config for the MQTT client.
type Config struct {
	BrokerAddr string
}

// Connect creates and connects an MQTT client for the orchestrator.
func Connect(cfg Config) (*Client, error) {
	if cfg.BrokerAddr == "" {
		cfg.BrokerAddr = defaultBrokerAddr
	}

	c := &Client{}

	opts := paho.NewClientOptions().
		AddBroker(cfg.BrokerAddr).
		SetClientID("boxcutter-orchestrator").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(_ paho.Client) {
			log.Printf("mqtt: connected to %s", cfg.BrokerAddr)
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			log.Printf("mqtt: connection lost: %v", err)
		})

	c.client = paho.NewClient(opts)

	// Connect in background (non-blocking)
	go func() {
		for {
			token := c.client.Connect()
			token.Wait()
			if token.Error() == nil {
				return
			}
			log.Printf("mqtt: connect failed: %v, retrying in 5s", token.Error())
			time.Sleep(5 * time.Second)
		}
	}()

	return c, nil
}

// PublishGoldenHead publishes the current golden head version as a retained message.
func (c *Client) PublishGoldenHead(version string) error {
	if !c.client.IsConnected() {
		return fmt.Errorf("not connected to MQTT broker")
	}
	token := c.client.Publish(TopicGoldenHead, 1, true, []byte(version))
	token.Wait()
	return token.Error()
}

// PublishVMEvent publishes a VM lifecycle event.
func (c *Client) PublishVMEvent(event map[string]string) {
	if !c.client.IsConnected() {
		return
	}
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
