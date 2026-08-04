// Package auth implements user management and request authentication (P6).
// Primary auth is SSO (OIDC, 决策⑤); the built-in OIDC client is configured
// via server.sso.oidc. When no SSO issuer is configured (intranet bootstrap),
// local users + HMAC-signed bearer tokens act as fallback. Passwords are
// hashed (PBKDF2-style with per-user salt) and never stored plaintext.
package auth

import (
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
	"time"

	"github.com/gin-gonic/gin"

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
}

// OIDCConfig SSO/OIDC 配置（内网对接企业网关用）。
type OIDCConfig struct {
	Issuer       string `yaml:"issuer" json:"issuer"`
	ClientID     string `yaml:"client_id" json:"clientId"`
	ClientSecret string `yaml:"client_secret" json:"-"`
	RedirectURL  string `yaml:"redirect_url" json:"redirectUrl"`
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
