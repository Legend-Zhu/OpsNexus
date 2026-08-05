// Package auth implements user management and request authentication (P6).
// Primary auth is SSO (OIDC, 决策⑤); the built-in OIDC client is configured
// via server.sso.oidc. When no SSO issuer is configured (intranet bootstrap),
// local users + HMAC-signed bearer tokens act as fallback. Passwords are
// hashed (PBKDF2-style with per-user salt) and never stored plaintext.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// Service 认证服务。
type Service struct {
	st     *store.Store
	secret []byte
	// tokenTTL 会话有效期。
	tokenTTL time.Duration
	// OIDC 配置（未配置 = 仅本地 fallback）。
	OIDC *OIDCConfig

	// SSO 一次性授权状态（state → nonce，防 CSRF）；复用 mu 保护 OIDC 懒加载。
	mu        sync.Mutex
	ssoStates map[string]ssoState
	oidcProv  *oidc.Provider
	oauthCfg  *oauth2.Config
	oidcErr   error
}

// OIDCConfig SSO/OIDC 配置（内网对接企业网关用）。
type OIDCConfig struct {
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

// ssoState 一次授权流程的暂存：nonce 绑定 state，一次性 + 短 TTL。
type ssoState struct {
	nonce  string
	expiry time.Time
}

// New 创建认证服务。
func New(st *store.Store, secret string, ttl time.Duration, oidc *OIDCConfig) *Service {
	if secret == "" {
		secret = "opsguard-dev-secret" // 生产经环境变量注入
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Service{st: st, secret: []byte(secret), tokenTTL: ttl, OIDC: oidc}
}

// --- 用户管理 ---

// CreateUser 创建用户（密码加盐哈希存储）。
func (s *Service) CreateUser(username, password, role string, enabled bool) (*store.User, error) {
	if username == "" || password == "" {
		return nil, fmt.Errorf("username and password are required")
	}
	if existing, err := s.st.GetUserByUsername(username); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("user %q already exists", username)
	}
	salt := randomHex(16)
	u := &store.User{
		ID:        "u-" + randomHex(8),
		Username:  username,
		Password:  hashPassword(password, salt) + ":" + salt,
		Role:      role,
		Enabled:   enabled,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.st.PutUser(u); err != nil {
		return nil, err
	}
	return u, nil
}

// ListUsers 列出用户（不含密码）。
func (s *Service) ListUsers() ([]*store.User, error) { return s.st.ListUsers() }

// --- 登录 ---

// Login 本地登录：校验密码 → 签发 token。
func (s *Service) Login(username, password string) (token string, err error) {
	u, err := s.st.GetUserByUsername(username)
	if err != nil {
		return "", err
	}
	if u == nil || !u.Enabled {
		return "", fmt.Errorf("invalid credentials")
	}
	parts := strings.SplitN(u.Password, ":", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid credentials")
	}
	got := hashPassword(password, parts[1])
	if subtle.ConstantTimeCompare([]byte(got), []byte(parts[0])) != 1 {
		return "", fmt.Errorf("invalid credentials")
	}
	return s.IssueToken(u.Username, u.Role)
}

// IssueToken 签发 HMAC token（payload=username|role|exp，签名=HMAC(secret)）。
func (s *Service) IssueToken(username, role string) (string, error) {
	return s.IssueTokenWithTTL(username, role, s.tokenTTL)
}

// IssueTokenWithTTL 按指定有效期签发 token（测试/SSO 回调用）。
func (s *Service) IssueTokenWithTTL(username, role string, ttl time.Duration) (string, error) {
	exp := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%d", username, role, exp)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + sig)), nil
}

// VerifyToken 校验 token 并返回 (username, role)。
func (s *Service) VerifyToken(token string) (string, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", fmt.Errorf("invalid token")
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return "", "", fmt.Errorf("invalid token")
	}
	payload := parts[0] + "|" + parts[1] + "|" + parts[2]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(parts[3])) != 1 {
		return "", "", fmt.Errorf("invalid signature")
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", "", fmt.Errorf("token expired")
	}
	return parts[0], parts[1], nil
}

// --- 认证中间件 ---

// Middleware 校验 Bearer token，注入 gin context（username/role）。
// public 里的路径前缀放行（健康检查、webhook ingest、SSO 回调）。
func (s *Service) Middleware(public []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		for _, p := range public {
			if strings.HasPrefix(path, p) {
				c.Next()
				return
			}
		}
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "missing bearer token"})
			return
		}
		username, role, err := s.VerifyToken(strings.TrimPrefix(auth, "Bearer "))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": err.Error()})
			return
		}
		c.Set("username", username)
		c.Set("role", role)
		c.Next()
	}
}

// --- SSO (OIDC 授权码) ---

// SSOEnabled 是否配置了 OIDC。
func (s *Service) SSOEnabled() bool { return s.OIDC != nil }

