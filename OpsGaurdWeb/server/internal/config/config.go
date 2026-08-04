// Package config loads the OpsGaurdWeb management-plane server configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the management-plane server configuration.
type Config struct {
	Server ServerConfig `yaml:"server" json:"server"`
	// AINexus is the AI 排查网关 (AiNexus) upstream, integrated as a service.
	// The management plane proxies chat/排查 requests to it.
	AINexus AINexusConfig `yaml:"ainexus" json:"ainexus"`
	// Clusters registry: name -> management-plane reachable Worker base URL.
	// Each cluster is managed via its Worker's HTTP API + MCP endpoint.
	Clusters map[string]ClusterConfig `yaml:"clusters" json:"clusters"`
}

// ServerConfig holds HTTP listen settings.
type ServerConfig struct {
	Addr   string `yaml:"addr" json:"addr"`
	APIKey string `yaml:"api_key" json:"apiKey"` // 网关鉴权，空则不启用
}

// AINexusConfig describes how to reach the integrated AiNexus service.
type AINexusConfig struct {
	BaseURL string `yaml:"base_url" json:"baseUrl"` // e.g. http://localhost:8080
	APIKey  string `yaml:"api_key" json:"apiKey"`
	Enabled bool   `yaml:"enabled" json:"enabled"`
}

// ClusterConfig binds a cluster name to its Worker endpoints.
type ClusterConfig struct {
	Name      string `yaml:"name" json:"name"`
	WorkerURL string `yaml:"worker_url" json:"workerUrl"` // Worker HTTP API base
	MCPURL    string `yaml:"mcp_url" json:"mcpUrl"`       // Worker MCP endpoint (optional)
	Token     string `yaml:"token" json:"token"`          // Worker bearer token (optional)
	Desc      string `yaml:"desc" json:"desc,omitempty"`
}

// Load reads the config file and applies defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8090"
	}
}

// Validate checks semantic constraints.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	return nil
}
