// Package config loads the OpsGaurdWeb management-plane server configuration.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	registrycfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/registry"
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
	// Registry 内嵌镜像仓库(OCI /v2)+ 页面传包构建(docker CLI)。
	Registry registrycfg.Config `yaml:"registry" json:"registry"`
	// IdP 让 OpsGaurd 自身作为 OIDC 身份提供者，其他系统可跳转过来认证。
	// nil 或 enabled=false = 不启用（默认）。详见 internal/idp。
	IdP *IdPConfig `yaml:"idp,omitempty" json:"idp,omitempty"`
}

// IdPConfig 是 OpsGaurd 作为 OIDC IdP 的配置（独立于 auth.sso，后者是作为 RP 对接上游）。
type IdPConfig struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	Issuer          string `yaml:"issuer" json:"issuer"`                    // 对外可达地址，须 https（localhost 例外便于开发）
	AccessTokenTTL  string `yaml:"access_token_ttl" json:"accessTokenTtl"`  // 默认 1h
	RefreshTokenTTL string `yaml:"refresh_token_ttl" json:"refreshTokenTtl"` // 默认 720h（30d）
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

// SSOOIDC OIDC 网关参数（授权码流程）。
type SSOOIDC struct {
	Issuer       string `yaml:"issuer" json:"issuer"`
	ClientID     string `yaml:"client_id" json:"clientId"`
	ClientSecret string `yaml:"client_secret" json:"-"`
	RedirectURL  string `yaml:"redirect_url" json:"redirectUrl"`
	// FrontendURL 回调成功后重定向的前端落地页（token 放 URL hash；默认 "/"）。
	FrontendURL string `yaml:"frontend_url" json:"frontendUrl"`
	// Scopes 额外 OIDC scopes（默认 profile email；openid 恒包含）。
	Scopes []string `yaml:"scopes" json:"scopes"`
	// DefaultRole SSO 新用户默认角色（admin|viewer，默认 viewer）。
	DefaultRole string `yaml:"default_role" json:"defaultRole"`
}

// StoreConfig holds the LevelDB data directory.
type StoreConfig struct {
	Path string `yaml:"path" json:"path"` // 数据目录，如 ./data
}

// ServerConfig holds HTTP listen settings.
type ServerConfig struct {
	Addr   string `yaml:"addr" json:"addr"`
	APIKey string `yaml:"api_key" json:"apiKey"` // 网关鉴权，空则不启用
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
	if c.Registry.Storage == "" {
		c.Registry.Storage = c.Store.Path + "/registry"
	}
	if c.Registry.Hostname == "" {
		c.Registry.Hostname = "registry.opsguard"
	}
	if c.Registry.MaxUploadMB <= 0 {
		c.Registry.MaxUploadMB = 500
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
	if c.IdP != nil && c.IdP.Enabled {
		if c.IdP.Issuer == "" {
			return fmt.Errorf("idp.issuer is required when idp.enabled is true")
		}
		// 生产强制 https；localhost 例外便于本地开发。
		if !isHTTPSorLocalhost(c.IdP.Issuer) {
			return fmt.Errorf("idp.issuer must be an https:// URL (got %q)", c.IdP.Issuer)
		}
	}
	return nil
}

// isHTTPSorLocalhost issuer 是否合规：https:// 或 http://localhost/http://127.0.0.1。
func isHTTPSorLocalhost(issuer string) bool {
	if issuer == "" {
		return false
	}
	if strings.HasPrefix(issuer, "https://") {
		return true
	}
	if strings.HasPrefix(issuer, "http://localhost") || strings.HasPrefix(issuer, "http://127.0.0.1") {
		return true
	}
	return false
}