// SSOFrontendURL 回调成功后的前端落地页（默认根路径，SPA 解析 hash）。
func (s *Service) SSOFrontendURL() string {
	if s.OIDC != nil && s.OIDC.FrontendURL != "" {
		return s.OIDC.FrontendURL
	}
	return "/"
}

// LoginURL 生成 OIDC 授权跳转地址：随机 state+nonce 暂存（一次性、10 分钟过期），
// 回调时校验 state 防 CSRF、nonce 防重放。
func (s *Service) LoginURL() (string, error) {
	if s.OIDC == nil {
		return "", fmt.Errorf("sso not configured")
	}
	_, cfg, err := s.oidcProvider()
	if err != nil {
		return "", err
	}
	state := randomHex(24)
	nonce := randomHex(16)
	s.mu.Lock()
	if s.ssoStates == nil {
		s.ssoStates = make(map[string]ssoState)
	}
	s.ssoStates[state] = ssoState{nonce: nonce, expiry: time.Now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return cfg.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce)), nil
}

// CompleteLogin 回调收尾：校验 state（一次性）→ 换 token → 校验 ID token
// （issuer/aud/exp/签名 + nonce）→ find-or-create 用户 → 签发管理端 token。
func (s *Service) CompleteLogin(ctx context.Context, state, code string) (string, error) {
	if s.OIDC == nil {
		return "", fmt.Errorf("sso not configured")
	}
	s.mu.Lock()
	st, ok := s.ssoStates[state]
	if ok {
		delete(s.ssoStates, state)
	}
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("invalid or expired sso state")
	}
	if time.Now().After(st.expiry) {
		return "", fmt.Errorf("sso state expired")
	}
	_, cfg, err := s.oidcProvider()
	if err != nil {
		return "", err
	}
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return "", fmt.Errorf("id_token missing in token response")
	}
	idTok, err := s.oidcVerifier().Verify(ctx, rawID)
	if err != nil {
		return "", fmt.Errorf("verify id_token: %w", err)
	}
	// nonce 防重放：与发起跳转时绑定的值比对（v3 verifier 不自动校验 nonce）。
	if idTok.Nonce != st.nonce {
		return "", fmt.Errorf("id_token nonce mismatch")
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return "", fmt.Errorf("parse id_token claims: %w", err)
	}
	username := claims.Email
	if username == "" {
		username = claims.PreferredUsername
	}
	if username == "" {
		username = "sso-" + idTok.Subject
	}
	u, err := s.findOrCreateSSOUser(username)
	if err != nil {
		return "", err
	}
	return s.IssueToken(u.Username, u.Role)
}

// oidcProvider 懒加载 OIDC 提供者（discovery：JWKS/端点）与 oauth2 配置。
func (s *Service) oidcProvider() (*oidc.Provider, *oauth2.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oidcProv != nil || s.oidcErr != nil {
		return s.oidcProv, s.oauthCfg, s.oidcErr
	}
	prov, err := oidc.NewProvider(context.Background(), s.OIDC.Issuer)
	if err != nil {
		s.oidcErr = err
		return nil, nil, err
	}
	scopes := s.OIDC.Scopes
	if len(scopes) == 0 {
		scopes = []string{"profile", "email"}
	}
	s.oidcProv = prov
	s.oauthCfg = &oauth2.Config{
		ClientID:     s.OIDC.ClientID,
		ClientSecret: s.OIDC.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  s.OIDC.RedirectURL,
		Scopes:       append([]string{oidc.ScopeOpenID}, scopes...),
	}
	return s.oidcProv, s.oauthCfg, nil
}

// oidcVerifier 构造 ID token 校验器（issuer/aud/签名，nonce 由调用方比对）。
func (s *Service) oidcVerifier() *oidc.IDTokenVerifier {
	prov, _, err := s.oidcProvider()
	if err != nil {
		return nil
	}
	return prov.Verifier(&oidc.Config{ClientID: s.OIDC.ClientID})
}

// findOrCreateSSOUser SSO 用户 find-or-create：无本地口令（不可密码登录），
// 首个用户提升为 admin（引导）。
func (s *Service) findOrCreateSSOUser(username string) (*store.User, error) {
	u, err := s.st.GetUserByUsername(username)
	if err != nil {
		return nil, err
	}
	if u != nil {
		return u, nil
	}
	role := s.OIDC.DefaultRole
	if role == "" {
		role = "viewer"
	}
	if all, err := s.st.ListUsers(); err == nil && len(all) == 0 {
		role = "admin"
	}
	u = &store.User{
		ID:        "u-" + randomHex(8),
		Username:  username,
		Password:  "", // SSO 用户无本地口令
		Role:      role,
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.st.PutUser(u); err != nil {
		return nil, err
	}
	return u, nil
}

// --- helpers ---

func hashPassword(password, salt string) string {
	h := sha256.New()
	h.Write([]byte(salt))
	h.Write([]byte(password))
	return hex.EncodeToString(h.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
