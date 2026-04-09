package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/AndrewBudd/boxcutter/concord/dispatcher/internal/task"
	"github.com/AndrewBudd/boxcutter/concord/dispatcher/internal/worker"
)

func main() {
	orchURL := flag.String("orchestrator", envOrDefault("BOXCUTTER_ORCHESTRATOR", "http://192.168.50.2:8801"), "orchestrator API URL")
	taskFile := flag.String("task", "", "task payload JSON file")
	vmName := flag.String("vm", "", "worker VM name (auto-generated if empty)")
	ramMIB := flag.Int("ram", 2048, "worker VM RAM in MiB")
	pollInterval := flag.Duration("poll", 5*time.Second, "status poll interval")
	timeout := flag.Duration("timeout", 30*time.Minute, "task execution timeout")
	keepVM := flag.Bool("keep", false, "don't destroy the VM after task completion")
	flag.Parse()

	if *taskFile == "" {
		fmt.Fprintf(os.Stderr, "Usage: concord-dispatcher -task <file.json> [-vm <name>] [-ram <mib>]\n")
		os.Exit(1)
	}

	// Load task
	data, err := os.ReadFile(*taskFile)
	if err != nil {
		log.Fatalf("Failed to read task file: %v", err)
	}
	var t task.Task
	if err := json.Unmarshal(data, &t); err != nil {
		log.Fatalf("Failed to parse task file: %v", err)
	}

	// Load agent installer
	agentScript, err := os.ReadFile("concord/agent/install.sh")
	if err != nil {
		// Try relative to binary location
		agentScript, err = os.ReadFile("/opt/concord/agent/install.sh")
		if err != nil {
			log.Fatalf("Failed to read agent install script: %v", err)
		}
	}

	client := worker.NewClient(*orchURL)

	// Phase 1: Create VM
	log.Printf("Creating worker VM...")
	name, err := client.CreateVM(*vmName, *ramMIB)
	if err != nil {
		log.Fatalf("Failed to create VM: %v", err)
	}
	log.Printf("Worker VM created: %s", name)

	// Ensure cleanup
	if !*keepVM {
		defer func() {
			log.Printf("Destroying worker VM %s...", name)
			if err := client.DestroyVM(name); err != nil {
				log.Printf("WARNING: failed to destroy VM %s: %v", name, err)
			}
		}()
	}

	// Phase 2: Deploy agent
	log.Printf("Deploying agent to %s...", name)
	if err := client.DeployAgent(name, agentScript); err != nil {
		log.Fatalf("Failed to deploy agent: %v", err)
	}
	log.Printf("Agent deployed")

	// Phase 3: Submit and execute task
	log.Printf("Submitting task %s (%d steps)...", t.ID, len(t.Steps))
	if err := client.SubmitTask(name, &t); err != nil {
		log.Fatalf("Failed to submit task: %v", err)
	}

	// Phase 4: Wait for completion
	log.Printf("Waiting for task completion (poll every %s, timeout %s)...", *pollInterval, *timeout)
	status, err := client.WaitForCompletion(name, *pollInterval, *timeout)
	if err != nil {
		// Get logs before failing
		logs, _ := client.GetLogs(name)
		log.Printf("Task logs:\n%s", logs)
		log.Fatalf("Task failed: %v", err)
	}

	// Phase 5: Retrieve results
	logs, _ := client.GetLogs(name)
	fmt.Println(logs)

	result, _ := json.MarshalIndent(status, "", "  ")
	fmt.Printf("\nTask result:\n%s\n", string(result))

	if status.Status != "completed" {
		os.Exit(1)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
