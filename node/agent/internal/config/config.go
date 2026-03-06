package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Node         NodeConfig         `yaml:"node"`
	Tailscale    TailscaleConfig    `yaml:"tailscale"`
	SSH          SSHConfig          `yaml:"ssh"`
	GitHub       GitHubConfig       `yaml:"github"`
	TLS          TLSConfig          `yaml:"tls"`
	Storage      StorageConfig      `yaml:"storage"`
	VMDefaults   VMDefaults         `yaml:"vm_defaults"`
	Orchestrator OrchestratorConfig `yaml:"orchestrator"`
}

type OrchestratorConfig struct {
	URL string `yaml:"url"` // e.g., http://192.168.50.2:8801
}

type NodeConfig struct {
	Hostname string `yaml:"hostname"`
	BridgeIP string `yaml:"bridge_ip"` // This node's IP on the host bridge (e.g., 192.168.50.3)
}

type TailscaleConfig struct {
	NodeAuthkeyFile string `yaml:"node_authkey_file"`
	VMAuthkeyFile   string `yaml:"vm_authkey_file"`
}

type SSHConfig struct {
	PrivateKeyPath     string `yaml:"private_key_path"`
	PublicKeyPath      string `yaml:"public_key_path"`
	AuthorizedKeysPath string `yaml:"authorized_keys_path"`
}

type GitHubConfig struct {
	Enabled        bool    `yaml:"enabled"`
	AppID          int64   `yaml:"app_id"`
	InstallationIDs []int64 `yaml:"installation_ids"`
	PrivateKeyFile string  `yaml:"private_key_file"`
}

type TLSConfig struct {
	CACertPath string `yaml:"ca_cert_path"`
	CAKeyPath  string `yaml:"ca_key_path"`
}

type StorageConfig struct {
	GoldenLocalPath string `yaml:"golden_local_path"`
}

type VMDefaults struct {
	VCPU   int    `yaml:"vcpu"`
	RAMMIB int    `yaml:"ram_mib"`
	Disk   string `yaml:"disk"`
	DNS    string `yaml:"dns"`
	Mode   string `yaml:"mode"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		Node: NodeConfig{Hostname: "boxcutter"},
		SSH: SSHConfig{
			PrivateKeyPath:     "/etc/boxcutter/secrets/node-ssh.key",
			PublicKeyPath:      "/etc/boxcutter/secrets/node-ssh.pub",
			AuthorizedKeysPath: "/etc/boxcutter/secrets/authorized-keys",
		},
		Tailscale: TailscaleConfig{
			NodeAuthkeyFile: "/etc/boxcutter/secrets/tailscale-node-authkey",
			VMAuthkeyFile:   "/etc/boxcutter/secrets/tailscale-vm-authkey",
		},
		TLS: TLSConfig{
			CACertPath: "/etc/boxcutter/secrets/ca.crt",
			CAKeyPath:  "/etc/boxcutter/secrets/ca.key",
		},
		Storage: StorageConfig{
			GoldenLocalPath: "/var/lib/boxcutter/golden/rootfs.ext4",
		},
		VMDefaults: VMDefaults{
			VCPU:   4,
			RAMMIB: 8192,
			Disk:   "50G",
			DNS:    "8.8.8.8",
			Mode:   "normal",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}

// ReadSecret reads a secret file, trimming whitespace.
func ReadSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
