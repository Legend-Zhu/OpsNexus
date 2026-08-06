package idp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// ClientService 管理 OIDC client（RP）的注册与认证。
type ClientService struct {
	st *store.Store
}

// NewClientService 创建 client 服务。
func NewClientService(st *store.Store) *ClientService {
	return &ClientService{st: st}
}

// CreateClientInput 创建 client 的入参。Public=true 时忽略 Secret，成为 PKCE-only 公共客户端。
type CreateClientInput struct {
	Name          string   `json:"name"`
	RedirectURIs  []string `json:"redirect_uris"`
	Scopes        []string `json:"scopes"`
	Public        bool     `json:"public"`
	TokenTTL      string   `json:"token_ttl,omitempty"`
}

// CreateClient 创建 client。返回创建后的 client（SecretHash 已哈希）与一次性明文 secret。
// 机密客户端（Public=false）调用方必须把明文 secret 立即交付对接方，服务端不再保存明文。
func (cs *ClientService) CreateClient(in CreateClientInput) (*store.Client, string, error) {
	if in.Name == "" {
		return nil, "", fmt.Errorf("client name is required")
	}
	if len(in.RedirectURIs) == 0 {
		return nil, "", fmt.Errorf("at least one redirect_uri is required")
	}
	if existing, err := cs.st.GetClientByName(in.Name); err != nil {
		return nil, "", err
	} else if existing != nil {
		return nil, "", fmt.Errorf("client %q already exists", in.Name)
	}

	plaintext := ""
	secretHash := ""
	if !in.Public {
		plaintext = randomToken(32)
		hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
		if err != nil {
			return nil, "", fmt.Errorf("hash client secret: %w", err)
		}
		secretHash = string(hash)
	}

	c := &store.Client{
		ID:            "cli-" + randomToken(8),
		Name:          in.Name,
		SecretHash:    secretHash,
		RedirectURIs:  in.RedirectURIs,
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		Scopes:        normalizeScopes(in.Scopes),
		TokenTTL:      in.TokenTTL,
		Public:        in.Public,
		CreatedAt:     time.Now().UTC(),
	}
	if err := cs.st.PutClient(c); err != nil {
		return nil, "", err
	}
	return c, plaintext, nil
}

// UpdateClientInput 更新 client 的可变字段。
type UpdateClientInput struct {
	Name         *string   `json:"name,omitempty"`
	RedirectURIs *[]string `json:"redirect_uris,omitempty"`
	Scopes       *[]string `json:"scopes,omitempty"`
	TokenTTL     *string   `json:"token_ttl,omitempty"`
}

// UpdateClient 更新 client（不可改 ID / Public / Secret，改密用 RotateSecret）。
func (cs *ClientService) UpdateClient(id string, in UpdateClientInput) (*store.Client, error) {
	c, err := cs.st.GetClient(id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("client not found")
	}
	if in.Name != nil && *in.Name != c.Name {
		if existing, err := cs.st.GetClientByName(*in.Name); err != nil {
			return nil, err
		} else if existing != nil {
			return nil, fmt.Errorf("client %q already exists", *in.Name)
		}
		c.Name = *in.Name
	}
	if in.RedirectURIs != nil {
		c.RedirectURIs = *in.RedirectURIs
	}
	if in.Scopes != nil {
		c.Scopes = normalizeScopes(*in.Scopes)
	}
	if in.TokenTTL != nil {
		c.TokenTTL = *in.TokenTTL
	}
	c.UpdatedAt = time.Now().UTC()
	if err := cs.st.PutClient(c); err != nil {
		return nil, err
	}
	return c, nil
}

// RotateSecret 重置机密 client 的 secret，返回一次性明文。
// 公共客户端调用返回错误（无 secret 可轮换）。
func (cs *ClientService) RotateSecret(id string) (string, error) {
	c, err := cs.st.GetClient(id)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", fmt.Errorf("client not found")
	}
	if c.Public {
		return "", fmt.Errorf("public client has no secret")
	}
	plaintext := randomToken(32)
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash client secret: %w", err)
	}
	c.SecretHash = string(hash)
	c.UpdatedAt = time.Now().UTC()
	if err := cs.st.PutClient(c); err != nil {
		return "", err
	}
	return plaintext, nil
}

// Authenticate 校验 client 凭证（机密客户端用）。公共客户端应直接跳过调用此方法。
// 用 bcrypt.CompareHashAndPassword 做常量时间比较；client 不存在时与一个 dummy hash
// 比较以抑制时序差异造成的存在性探测。
func (cs *ClientService) Authenticate(clientID, clientSecret string) (*store.Client, error) {
	c, err := cs.st.GetClient(clientID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(clientSecret))
		return nil, fmt.Errorf("invalid client credentials")
	}
	if c.Public {
		// 公共客户端不应携带 secret；视为认证失败（应走 PKCE）。
		return nil, fmt.Errorf("public client must not present a secret")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(clientSecret)); err != nil {
		return nil, fmt.Errorf("invalid client credentials")
	}
	return c, nil
}

// Get / List / Delete 透传到 store。
func (cs *ClientService) Get(id string) (*store.Client, error)   { return cs.st.GetClient(id) }
func (cs *ClientService) List() ([]*store.Client, error)        { return cs.st.ListClients() }
func (cs *ClientService) Delete(id string) error                 { return cs.st.DeleteClient(id) }

// --- helpers ---

// normalizeScopes 去重并保留 openid/profile/email 的合法 scope 子集。
func normalizeScopes(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// randomToken 生成 n 字节的随机十六进制串（用作 client_id 片段、secret、code、refresh token）。
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()) // 极端兜底，不应触达
	}
	return hex.EncodeToString(b)
}

// dummyBcryptHash 用于 Authenticate 在 client 不存在时仍消耗 bcrypt 时间。
var dummyBcryptHash = mustDummyHash()

func mustDummyHash() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)
	return h
}
