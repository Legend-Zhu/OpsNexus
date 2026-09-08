// token 运营：LevelDB 运行时表与内存鉴权表的同步。静态种子（config.yaml）
// 镜像进库供管理页统一展示/禁用；运行时条目可增删。名字全局唯一（静态
// 种子名保留）。任何变更后原子换表立即生效（无需重载网关）。
package mcpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// TokenView 管理 API 的 token 视图（绝不含 secret 原文/摘要）。
type TokenView struct {
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Static    bool   `json:"static"` // 来自 config.yaml 种子（不可删，可禁用）
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at,omitempty"`
	LastUsed  string `json:"last_used,omitempty"`
}

// syncTokens 从 store 加载运行时表并原子换表；同时把静态种子镜像进库
// （缺则补、变则更新摘要/scope，保留禁用状态与 last_used）。
// New 时调用一次；管理 API 的每次写操作后调用。
func (h *Handler) syncTokens() error {
	if h.deps.Store == nil {
		return nil
	}
	// 1. 静态种子镜像 upsert（同名的非静态历史条目会让位给静态种子：
	// 名字唯一，种子以配置为准）
	existing := map[string]*store.MCPToken{}
	if recs, err := h.deps.Store.ListMCPTokens(); err == nil {
		for _, r := range recs {
			existing[r.Name] = r
		}
	}
	now := time.Now().UTC()
	for _, seed := range h.deps.Config.Tokens {
		if seed.Name == "" || seed.Secret == "" {
			continue
		}
		sum := sha256.Sum256([]byte(seed.Secret))
		rec, ok := existing[seed.Name]
		if !ok || !rec.Static {
			rec = &store.MCPToken{Name: seed.Name, Static: true, Enabled: true, CreatedAt: now}
		}
		rec.SecretSHA = hex.EncodeToString(sum[:])
		rec.Scope = string(parseScope(seed.Scope))
		if rec.CreatedAt.IsZero() {
			rec.CreatedAt = now
		}
		if err := h.deps.Store.PutMCPToken(rec); err != nil {
			return fmt.Errorf("mirror static token %q: %w", seed.Name, err)
		}
	}

	// 2. 加载全表 → 构建内存条目
	recs, err := h.deps.Store.ListMCPTokens()
	if err != nil {
		return err
	}
	entries := make([]tokenEntry, 0, len(recs))
	for _, r := range recs {
		sha, err := hex.DecodeString(r.SecretSHA)
		if err != nil || len(sha) != sha256.Size {
			h.log.Warn("skip mcp token with bad digest", "name", r.Name)
			continue
		}
		var e [32]byte
		copy(e[:], sha)
		entries = append(entries, tokenEntry{
			name: r.Name, secretSHA: e,
			scope:    parseScope(r.Scope),
			runtime:  !r.Static,
			disabled: !r.Enabled,
		})
	}
	h.auth.replaceRuntime(entries)
	return nil
}

// ListTokens 管理页视图（静态镜像 + 运行时，静态在前、名字排序）。
func (h *Handler) ListTokens() ([]TokenView, error) {
	if h.deps.Store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	recs, err := h.deps.Store.ListMCPTokens()
	if err != nil {
		return nil, err
	}
	h.auth.mu.RLock()
	lastUsed := make(map[string]time.Time, len(h.auth.lastUsed))
	for k, v := range h.auth.lastUsed {
		lastUsed[k] = v
	}
	h.auth.mu.RUnlock()

	out := make([]TokenView, 0, len(recs))
	for _, r := range recs {
		v := TokenView{
			Name: r.Name, Scope: r.Scope,
			Static: r.Static, Enabled: r.Enabled,
			CreatedAt: formatTime(r.CreatedAt),
		}
		if lu, ok := lastUsed[r.Name]; ok {
			v.LastUsed = formatTime(lu)
		} else if !r.LastUsed.IsZero() {
			v.LastUsed = formatTime(r.LastUsed)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Static != out[j].Static {
			return out[i].Static
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// CreateToken 新建运行时 token：生成 secret 原文（仅本次返回），落 SHA。
func (h *Handler) CreateToken(name, scope string) (*TokenView, string, error) {
	if h.deps.Store == nil {
		return nil, "", fmt.Errorf("store not initialized")
	}
	if !validTokenName(name) {
		return nil, "", fmt.Errorf("token name must match [a-z][a-z0-9-]{1,31} (lowercase slug)")
	}
	scope = string(parseScope(scope)) // 归一化：read | write | exec（未知值回 read）
	if rec, err := h.deps.Store.GetMCPToken(name); err != nil {
		return nil, "", err
	} else if rec != nil {
		return nil, "", fmt.Errorf("token %q already exists", name)
	}
	secret, err := randomHexID()
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256([]byte(secret))
	rec := &store.MCPToken{
		Name:      name,
		SecretSHA: hex.EncodeToString(sum[:]),
		Scope:     scope,
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.deps.Store.PutMCPToken(rec); err != nil {
		return nil, "", err
	}
	if err := h.syncTokens(); err != nil {
		return nil, "", err
	}
	return &TokenView{Name: rec.Name, Scope: rec.Scope, Enabled: true,
		CreatedAt: formatTime(rec.CreatedAt)}, secret, nil
}

// DeleteToken 删除运行时 token（静态种子拒绝——改 config.yaml）。
func (h *Handler) DeleteToken(name string) error {
	if h.deps.Store == nil {
		return fmt.Errorf("store not initialized")
	}
	rec, err := h.deps.Store.GetMCPToken(name)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("token %q not found", name)
	}
	if rec.Static {
		return fmt.Errorf("token %q comes from config.yaml (mcp.tokens); remove it there instead", name)
	}
	if err := h.deps.Store.DeleteMCPToken(name); err != nil {
		return err
	}
	return h.syncTokens()
}

// SetTokenEnabled 启停（静态种子可禁用：镜像记录记 Enabled=false）。
func (h *Handler) SetTokenEnabled(name string, enabled bool) error {
	if h.deps.Store == nil {
		return fmt.Errorf("store not initialized")
	}
	rec, err := h.deps.Store.GetMCPToken(name)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("token %q not found", name)
	}
	rec.Enabled = enabled
	if err := h.deps.Store.PutMCPToken(rec); err != nil {
		return err
	}
	return h.syncTokens()
}

// validTokenName token 名：小写字母开头的 slug，2-32 字符（审计主体可读性）。
func validTokenName(s string) bool {
	if len(s) < 2 || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
