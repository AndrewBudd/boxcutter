package vm

import (
	"encoding/json"
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

// localKernel returns kernel and initrd paths local to the VM directory.
// On first launch, it copies the host kernel/initrd into vmDir so the kernel
// travels with the VM during migration. On subsequent launches (including on
// a migration target), the already-present local copies are used.
func localKernel(vmDir string) (kernel string, initrd string, err error) {
	localKernelPath := filepath.Join(vmDir, "vmlinuz")
	localInitrdPath := filepath.Join(vmDir, "initrd.img")

	// If local kernel already exists (e.g. migrated VM), use it directly.
	if _, statErr := os.Stat(localKernelPath); statErr == nil {
		kernel = localKernelPath
		if _, statErr := os.Stat(localInitrdPath); statErr == nil {
			initrd = localInitrdPath
		}
		return kernel, initrd, nil
	}

	// First launch on this node: find the host kernel and copy it into vmDir.
	srcKernel, srcInitrd := findQEMUKernel()
	if srcKernel == "" {
		return "", "", fmt.Errorf("no kernel found for QEMU VM (checked /var/lib/boxcutter/kernel/vmlinuz-qemu and /boot/vmlinuz-*)")
	}

	if err := copyFile(srcKernel, localKernelPath); err != nil {
		return "", "", fmt.Errorf("copy kernel to VM dir: %w", err)
	}
	log.Printf("Copied kernel %s -> %s", srcKernel, localKernelPath)

	if srcInitrd != "" {
		if err := copyFile(srcInitrd, localInitrdPath); err != nil {
			return "", "", fmt.Errorf("copy initrd to VM dir: %w", err)
		}
		log.Printf("Copied initrd %s -> %s", srcInitrd, localInitrdPath)
		return localKernelPath, localInitrdPath, nil
	}

	return localKernelPath, "", nil
}

// launchQEMU starts a QEMU VM with direct kernel boot.
// Returns the PID of the QEMU process.
func launchQEMU(vmDir string, st *VMState) (int, error) {
	rootfs := RootfsPath(vmDir)
	logPath := filepath.Join(vmDir, "console.log")
	pidFile := filepath.Join(vmDir, "qemu.pid")

	kernel, initrd, err := localKernel(vmDir)
	if err != nil {
		return 0, err
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
		"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio,cache=writeback", rootfs, DiskFormat(vmDir)),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", st.TAP),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", st.MAC),
		"-serial", fmt.Sprintf("file:%s", logPath),
		"-display", "none",
		"-no-reboot",
		"-pidfile", pidFile,
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", filepath.Join(vmDir, "qmp.sock")),
		"-daemonize",
	}

	if initrd != "" {
		args = append(args, "-initrd", initrd)
	}

	// Attach tools image (read-only squashfs with boxcutter CLI)
	if _, err := os.Stat(toolsImagePath); err != nil {
		return 0, fmt.Errorf("tools.img not found at %s: %w (QEMU VMs require tools.img for tapegun daemon)", toolsImagePath, err)
	}
	args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", toolsImagePath))
	log.Printf("QEMU VM %s: attached tools.img as /dev/vdb", st.Name)

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

	// Mount rootfs temporarily to inject files.
	// QCOW2 requires qemu-nbd; raw ext4 uses loop mount.
	mountPoint := filepath.Join(vmDir, "mnt")
	os.MkdirAll(mountPoint, 0755)

	format := DiskFormat(vmDir)
	var nbdDev string
	if format == "qcow2" {
		// Use qemu-nbd to expose QCOW2 as block device
		exec.Command("modprobe", "nbd", "max_part=8").Run()
		// Find a free nbd device
		for i := 0; i < 16; i++ {
			dev := fmt.Sprintf("/dev/nbd%d", i)
			if out, err := exec.Command("qemu-nbd", "-c", dev, rootfs).CombinedOutput(); err == nil {
				nbdDev = dev
				break
			} else {
				_ = out // device busy, try next
			}
		}
		if nbdDev == "" {
			log.Printf("Warning: could not find free nbd device for QEMU prep")
			return
		}
		time.Sleep(500 * time.Millisecond) // let kernel discover partition
		if out, err := exec.Command("mount", nbdDev, mountPoint).CombinedOutput(); err != nil {
			log.Printf("Warning: could not mount qcow2 via nbd for QEMU prep: %s: %v", string(out), err)
			exec.Command("qemu-nbd", "-d", nbdDev).Run()
			return
		}
	} else {
		if out, err := exec.Command("mount", "-o", "loop", rootfs, mountPoint).CombinedOutput(); err != nil {
			log.Printf("Warning: could not mount rootfs for QEMU prep: %s: %v", string(out), err)
			return
		}
	}
	defer func() {
		// Sync filesystem before unmount to ensure all writes (including
		// writeAgentConfig) are flushed to the QCOW2 backing store.
		exec.Command("sync").Run()
		exec.Command("umount", mountPoint).Run()
		if nbdDev != "" {
			// Give the kernel a moment to release the block device
			time.Sleep(200 * time.Millisecond)
			exec.Command("qemu-nbd", "-d", nbdDev).Run()
		}
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

	// Auto-load KVM modules for nested virtualization
	os.WriteFile(filepath.Join(modulesDir, "kvm.conf"),
		[]byte("kvm\nkvm_amd\nkvm_intel\n"), 0644)

	// Set up boxcutter tools mount (squashfs on /dev/vdb → /opt/boxcutter/tools).
	// Firecracker golden images get this from the Dockerfile, but QEMU VMs need
	// the mount units and symlinks injected during rootfs prep.
	toolsMountPoint := filepath.Join(mountPoint, "opt", "boxcutter", "tools")
	os.MkdirAll(toolsMountPoint, 0755)

	// Install systemd mount + automount units for /dev/vdb
	os.WriteFile(filepath.Join(sysDir, "opt-boxcutter-tools.mount"), []byte(`[Unit]
Description=Boxcutter tools squashfs (/dev/vdb)
DefaultDependencies=no
After=dev-vdb.device

[Mount]
What=/dev/vdb
Where=/opt/boxcutter/tools
Type=squashfs
Options=ro

[Install]
WantedBy=local-fs.target
`), 0644)
	os.WriteFile(filepath.Join(sysDir, "opt-boxcutter-tools.automount"), []byte(`[Unit]
Description=Automount boxcutter tools squashfs

[Automount]
Where=/opt/boxcutter/tools
TimeoutIdleSec=0

[Install]
WantedBy=local-fs.target
`), 0644)

	// Enable automount in local-fs.target
	localFSWants := filepath.Join(sysDir, "local-fs.target.wants")
	os.MkdirAll(localFSWants, 0755)
	os.Symlink("/etc/systemd/system/opt-boxcutter-tools.automount",
		filepath.Join(localFSWants, "opt-boxcutter-tools.automount"))

	// Symlink binaries and tapegun service (same as golden Dockerfile)
	usrLocalBin := filepath.Join(mountPoint, "usr", "local", "bin")
	os.MkdirAll(usrLocalBin, 0755)
	os.Symlink("/opt/boxcutter/tools/bin/boxcutter", filepath.Join(usrLocalBin, "boxcutter"))
	os.Symlink("/opt/boxcutter/tools/bin/boxcutter-tapegun", filepath.Join(usrLocalBin, "boxcutter-tapegun"))
	os.Symlink("/opt/boxcutter/tools/etc/systemd/boxcutter-tapegun.service",
		filepath.Join(sysDir, "boxcutter-tapegun.service"))
	multiUserWants := filepath.Join(sysDir, "multi-user.target.wants")
	os.MkdirAll(multiUserWants, 0755)
	os.Symlink("/etc/systemd/system/boxcutter-tapegun.service",
		filepath.Join(multiUserWants, "boxcutter-tapegun.service"))

	// Create Claude Code plugin directory for tapegun-guest plugin
	claudePluginDir := filepath.Join(mountPoint, "home", "dev", ".claude", "plugins")
	os.MkdirAll(claudePluginDir, 0755)
	chroot("chown -R 1000:1000 /home/dev/.claude")

	log.Printf("QEMU VM %s: installed tools mount + tapegun service", st.Name)

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

	// Write agent configuration for tapegun (uses already-mounted rootfs)
	writeAgentConfig(mountPoint, st.AgentConfig)

	log.Printf("QEMU VM %s: rootfs prepared (modules, iptables-legacy, docker config)", st.Name)
}

