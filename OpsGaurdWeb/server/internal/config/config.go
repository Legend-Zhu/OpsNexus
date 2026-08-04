// Package config loads the OpsGaurdWeb management-plane server configuration.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
)

// Config is the management-plane server configuration.
type Config struct {
	Server ServerConfig `yaml:"server" json:"server"`
	// Auth 认证：本地用户 token 签名密钥 + SSO(OIDC) 配置（决策⑤）。
	Auth AuthConfig `yaml:"auth" json:"auth"`
	// Store 持久化配置（LevelDB 数据目录）。
	Store StoreConfig `yaml:"store" json:"store"`
	// AINexus embeds the AiNexus AI 网关 into this process (vendored under
	// internal/ainexus). No standalone service, no separate port — providers,
	// tools, MCP clients and the ReAct agent run in-process; the /ainexus/*
	// native endpoints are mounted on the management router.
	AINexus ainexuscfg.Config `yaml:"ainexus" json:"ainexus"`
	// Clusters registry: name -> management-plane reachable Worker base URL.
	// Each cluster is managed via its Worker's HTTP API + MCP endpoint.
	Clusters map[string]ClusterConfig `yaml:"clusters" json:"clusters"`
}

// AuthConfig 认证配置。
type AuthConfig struct {
	// TokenSecret 本地 token 签名密钥（生产经环境变量注入）。
	TokenSecret string `yaml:"token_secret" json:"-"`
	// TokenTTL 会话有效期（如 24h）。
	TokenTTL string `yaml:"token_ttl" json:"tokenTtl"`
	// SSO OIDC 配置（未配置 = 仅本地用户 fallback）。
	SSO *SSOConfig `yaml:"sso,omitempty" json:"sso,omitempty"`
}

// SSOConfig 企业 SSO（OIDC）配置。
type SSOConfig struct {
	OIDC *SSOOIDC `yaml:"oidc,omitempty" json:"oidc,omitempty"`
}

// SSOOIDC OIDC 网关参数。
type SSOOIDC struct {
	Issuer       string `yaml:"issuer" json:"issuer"`
	ClientID     string `yaml:"client_id" json:"clientId"`
	ClientSecret string `yaml:"client_secret" json:"-"`
	RedirectURL  string `yaml:"redirect_url" json:"redirectUrl"`
}

// StoreConfig holds the LevelDB data directory.
type StoreConfig struct {
	Path string `yaml:"path" json:"path"` // 数据目录，如 ./data
}

// ServerConfig holds HTTP listen settings.
type ServerConfig struct {
	Addr   string `yaml:"addr" json:"addr"`
	APIKey string `yaml:"api_key" json:"apiKey"` // 网关鉴权，空则不启用
	// IngestToken 校验 Worker webhook 推送（POST /api/v1/ingest/events?token=）。
	IngestToken string `yaml:"ingest_token" json:"ingestToken"`
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
	if c.Store.Path == "" {
		c.Store.Path = "./data"
	}
}

// Validate checks semantic constraints.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server.addr is required")
	}
	if c.AINexus.Enabled {
		if err := c.AINexus.Validate(); err != nil {
			return fmt.Errorf("ainexus: %w", err)
		}
	}
	return nil
}
