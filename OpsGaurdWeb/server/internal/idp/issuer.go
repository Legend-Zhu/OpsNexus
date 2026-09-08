package idp

import (
	"fmt"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// Config 是 IdP（OpsGaurd 作为 OIDC 身份提供者）的配置。
type Config struct {
	Enabled         bool   `yaml:"enabled" json:"enabled"`
	Issuer          string `yaml:"issuer" json:"issuer"`                     // 对外可达地址，须 https（localhost 例外）
	AccessTokenTTL  string `yaml:"access_token_ttl" json:"accessTokenTtl"`   // 默认 1h
	RefreshTokenTTL string `yaml:"refresh_token_ttl" json:"refreshTokenTtl"` // 默认 720h（30d）
}

// DefaultTTLs 提供未配置时的默认有效期。
const (
	defaultAccessTokenTTL  = 1 * time.Hour
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	defaultAuthCodeTTL     = 10 * time.Minute
	defaultIDPSessionTTL   = 7 * 24 * time.Hour
)

// Service 是 IdP 核心服务，聚合签名密钥、client 管理、token 签发与会话。
// 它由路由层（router.go）在 IdP 启用时装配，端点方法直接挂在 *Service 上。
type Service struct {
	cfg     *Config
	st      *store.Store
	keys    *SigningKeys
	clients *ClientService

	// 解析后的 TTL（构造时算好，避免每请求解析）。
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

// NewService 构造 IdP 服务：加载签名密钥、初始化 client 服务、解析 TTL。
func NewService(cfg *Config, st *store.Store) (*Service, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("idp not enabled")
	}
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("idp.issuer is required")
	}
	keys, err := LoadSigningKeys(st)
	if err != nil {
		return nil, fmt.Errorf("load signing keys: %w", err)
	}
	return &Service{
		cfg:             cfg,
		st:              st,
		keys:            keys,
		clients:         NewClientService(st),
		accessTokenTTL:  parseTTL(cfg.AccessTokenTTL, defaultAccessTokenTTL),
		refreshTokenTTL: parseTTL(cfg.RefreshTokenTTL, defaultRefreshTokenTTL),
	}, nil
}

// Issuer 返回配置的 issuer（discovery / JWT iss claim 用）。
func (s *Service) Issuer() string { return s.cfg.Issuer }

// Clients 返回 client 管理子服务（admin API 用）。
func (s *Service) Clients() *ClientService { return s.clients }

// Keys 返回签名密钥管理器（JWKS 端点 / 测试用）。
func (s *Service) Keys() *SigningKeys { return s.keys }

// parseTTL 解析如 "1h"/"720h" 的字符串，空或非法时回退到 def。
func parseTTL(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
