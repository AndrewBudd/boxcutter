package vm

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Default kernel path — the node VM's own kernel works for QEMU guests.
	// This is a full Ubuntu kernel with all modules (Docker, nftables, etc.).
	defaultQEMUKernel = "/boot/vmlinuz"
	// Initrd for the node VM's kernel (needed for module loading).
	defaultQEMUInitrd = "/boot/initrd.img"
)

// findQEMUKernel finds the best kernel to use for QEMU VMs.
// Uses the RUNNING kernel (uname -r) to ensure kernel and modules match.
// A dedicated QEMU kernel at /var/lib/boxcutter/kernel/vmlinuz-qemu takes priority.
func findQEMUKernel() (kernel string, initrd string) {
	// Check for dedicated QEMU kernel
	dedicated := "/var/lib/boxcutter/kernel/vmlinuz-qemu"
	if _, err := os.Stat(dedicated); err == nil {
		initrdDedicated := "/var/lib/boxcutter/kernel/initrd-qemu.img"
		if _, err := os.Stat(initrdDedicated); err == nil {
			return dedicated, initrdDedicated
		}
		return dedicated, ""
	}

	// Use the RUNNING kernel — not the /boot/vmlinuz symlink which may point
	// to a newer installed-but-not-booted kernel. The modules we copy into
	// the guest rootfs come from uname -r, so the kernel must match.
	kver := kernelVersion()
	if kver == "" {
		return "", ""
	}

	kernel = fmt.Sprintf("/boot/vmlinuz-%s", kver)
	if _, err := os.Stat(kernel); err != nil {
		return "", ""
	}

	initrdPath := fmt.Sprintf("/boot/initrd.img-%s", kver)
	if _, err := os.Stat(initrdPath); err == nil {
		return kernel, initrdPath
	}

	return kernel, ""
}

