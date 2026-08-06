// Persistence for the OpsGaurd IdP (OIDC provider): OIDC clients (RPs),
// one-shot authorization codes, access-token jti records, refresh tokens,
// JWT signing keys, and SSO sessions. All stored as JSON under the IdP
// buckets declared in store.go (schema v2).
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// --- OIDC Client ---

// Client 是注册到本 IdP 的 OIDC 客户端（依赖方 RP）。
type Client struct {
	ID            string    `json:"id"`                       // 形如 "cli-" + 16 hex
	Name          string    `json:"name"`                     // 展示名
	SecretHash    string    `json:"secret_hash,omitempty"`    // bcrypt 哈希；公共客户端为空
	RedirectURIs  []string  `json:"redirect_uris"`            // 精确匹配白名单
	GrantTypes    []string  `json:"grant_types"`              // ["authorization_code","refresh_token"]
	ResponseTypes []string  `json:"response_types"`           // ["code"]
	Scopes        []string  `json:"scopes"`                   // 允许申请的 scope 子集
	TokenTTL      string    `json:"token_ttl,omitempty"`      // 覆盖全局，空用默认
	Public        bool      `json:"public"`                   // true = PKCE-only 公共客户端（无 secret）
	ConsentRequired bool    `json:"consent_required,omitempty"` // 预留：未来 consent 页用
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

func clientKey(id string) string    { return BucketClient + "/" + id }
func clientNameKey(n string) string { return BucketClient + ":by-name/" + n }

// PutClient 写入 client，并维护 by-name 二级索引。
func (s *Store) PutClient(c *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal client: %w", err)
	}
	batch := new(leveldb.Batch)
	batch.Put([]byte(clientKey(c.ID)), data)
	batch.Put([]byte(clientNameKey(c.Name)), []byte(c.ID))
	return s.db.Write(batch, nil)
}

// GetClient 按 ID 读取；不存在返回 (nil, nil)。
func (s *Store) GetClient(id string) (*Client, error) {
	raw, err := s.get(clientKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Client
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode client %q: %w", id, err)
	}
	return &c, nil
}

// GetClientByName 按名称读取（admin 查重 / 登录页展示用）。
func (s *Store) GetClientByName(name string) (*Client, error) {
	idRaw, err := s.get(clientNameKey(name))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetClient(string(idRaw))
}

