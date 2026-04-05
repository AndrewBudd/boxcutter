// boxcutter is the VM-side CLI for interacting with the boxcutter orchestrator.
// It executes commands via SSH to the orchestrator's ForceCommand interface.
//
// This binary is distributed via the tools mount (squashfs on /dev/vdb)
// and symlinked to /usr/local/bin/boxcutter so it's on PATH.
//
// Usage:
//
//	boxcutter list
//	boxcutter new --name my-vm --clone owner/repo
//	boxcutter exec my-vm hostname
//	boxcutter tapegun activity
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const defaultHost = "orchestrator"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: boxcutter <command> [args...]")
		fmt.Fprintln(os.Stderr, "Run 'boxcutter help' for available commands.")
		os.Exit(1)
	}

	host := os.Getenv("BOXCUTTER_HOST")
	if host == "" {
		host = defaultHost
	}

	// Build the SSH command. The orchestrator's ForceCommand reads
	// SSH_ORIGINAL_COMMAND, so we pass all args as the remote command.
	command := strings.Join(os.Args[1:], " ")

	args := []string{
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		host,
		command,
	}

	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "boxcutter: %v\n", err)
		os.Exit(1)
	}

}
