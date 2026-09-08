// Package idp implements OpsGaurd as an OIDC / OAuth 2.1 identity provider.
// It exposes standard endpoints (discovery, JWKS, authorize, token, userinfo,
// logout) so that other systems — internal business apps and Worker/MCP
// tools — can redirect here for authentication.
//
// Design notes:
//   - JWTs (ID/Access tokens) are signed with RS256 via go-jose; the private
//     key persists in LevelDB and is generated on first start.
//   - Authorization codes are one-shot, short-lived, stored in LevelDB.
//   - Refresh tokens are opaque random strings persisted server-side.
//   - An IdP SSO session cookie (opsguard_idp_sid) lets a user logged into
//     OpsGaurd authorize multiple RPs without re-entering credentials.
//   - PKCE is required for public clients and accepted for confidential ones.
package idp

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// SigningKeys 管理 IdP 的 JWT 签名 RSA 密钥：私钥签名、公钥以 JWKS 发布。
type SigningKeys struct {
	st *store.Store

	mu     sync.RWMutex
	kid    string             // 当前 active KID
	priv   *rsa.PrivateKey    // 当前 active 私钥（内存缓存）
	jwks   jose.JSONWebKeySet // 全部公钥（含历史，供轮换）
	signer jose.Signer        // 当前 active 签名器
}

// LoadSigningKeys 加载（或在首次启动时生成）IdP 签名密钥。
func LoadSigningKeys(st *store.Store) (*SigningKeys, error) {
	sk := &SigningKeys{st: st}
	if err := sk.loadOrGenerate(); err != nil {
		return nil, err
	}
	return sk, nil
}

func (k *SigningKeys) loadOrGenerate() error {
	kid, err := k.st.GetActiveSigningKeyKID()
	if err != nil {
		return fmt.Errorf("read active signing key: %w", err)
	}
	if kid != "" {
		if err := k.loadFromStore(kid); err != nil {
			return err
		}
		// 加载历史公钥进 JWKS（轮换期保留旧公钥供验签）。
		return k.rebuildJWKS()
	}
	// 首次启动：生成新密钥。
	return k.rotate()
}

func (k *SigningKeys) loadFromStore(kid string) error {
	rec, err := k.st.GetSigningKey(kid)
	if err != nil {
		return fmt.Errorf("read signing key %q: %w", kid, err)
	}
	if rec == nil {
		// active 指针失效（数据被清），退回生成新密钥。
		return k.rotate()
	}
	var jwk jose.JSONWebKey
	if err := jwk.UnmarshalJSON(rec.PrivateKeyJWK); err != nil {
		return fmt.Errorf("unmarshal signing key jwk: %w", err)
	}
	priv, ok := jwk.Key.(*rsa.PrivateKey)
	if !ok {
		return fmt.Errorf("signing key %q is not RSA", kid)
	}
	k.mu.Lock()
	k.kid = kid
	k.priv = priv
	k.mu.Unlock()
	return k.initSigner()
}

// rebuildJWKS 把存储中的全部历史签名公钥加入 JWKS。
func (k *SigningKeys) rebuildJWKS() error {
	recs, err := k.st.ListSigningKeys()
	if err != nil {
		return fmt.Errorf("list signing keys: %w", err)
	}
	var set jose.JSONWebKeySet
	for _, rec := range recs {
		var jwk jose.JSONWebKey
		if err := jwk.UnmarshalJSON(rec.PrivateKeyJWK); err != nil {
			continue // 跳过损坏记录
		}
		pub := jwk.Public()
		set.Keys = append(set.Keys, pub)
	}
	k.mu.Lock()
	k.jwks = set
	k.mu.Unlock()
	return nil
}

// rotate 生成新 RSA 私钥、持久化、设为 active，并重建 JWKS。
func (k *SigningKeys) rotate() error {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate rsa key: %w", err)
	}
	kid := "idp-" + uuid.NewString()
	jwk := jose.JSONWebKey{Key: priv, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}
	jwkBytes, err := jwk.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal signing key: %w", err)
	}
	if err := k.st.PutSigningKey(&store.SigningKeyRecord{
		KID:           kid,
		PrivateKeyJWK: jwkBytes,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("persist signing key: %w", err)
	}
	k.mu.Lock()
	k.kid = kid
	k.priv = priv
	k.mu.Unlock()
	if err := k.initSigner(); err != nil {
		return err
	}
	return k.rebuildJWKS()
}

// initSigner 构造 go-jose Signer（RS256，kid 写入 JWT 头）。
func (k *SigningKeys) initSigner() error {
	k.mu.RLock()
	priv, kid := k.priv, k.kid
	k.mu.RUnlock()
	if priv == nil {
		return fmt.Errorf("no active signing key")
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       priv,
	}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderKey("kid"): kid}})
	if err != nil {
		return fmt.Errorf("create jose signer: %w", err)
	}
	k.mu.Lock()
	k.signer = signer
	k.mu.Unlock()
	return nil
}

// Sign 用 active 密钥签名一段 payload，返回紧凑序列化的 JWS（JWT）。
func (k *SigningKeys) Sign(payload []byte) (string, error) {
	k.mu.RLock()
	signer := k.signer
	k.mu.RUnlock()
	if signer == nil {
		return "", fmt.Errorf("signing keys not initialized")
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return obj.CompactSerialize()
}

// JWKS 返回所有签名公钥（含轮换期历史公钥），供 /jwks 端点发布。
func (k *SigningKeys) JWKS() jose.JSONWebKeySet {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.jwks
}

// KID 返回当前 active 公钥的 KID。
func (k *SigningKeys) KID() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.kid
}
