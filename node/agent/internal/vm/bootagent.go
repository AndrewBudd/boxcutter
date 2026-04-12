package vm

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// BootAgent handles VM self-configuration at creation time.
// It writes agent config and prepares the tools mount inside the rootfs.
type BootAgent struct {
	MountPoint  string          // Already-mounted rootfs path
	AgentConfig json.RawMessage // Agent config from orchestrator
}

// Setup writes the tapegun config and prepares the environment.
// Called from prepareRootfsForQEMU while the rootfs is mounted.
func (ba *BootAgent) Setup() error {
	if len(ba.AgentConfig) == 0 {
		log.Printf("BootAgent: no agent config, skipping")
		return nil
	}

	configDir := filepath.Join(ba.MountPoint, "home", "dev", ".tapegun")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("creating tapegun config dir: %w", err)
	}

	// Map agent_config JSON to tapegun config variables
	var configMap map[string]interface{}
	var lines []string
	if err := json.Unmarshal(ba.AgentConfig, &configMap); err == nil {
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

	if len(lines) == 0 {
		log.Printf("BootAgent: agent config parsed but no recognized fields")
		return nil
	}

	configPath := filepath.Join(configDir, "config")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing tapegun config: %w", err)
	}
	os.Chown(configPath, 1000, 1000)
	os.Chown(configDir, 1000, 1000)

	log.Printf("BootAgent: wrote %d vars to %s", len(lines), configPath)
	return nil
}
