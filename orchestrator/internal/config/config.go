package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Node      NodeConfig      `yaml:"node"`
	Tailscale TailscaleConfig `yaml:"tailscale"`
	SSH       SSHConfig       `yaml:"ssh"`
	DB        DBConfig        `yaml:"db"`
}

type NodeConfig struct {
	Hostname string `yaml:"hostname"`
}

type TailscaleConfig struct {
	NodeAuthkeyFile string `yaml:"node_authkey_file"`
	VMAuthkeyFile   string `yaml:"vm_authkey_file"`
}

type SSHConfig struct {
	AuthorizedKeysPath string `yaml:"authorized_keys_path"`
}

type DBConfig struct {
	Path string `yaml:"path"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		Node: NodeConfig{Hostname: "boxcutter"},
		SSH: SSHConfig{
			AuthorizedKeysPath: "/etc/boxcutter/secrets/authorized-keys",
		},
		DB: DBConfig{
			Path: "/var/lib/boxcutter/orchestrator.db",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}