// writeAgentConfig writes tapegun config to an already-mounted VM rootfs.
// Maps agent_config JSON fields to tapegun config variable names.
// Called from prepareQEMURootfs while the rootfs is mounted.
func writeAgentConfig(mountPoint string, agentConfig json.RawMessage) {
	log.Printf("writeAgentConfig: mountPoint=%s agentConfig=%d bytes", mountPoint, len(agentConfig))
	if len(agentConfig) == 0 {
		log.Printf("writeAgentConfig: empty agent config, skipping")
		return
	}

	configDir := filepath.Join(mountPoint, "home", "dev", ".tapegun")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log.Printf("Warning: could not create tapegun config dir: %v", err)
		return
	}
	configPath := filepath.Join(configDir, "config")

	var configMap map[string]interface{}
	var lines []string
	if err := json.Unmarshal(agentConfig, &configMap); err == nil {
		if tool, ok := configMap["tool"].(string); ok {
			lines = append(lines, fmt.Sprintf("AGENT_TOOL=%s", tool))
		}
		if inf, ok := configMap["inference"].(map[string]interface{}); ok {
			if model, ok := inf["model"].(string); ok {
				lines = append(lines, fmt.Sprintf("AGENT_MODEL=%s", model))
			}
			if provider, ok := inf["provider"].(string); ok {
				lines = append(lines, fmt.Sprintf("INFERENCE_PROVIDER=%s", provider))
			}
			if endpoint, ok := inf["endpoint"].(string); ok {
				lines = append(lines, fmt.Sprintf("INFERENCE_ENDPOINT=%s", endpoint))
			}
			if apiKey, ok := inf["api_key"].(string); ok {
				lines = append(lines, fmt.Sprintf("INFERENCE_API_KEY=%s", apiKey))
			}
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		log.Printf("Warning: could not write tapegun config: %v", err)
		return
	}
	os.Chown(configPath, 1000, 1000)
	os.Chown(configDir, 1000, 1000)
	log.Printf("QEMU VM: wrote tapegun config (%d vars) to %s", len(lines), configPath)
}

// kernelVersion returns the running kernel version.
func kernelVersion() string {
	data, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
