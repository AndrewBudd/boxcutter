package network

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Setup performs one-time network infrastructure setup (equivalent to boxcutter-net up).
func Setup() error {
	// Ensure device nodes
	ensureDeviceNodes()

	// Enable IP forwarding and fwmark accept
	if err := sysctl("net.ipv4.ip_forward", "1"); err != nil {
		return err
	}
	if err := sysctl("net.ipv4.tcp_fwmark_accept", "1"); err != nil {
		return err
	}

	uplink := defaultUplink()
	if uplink == "" {
		return fmt.Errorf("no default route found")
	}

	// CONNMARK save/restore for return traffic
	iptablesIdempotent("-t", "mangle", "-A", "PREROUTING", "-j", "CONNMARK", "--restore-mark")
	iptablesIdempotent("-t", "mangle", "-A", "POSTROUTING", "-m", "mark", "!", "--mark", "0", "-j", "CONNMARK", "--save-mark")

	// NAT all VM traffic
	iptablesIdempotent("-t", "nat", "-A", "POSTROUTING", "-s", "10.0.0.2/32", "-o", uplink, "-j", "MASQUERADE")

	// Allow established return traffic
	iptablesIdempotent("-A", "FORWARD", "-i", uplink, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT")

	return nil
}

func ensureDeviceNodes() {
	if _, err := os.Stat("/dev/loop-control"); os.IsNotExist(err) {
		exec.Command("mknod", "/dev/loop-control", "c", "10", "237").Run()
	}
	for i := 0; i < 8; i++ {
		dev := fmt.Sprintf("/dev/loop%d", i)
		if _, err := os.Stat(dev); os.IsNotExist(err) {
			exec.Command("mknod", "-m", "660", dev, "b", "7", fmt.Sprintf("%d", i)).Run()
		}
	}
	os.MkdirAll("/dev/net", 0755)
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		exec.Command("mknod", "/dev/net/tun", "c", "10", "200").Run()
	}
	os.Chmod("/dev/net/tun", 0666)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		exec.Command("mknod", "/dev/kvm", "c", "10", "232").Run()
	}
	os.Chmod("/dev/kvm", 0660)
}

func sysctl(key, value string) error {
	return exec.Command("sysctl", "-w", key+"="+value).Run()
}

func defaultUplink() string {
	out, err := exec.Command("ip", "route").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "default") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "dev" && i+1 < len(fields) {
					return fields[i+1]
				}
			}
		}
	}
	return ""
}

// iptablesIdempotent adds a rule if it doesn't already exist.
func iptablesIdempotent(args ...string) {
	// Try -C (check) first
	checkArgs := make([]string, len(args))
	copy(checkArgs, args)
	for i, a := range checkArgs {
		if a == "-A" {
			checkArgs[i] = "-C"
			break
		}
	}
	if exec.Command("iptables", checkArgs...).Run() == nil {
		return // Rule already exists
	}
	exec.Command("iptables", args...).Run()
}
