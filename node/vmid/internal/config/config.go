package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   ListenConfig        `yaml:"listen"`
	JWT      JWTConfig           `yaml:"jwt"`
	GitHub   *GitHubConfig       `yaml:"github,omitempty"`
	Policies []Policy            `yaml:"policies,omitempty"`
	Metadata MetadataFilesConfig `yaml:"metadata"`
	Log      LogConfig           `yaml:"log"`
}

type ListenConfig struct {
	VMAddr      string `yaml:"vm_addr"`
	VMPort      int    `yaml:"vm_port"`
	AdminSocket string `yaml:"admin_socket"`
}

type MetadataFilesConfig struct {
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys"`
	CACertPath        string   `yaml:"ca_cert_path"`
}

type JWTConfig struct {
	TTL     time.Duration `yaml:"ttl"`
	KeyPath string        `yaml:"key_path"`
}

type GitHubConfig struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	RepoCacheTTL   time.Duration `yaml:"repo_cache_ttl"`
}

type Policy struct {
	Match  PolicyMatch  `yaml:"match"`
	GitHub *PolicyGitHub `yaml:"github,omitempty"`
}

type PolicyMatch struct {
	Labels map[string]string `yaml:"labels"`
}

type PolicyGitHub struct {
	Repositories []string          `yaml:"repositories"`
	Permissions  map[string]string `yaml:"permissions"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func (c *JWTConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw struct {
		TTL     string `yaml:"ttl"`
		KeyPath string `yaml:"key_path"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	c.KeyPath = raw.KeyPath
	if raw.TTL != "" {
		d, err := time.ParseDuration(raw.TTL)
		if err != nil {
			return fmt.Errorf("invalid jwt.ttl: %w", err)
		}
		c.TTL = d
	}
	return nil
}

func (c *GitHubConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw struct {
		AppID          int64  `yaml:"app_id"`
		InstallationID int64  `yaml:"installation_id"`
		PrivateKeyPath string `yaml:"private_key_path"`
		RepoCacheTTL   string `yaml:"repo_cache_ttl"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	c.AppID = raw.AppID
	c.InstallationID = raw.InstallationID
	c.PrivateKeyPath = raw.PrivateKeyPath
	if raw.RepoCacheTTL != "" {
		d, err := time.ParseDuration(raw.RepoCacheTTL)
		if err != nil {
			return fmt.Errorf("invalid github.repo_cache_ttl: %w", err)
		}
		c.RepoCacheTTL = d
	} else {
		c.RepoCacheTTL = 15 * time.Minute
	}
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		Listen: ListenConfig{
			VMAddr:      "169.254.169.254",
			VMPort:      80,
			AdminSocket: "/run/vmid/admin.sock",
		},
		Metadata: MetadataFilesConfig{
			SSHAuthorizedKeys: []string{
				"/etc/boxcutter/secrets/cluster-ssh.key.pub",
				"/etc/boxcutter/secrets/authorized-keys",
			},
			CACertPath: "/etc/boxcutter/secrets/ca.crt",
		},
		JWT: JWTConfig{
			TTL: 10 * time.Minute,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return cfg, nil
}
