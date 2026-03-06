package vm

import (
	"crypto/md5"
	"fmt"
	"hash/crc32"
	"os/exec"
	"strings"
)

// TAPName returns the TAP device name for a VM, truncated to 15 chars.
func TAPName(name string) string {
	tap := "tap-" + name
	if len(tap) <= 15 {
		return tap
	}
	h := md5.Sum([]byte(name))
	return fmt.Sprintf("tap-%s%x", name[:7], h[:2])
}

// AllocateMark generates a fwmark from the VM name, checking for collisions.
func AllocateMark(name string, existingMarks map[int]bool) int {
	mark := crc32Mark(name)
	suffix := 0
	for existingMarks[mark] {
		suffix++
		mark = crc32Mark(fmt.Sprintf("%s_%d", name, suffix))
	}
	return mark
}

func crc32Mark(s string) int {
	h := crc32.ChecksumIEEE([]byte(s))
	return int(h%65535) + 1
}

// SetupTAP creates a TAP device with point-to-point addressing and fwmark routing.
func SetupTAP(tap string, mark int) error {
	uplink, gw := defaultRoute()

	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "addr", "add", "10.0.0.1", "peer", "10.0.0.2", "dev", tap},
		{"ip", "link", "set", tap, "up"},
		{"iptables", "-t", "mangle", "-I", "PREROUTING", "2", "-i", tap, "-j", "MARK", "--set-mark", fmt.Sprintf("%d", mark)},
	}

	prio := 10000 + (mark % 20000)
	cmds = append(cmds,
		[]string{"ip", "rule", "add", "fwmark", fmt.Sprintf("%d", mark), "lookup", fmt.Sprintf("%d", mark), "priority", fmt.Sprintf("%d", prio)},
		[]string{"ip", "route", "add", "10.0.0.2", "dev", tap, "table", fmt.Sprintf("%d", mark)},
	)

	if gw != "" && uplink != "" {
		cmds = append(cmds, []string{"ip", "route", "add", "default", "via", gw, "dev", uplink, "table", fmt.Sprintf("%d", mark)})
	}

	cmds = append(cmds, []string{"iptables", "-I", "FORWARD", "-i", tap, "-j", "ACCEPT"})

	for _, args := range cmds {
		if err := run(args[0], args[1:]...); err != nil {
			return fmt.Errorf("running %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// TeardownTAP removes TAP device and all associated rules.
func TeardownTAP(tap string, mark int) {
	markStr := fmt.Sprintf("%d", mark)
	// Best-effort cleanup — ignore errors
	run("iptables", "-t", "mangle", "-D", "PREROUTING", "-i", tap, "-j", "MARK", "--set-mark", markStr)
	run("iptables", "-D", "FORWARD", "-i", tap, "-j", "ACCEPT")
	run("iptables", "-D", "FORWARD", "-i", tap, "!", "-d", "10.0.0.0/24", "-j", "DROP")
	run("iptables", "-D", "FORWARD", "-i", tap, "-d", "10.0.0.1/32", "-p", "tcp", "--dport", "8080", "-j", "ACCEPT")
	run("ip", "rule", "del", "fwmark", markStr, "lookup", markStr)
	run("ip", "route", "flush", "table", markStr)
	run("ip", "link", "del", tap)
}

// SetupParanoidMode adds iptables rules to block direct internet, allow only proxy.
func SetupParanoidMode(tap string) error {
	if err := run("iptables", "-I", "FORWARD", "-i", tap, "-d", "10.0.0.1/32", "-p", "tcp", "--dport", "8080", "-j", "ACCEPT"); err != nil {
		return err
	}
	return run("iptables", "-I", "FORWARD", "-i", tap, "!", "-d", "10.0.0.0/24", "-j", "DROP")
}

func defaultRoute() (uplink, gw string) {
	out, err := exec.Command("ip", "route").Output()
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "default") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "via" && i+1 < len(fields) {
					gw = fields[i+1]
				}
				if f == "dev" && i+1 < len(fields) {
					uplink = fields[i+1]
				}
			}
			return
		}
	}
	return "", ""
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// RunOutput runs a command and returns stdout.
func runOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(out)), err
}