// launchQEMU starts a QEMU VM with direct kernel boot.
// Returns the PID of the QEMU process.
func launchQEMU(vmDir string, st *VMState) (int, error) {
	rootfs := RootfsPath(vmDir)
	logPath := filepath.Join(vmDir, "console.log")
	pidFile := filepath.Join(vmDir, "qemu.pid")

	kernel, initrd := findQEMUKernel()
	if kernel == "" {
		return 0, fmt.Errorf("no kernel found for QEMU VM (checked /var/lib/boxcutter/kernel/vmlinuz-qemu and /boot/vmlinuz-*)")
	}

	// Boot args: direct kernel boot with network config via kernel ip= parameter.
	// net.ifnames=0 biosdevname=0: force classic eth0 naming (Ubuntu uses predictable names).
	bootArgs := fmt.Sprintf(
		"console=ttyS0 root=/dev/vda rw init=/sbin/init "+
			"net.ifnames=0 biosdevname=0 "+
			"ip=10.0.0.2::10.0.0.1:255.255.255.252:%s:eth0:off:8.8.8.8",
		st.Name)

	args := []string{
		"-enable-kvm",
		"-cpu", "host",
		"-smp", fmt.Sprintf("%d", st.VCPU),
		"-m", fmt.Sprintf("%d", st.RAMMIB),
		"-kernel", kernel,
		"-append", bootArgs,
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,cache=writeback", rootfs),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", st.TAP),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", st.MAC),
		"-serial", fmt.Sprintf("file:%s", logPath),
		"-display", "none",
		"-no-reboot",
		"-pidfile", pidFile,
		"-daemonize",
	}

	if initrd != "" {
		args = append(args, "-initrd", initrd)
	}

	log.Printf("Launching QEMU VM %s: kernel=%s initrd=%s vcpu=%d ram=%dMiB",
		st.Name, kernel, initrd, st.VCPU, st.RAMMIB)

	cmd := exec.Command("qemu-system-x86_64", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("qemu-system-x86_64: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// QEMU -daemonize writes PID file. Wait briefly for it to appear.
	var pid int
	for i := 0; i < 20; i++ {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
			if pid > 0 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pid == 0 {
		return 0, fmt.Errorf("QEMU started but PID file not found at %s", pidFile)
	}

	log.Printf("QEMU VM %s started (PID %d)", st.Name, pid)
	return pid, nil
}

// prepareRootfsForQEMU does QEMU-specific rootfs preparation.
// - Copies kernel modules from the host so Docker works inside the VM.
// - Configures Tailscale for kernel networking (not userspace).
// - Configures iptables to use legacy backend (not nftables).
func (m *Manager) prepareRootfsForQEMU(st *VMState) {
	vmDir := VMDir(st.Name)
	rootfs := RootfsPath(vmDir)

	// Mount rootfs temporarily to inject files
	mountPoint := filepath.Join(vmDir, "mnt")
	os.MkdirAll(mountPoint, 0755)

	if out, err := exec.Command("mount", "-o", "loop", rootfs, mountPoint).CombinedOutput(); err != nil {
		log.Printf("Warning: could not mount rootfs for QEMU prep: %s: %v", string(out), err)
		return
	}
	defer func() {
		exec.Command("umount", mountPoint).Run()
		os.Remove(mountPoint)
	}()

	// Copy kernel modules from the host node VM
	kver := kernelVersion()
	if kver != "" {
		srcModules := fmt.Sprintf("/lib/modules/%s", kver)
		dstModules := filepath.Join(mountPoint, "lib", "modules", kver)
		if _, err := os.Stat(srcModules); err == nil {
			os.MkdirAll(filepath.Dir(dstModules), 0755)
			exec.Command("cp", "-a", srcModules, dstModules).Run()
			log.Printf("QEMU VM %s: copied kernel modules %s", st.Name, kver)
		}
	}

	// Configure iptables to use legacy backend (not nftables)
	chroot := func(cmd string) {
		exec.Command("chroot", mountPoint, "bash", "-c", cmd).Run()
	}
	chroot("update-alternatives --set iptables /usr/sbin/iptables-legacy 2>/dev/null")
	chroot("update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null")

	// Mask serial-getty on ttyS0 — QEMU serial is redirected to a log file,
	// so there's no real tty device. Without this, systemd waits 90s for
	// dev-ttyS0.device to appear, spamming the console log.
	sysDir := filepath.Join(mountPoint, "etc", "systemd", "system")
	os.MkdirAll(sysDir, 0755)
	os.Symlink("/dev/null", filepath.Join(sysDir, "serial-getty@ttyS0.service"))

	// Create /etc/modules-load.d to auto-load Docker-required modules on boot
	// kmod is installed in the golden image (Dockerfile)
	modulesDir := filepath.Join(mountPoint, "etc", "modules-load.d")
	os.MkdirAll(modulesDir, 0755)
	os.WriteFile(filepath.Join(modulesDir, "docker.conf"),
		[]byte("overlay\nbridge\nbr_netfilter\nveth\n"+
			"ip_tables\nip6_tables\n"+
			"iptable_filter\nip6table_filter\n"+
			"iptable_nat\nip6table_nat\n"+
			"xt_conntrack\nxt_addrtype\nxt_MASQUERADE\n"+
			"nf_nat\nnf_conntrack\n"), 0644)

	// Configure Tailscale for kernel networking (Firecracker uses userspace)
	tailscaleDefaults := filepath.Join(mountPoint, "etc", "default", "tailscaled")
	os.WriteFile(tailscaleDefaults, []byte("PORT=0\nFLAGS=\n"), 0644)

	// Add dev user to docker group (created when Docker is installed at first boot).
	// Also create a firstboot script that installs Docker + adds the user.
	firstbootDir := filepath.Join(mountPoint, "etc", "boxcutter")
	os.MkdirAll(firstbootDir, 0755)
	os.WriteFile(filepath.Join(firstbootDir, "qemu-firstboot.sh"), []byte(`#!/bin/bash
# QEMU VM first-boot setup: install Docker, configure for dev user
set -e
MARKER=/etc/boxcutter/.qemu-firstboot-done
[ -f "$MARKER" ] && exit 0

# Install Docker if not present
if ! command -v docker &>/dev/null; then
    curl -fsSL https://get.docker.com | sh 2>/dev/null
fi

# Add dev user to docker group
usermod -aG docker dev 2>/dev/null || true

# Set iptables to legacy (Docker needs this in nested KVM)
update-alternatives --set iptables /usr/sbin/iptables-legacy 2>/dev/null || true
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null || true

# Enable + start Docker
systemctl enable docker 2>/dev/null || true
systemctl start docker 2>/dev/null || true

touch "$MARKER"
`), 0755)

	// Create systemd service for firstboot
	systemdDir := filepath.Join(mountPoint, "etc", "systemd", "system")
	os.WriteFile(filepath.Join(systemdDir, "boxcutter-qemu-firstboot.service"), []byte(`[Unit]
Description=Boxcutter QEMU first-boot setup (Docker install)
After=network-online.target
Wants=network-online.target
ConditionPathExists=!/etc/boxcutter/.qemu-firstboot-done

[Service]
Type=oneshot
ExecStart=/etc/boxcutter/qemu-firstboot.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`), 0644)

	// Enable the service
	wantsDir := filepath.Join(systemdDir, "multi-user.target.wants")
	os.MkdirAll(wantsDir, 0755)
	os.Symlink("/etc/systemd/system/boxcutter-qemu-firstboot.service",
		filepath.Join(wantsDir, "boxcutter-qemu-firstboot.service"))

	log.Printf("QEMU VM %s: rootfs prepared (modules, iptables-legacy, docker config)", st.Name)
}

// kernelVersion returns the running kernel version.
func kernelVersion() string {
	data, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