// DeleteClient 删除 client 及其 by-name 索引。
func (s *Store) DeleteClient(id string) error {
	c, err := s.GetClient(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := new(leveldb.Batch)
	batch.Delete([]byte(clientKey(id)))
	if c != nil {
		batch.Delete([]byte(clientNameKey(c.Name)))
	}
	return s.db.Write(batch, nil)
}

// ListClients 列出全部 client。
func (s *Store) ListClients() ([]*Client, error) {
	var out []*Client
	err := s.iterate(BucketClient+"/", func(_ string, value []byte) error {
		var c Client
		if err := json.Unmarshal(value, &c); err != nil {
			return fmt.Errorf("decode client: %w", err)
		}
		out = append(out, &c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- 授权码 ---

// AuthCode 一次性授权码（短 TTL，兑换后删除）。
type AuthCode struct {
	Code            string    `json:"code"`
	ClientID        string    `json:"client_id"`
	UserID          string    `json:"user_id"`
	Username        string    `json:"username"` // 冗余，便于签 token 时取 profile
	RedirectURI     string    `json:"redirect_uri"`
	Scopes          []string  `json:"scopes"`
	Nonce           string    `json:"nonce,omitempty"`
	CodeChallenge   string    `json:"code_challenge,omitempty"`   // PKCE
	CodeChallengeMethod string `json:"code_challenge_method,omitempty"` // "S256"
	AuthTime        time.Time `json:"auth_time"`
	Expiry          time.Time `json:"expiry"`
	Used            bool      `json:"used"`
}

func authCodeKey(code string) string { return BucketAuthCode + "/" + code }

// PutAuthCode 写入授权码。
func (s *Store) PutAuthCode(ac *AuthCode) error { return s.put(authCodeKey(ac.Code), ac) }

// GetAuthCode 读取授权码；不存在返回 (nil, nil)。
func (s *Store) GetAuthCode(code string) (*AuthCode, error) {
	raw, err := s.get(authCodeKey(code))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ac AuthCode
	if err := json.Unmarshal(raw, &ac); err != nil {
		return nil, fmt.Errorf("decode authcode: %w", err)
	}
	return &ac, nil
}

// DeleteAuthCode 删除授权码（兑换后 / 过期清理）。
func (s *Store) DeleteAuthCode(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(authCodeKey(code)), nil)
}

// --- Access Token jti（用于 introspect / 吊销） ---

// AccessTokenRecord 是 access token（JWT 自包含）在服务端的影子记录。
type AccessTokenRecord struct {
	JTI       string    `json:"jti"`
	Sub       string    `json:"sub"`
	ClientID  string    `json:"client_id"`
	Scope     string    `json:"scope"`
	Expiry    time.Time `json:"expiry"`
	Revoked   bool      `json:"revoked,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func accessTokenKey(jti string) string { return BucketAccessToken + "/" + jti }

// PutAccessToken 写入 access token 影子记录。
func (s *Store) PutAccessToken(t *AccessTokenRecord) error {
	return s.put(accessTokenKey(t.JTI), t)
}

// GetAccessToken 读取；不存在返回 (nil, nil)。
func (s *Store) GetAccessToken(jti string) (*AccessTokenRecord, error) {
	raw, err := s.get(accessTokenKey(jti))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t AccessTokenRecord
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode access token: %w", err)
	}
	return &t, nil
}

// RevokeAccessToken 标记 access token 为已吊销。
func (s *Store) RevokeAccessToken(jti string) error {
	t, err := s.GetAccessToken(jti)
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	t.Revoked = true
	return s.PutAccessToken(t)
}

// --- Refresh Token ---

// RefreshToken 不透明 refresh token 记录。
type RefreshToken struct {
	Token     string    `json:"token"`
	Sub       string    `json:"sub"`
	Username  string    `json:"username"`
	ClientID  string    `json:"client_id"`
	Scopes    []string  `json:"scopes"`
	Expiry    time.Time `json:"expiry"`
	Revoked   bool      `json:"revoked,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func refreshTokenKey(tok string) string { return BucketRefreshToken + "/" + tok }

// PutRefreshToken 写入 refresh token。
func (s *Store) PutRefreshToken(t *RefreshToken) error {
	return s.put(refreshTokenKey(t.Token), t)
}

// GetRefreshToken 读取；不存在返回 (nil, nil)。
func (s *Store) GetRefreshToken(tok string) (*RefreshToken, error) {
	raw, err := s.get(refreshTokenKey(tok))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t RefreshToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode refresh token: %w", err)
	}
	return &t, nil
}

// DeleteRefreshToken 删除 refresh token（轮换 / 吊销）。
func (s *Store) DeleteRefreshToken(tok string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(refreshTokenKey(tok)), nil)
}

// --- JWT 签名密钥 ---

// SigningKeyRecord 是 IdP JWT 签名 RSA 私钥的持久化记录。
// PrivateKeyJWK 为 JWK 格式的私钥（便于 go-jose 反序列化）。
type SigningKeyRecord struct {
	KID          string    `json:"kid"`
	PrivateKeyJWK []byte   `json:"private_key_jwk"` // go-jose JSONWebKey 序列化
	CreatedAt    time.Time `json:"created_at"`
}

func signingKeyKey(kid string) string { return BucketSigningKey + "/" + kid }
func signingKeyActiveKey() string     { return BucketSigningKey + "/active" }

// PutSigningKey 写入签名密钥，并将其设为 active。
func (s *Store) PutSigningKey(k *SigningKeyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(k)
	if err != nil {
		return fmt.Errorf("marshal signing key: %w", err)
	}
	batch := new(leveldb.Batch)
	batch.Put([]byte(signingKeyKey(k.KID)), data)
	batch.Put([]byte(signingKeyActiveKey()), []byte(k.KID))
	return s.db.Write(batch, nil)
}

// GetSigningKey 按 KID 读取；不存在返回 (nil, nil)。
func (s *Store) GetSigningKey(kid string) (*SigningKeyRecord, error) {
	raw, err := s.get(signingKeyKey(kid))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var k SigningKeyRecord
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	return &k, nil
}

// GetActiveSigningKeyKID 读取当前 active 密钥的 KID。
func (s *Store) GetActiveSigningKeyKID() (string, error) {
	raw, err := s.get(signingKeyActiveKey())
	if err == leveldb.ErrNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ListSigningKeys 列出全部历史签名密钥（JWKS 发布多公钥用）。
func (s *Store) ListSigningKeys() ([]*SigningKeyRecord, error) {
	var out []*SigningKeyRecord
	err := s.iterate(BucketSigningKey+"/", func(key string, value []byte) error {
		if key == signingKeyActiveKey() { // 跳过 active 指针
			return nil
		}
		var k SigningKeyRecord
		if err := json.Unmarshal(value, &k); err != nil {
			return fmt.Errorf("decode signing key: %w", err)
		}
		out = append(out, &k)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- IdP SSO 会话 ---

// IDPSession 是 IdP 端的 SSO 会话（cookie opsguard_idp_sid -> 记录）。
// 用户在 OpsGaurd 登录一次后，多个 RP 跳 /authorize 时凭此免再登录。
type IDPSession struct {
	SID        string    `json:"sid"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeen   time.Time `json:"last_seen"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func idpSessionKey(sid string) string { return BucketIDPSession + "/" + sid }

// PutIDPSession 写入 / 续期 IdP 会话。
func (s *Store) PutIDPSession(sess *IDPSession) error {
	return s.put(idpSessionKey(sess.SID), sess)
}

// GetIDPSession 读取；不存在或已过期返回 (nil, nil)。
func (s *Store) GetIDPSession(sid string) (*IDPSession, error) {
	raw, err := s.get(idpSessionKey(sid))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sess IDPSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return nil, fmt.Errorf("decode idp session: %w", err)
	}
	if !sess.ExpiresAt.IsZero() && time.Now().After(sess.ExpiresAt) {
		return nil, nil
	}
	return &sess, nil
}

// DeleteIDPSession 删除 IdP 会话（登出）。
func (s *Store) DeleteIDPSession(sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(idpSessionKey(sid)), nil)
}
