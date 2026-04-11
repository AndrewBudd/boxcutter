package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
)

func main() {
	if err := runDashboard(); err != nil {
		log.Fatalf("Dashboard error: %v", err)
	}
}

func runDashboard() error {
	fmt.Println("Boxcutter Dashboard")
	fmt.Println("===================")
	fmt.Println()

	// Run list command
	fmt.Println("VM List:")
	fmt.Println("-------------------")
	if err := runSSHCommand("list"); err != nil {
		log.Printf("Failed to run list command: %v", err)
	}

	fmt.Println()
	fmt.Println("Node Status:")
	fmt.Println("-------------------")
	if err := runSSHCommand("nodes"); err != nil {
		log.Printf("Failed to run nodes command: %v", err)
	}

	fmt.Println()
	fmt.Println("Cluster Status:")
	fmt.Println("-------------------")
	if err := runSSHCommand("status"); err != nil {
		log.Printf("Failed to run status command: %v", err)
	}

	fmt.Println()
	fmt.Print("Press Enter to exit...")
	fmt.Scanln()
	return nil
}

func runSSHCommand(command string) error {
	sshCommand := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"boxcutter@192.168.50.2",
		command,
	}

	cmd := exec.Command("ssh", sshCommand[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	fmt.Printf("Running: ssh boxcutter@192.168.50.2 %s\n", command)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("command execution failed: %w", err)
	}
	
	return nil
}