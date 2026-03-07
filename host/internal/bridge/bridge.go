// Package bridge manages the host bridge device, TAP interfaces, and NAT rules.
package bridge

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

type Config struct {
	BridgeDevice string
	BridgeIP     string
	BridgeCIDR   string
	HostNIC      string // Physical NIC for NAT masquerade
}

// Setup creates the bridge device, sets its IP, and configures NAT.
// Idempotent — safe to call on every boot.
func Setup(cfg Config) error {
	// Enable IP forwarding
	if err := run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}

	// Create bridge if missing
	if !linkExists(cfg.BridgeDevice) {
		log.Printf("Creating bridge %s", cfg.BridgeDevice)
		if err := run("ip", "link", "add", "name", cfg.BridgeDevice, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge: %w", err)
		}
	}

	// Set IP (idempotent — ignore already-exists error)
	addr := fmt.Sprintf("%s/%s", cfg.BridgeIP, cfg.BridgeCIDR)
	run("ip", "addr", "add", addr, "dev", cfg.BridgeDevice)

	// Bring up
	if err := run("ip", "link", "set", cfg.BridgeDevice, "up"); err != nil {
		return fmt.Errorf("bring up bridge: %w", err)
	}

	// NAT masquerade
	subnet := fmt.Sprintf("%s/%s", cfg.BridgeIP, cfg.BridgeCIDR)
	iptablesEnsure("-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-o", cfg.HostNIC, "-j", "MASQUERADE")
	iptablesEnsure("-I", "FORWARD", "-i", cfg.BridgeDevice, "-j", "ACCEPT")
	iptablesEnsure("-I", "FORWARD", "-o", cfg.BridgeDevice, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	log.Printf("Bridge %s ready (%s)", cfg.BridgeDevice, addr)
	return nil
}

// EnsureTAP creates a TAP device and attaches it to the bridge if needed.
func EnsureTAP(tap, bridge, user string) error {
	if !linkExists(tap) {
		log.Printf("Creating TAP %s", tap)
		if err := run("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", user); err != nil {
			return fmt.Errorf("create TAP %s: %w", tap, err)
		}
	}
	// Always re-attach to bridge (survives bridge recreation)
	run("ip", "link", "set", tap, "master", bridge)
	if err := run("ip", "link", "set", tap, "up"); err != nil {
		return fmt.Errorf("bring up TAP %s: %w", tap, err)
	}
	return nil
}

// DeleteTAP removes a TAP device.
func DeleteTAP(tap string) error {
	if linkExists(tap) {
		return run("ip", "link", "delete", tap)
	}
	return nil
}

func linkExists(name string) bool {
	err := exec.Command("ip", "link", "show", name).Run()
	return err == nil
}

func run(args ...string) error {
	cmd := exec.Command("sudo", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// iptablesEnsure adds an iptables rule if it doesn't already exist.
func iptablesEnsure(args ...string) {
	// Build check args: replace -A/-I with -C
	checkArgs := make([]string, len(args))
	copy(checkArgs, args)
	for i, a := range checkArgs {
		if a == "-A" || a == "-I" {
			checkArgs[i] = "-C"
			break
		}
	}

	// Check if rule exists
	checkCmd := append([]string{"iptables"}, checkArgs...)
	if exec.Command("sudo", checkCmd...).Run() == nil {
		return // already exists
	}

	// Add the rule
	addCmd := append([]string{"iptables"}, args...)
	if err := run(addCmd...); err != nil {
		log.Printf("Warning: iptables rule failed: %v", err)
	}
}
