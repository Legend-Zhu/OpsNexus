// Token 鉴权与读写 scope。命名 token 与 Worker 的 auth.tokens（name→secret）
// 同构：长期有效、可命名（审计 actor）、可按 scope 分权。校验对 SHA-256
// 摘要做常数时间比较（镜像 Worker internal/authz），secret 原文不落内存表。
//
// token 来源两层：config.yaml 静态种子（只读）+ LevelDB 运行时表（admin
// API 增删）。合并规则：同名时运行时条目生效（可禁用静态种子）；运行时
// 条目删除后回落静态种子。表整体经原子指针替换热生效，工具调用路径无锁。
package mcpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Scope token 权限档位。
type Scope string

const (
	// ScopeRead 只读：证据查询、观测、拨测、排查会话读取。
	ScopeRead Scope = "read"
	// ScopeWrite 含 read 的全部写操作（增删改、部署、构建提交）。
	ScopeWrite Scope = "write"
	// ScopeExec 含 write 的全部能力，外加 exec 透传（service_exec /
	// node_exec——容器/宿主机命令执行，最高危档）。还需 mcp.exec_enabled
	// 开启才实际可用。
	ScopeExec Scope = "exec"
)

// identity 一次请求的调用方身份（由中间件注入 request context）。
type identity struct {
	Name  string
	Scope Scope
	// Actor 审计主体：name@clientIP。
	Actor string
}

// canWrite write 及以上（含 exec）scope 判定。
func (i *identity) canWrite() bool {
	return i != nil && (i.Scope == ScopeWrite || i.Scope == ScopeExec)
}

// canExec 仅 exec scope。
func (i *identity) canExec() bool { return i != nil && i.Scope == ScopeExec }

// tokenEntry 内存 token 表条目（仅摘要）。
type tokenEntry struct {
	name      string
	secretSHA [32]byte
	scope     Scope
	// runtime true = 来自 LevelDB 运行时表（可删除）。
	runtime bool
	// disabled true = 已禁用（鉴权必败）。
	disabled bool
}

// authorizer token 表：静态种子 + 运行时表，原子换表热生效。
type authorizer struct {
	mu       sync.RWMutex
	static   []tokenEntry // config.yaml 种子（摘要化）
	runtime  []tokenEntry // LevelDB 运行时表
	lastUsed map[string]time.Time
}

// newAuthorizer 从配置构建静态种子表。原文即刻摘要化，调用方丢弃原文。
func newAuthorizer(cfg []TokenConfig) *authorizer {
	a := &authorizer{lastUsed: map[string]time.Time{}}
	for _, t := range cfg {
		if t.Name == "" || t.Secret == "" {
			continue
		}
		a.static = append(a.static, tokenEntry{
			name:      t.Name,
			secretSHA: sha256.Sum256([]byte(t.Secret)),
			scope:     parseScope(t.Scope),
		})
	}
	return a
}

func parseScope(s string) Scope {
	switch Scope(s) {
	case ScopeWrite:
		return ScopeWrite
	case ScopeExec:
		return ScopeExec
	}
	return ScopeRead
}

// replaceRuntime 整表替换运行时条目（加载/变更后调用，原子生效）。
func (a *authorizer) replaceRuntime(entries []tokenEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runtime = entries
}

// authenticate bearer 原文 → 身份。运行时条目优先于静态种子（同名覆盖）；
// 常数时间比较防时序侧信道；命中记录 last_used（懒更新，list 时落库）。
func (a *authorizer) authenticate(secret string) (*identity, bool) {
	if secret == "" || a == nil {
		return nil, false
	}
	sum := sha256.Sum256([]byte(secret))
	a.mu.RLock()
	var hit *tokenEntry
	for i := range a.runtime {
		if subtle.ConstantTimeCompare(sum[:], a.runtime[i].secretSHA[:]) == 1 {
			hit = &a.runtime[i]
			break
		}
	}
	if hit == nil {
		for i := range a.static {
			if subtle.ConstantTimeCompare(sum[:], a.static[i].secretSHA[:]) == 1 {
				c := a.static[i]
				hit = &c
				break
			}
		}
	}
	enabled := hit != nil && !hit.disabled
	if enabled {
		name := hit.name
		a.mu.RUnlock()
		a.mu.Lock()
		a.lastUsed[name] = time.Now()
		a.mu.Unlock()
		return &identity{Name: name, Scope: hit.scope}, true
	}
	a.mu.RUnlock()
	return nil, false
}

// enabled 表内是否有可用 token。
func (a *authorizer) enabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for i := range a.runtime {
		if !a.runtime[i].disabled {
			return true
		}
	}
	return len(a.static) > 0 // 静态种子不可单独禁用，存在即可用
}

// tokenNames 可用 token 名（健康检查展示；静态种子与其库内镜像去重——
// 运行时表含全部条目，静态列表仅作无 store 时的兜底）。
func (a *authorizer) tokenNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]string, 0, len(a.runtime)+len(a.static))
	for i := range a.runtime {
		if a.runtime[i].disabled || seen[a.runtime[i].name] {
			continue
		}
		seen[a.runtime[i].name] = true
		out = append(out, a.runtime[i].name)
	}
	for i := range a.static {
		if seen[a.static[i].name] {
			continue
		}
		seen[a.static[i].name] = true
		out = append(out, a.static[i].name)
	}
	return out
}

// --- request context 注入 ---

type identityKey struct{}
type baseURLKey struct{}

// withIdentity 注入身份与审计 actor（中间件调用）。
func withIdentity(ctx context.Context, id *identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// withBaseURL 注入本次请求的对外基础地址（scheme://host，直传 curl 模板用）。
func withBaseURL(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, baseURLKey{}, base)
}

// baseURLFrom 取基础地址（"" = 未知，调用方降级为相对路径提示）。
func baseURLFrom(ctx context.Context) string {
	base, _ := ctx.Value(baseURLKey{}).(string)
	return base
}

// identityFrom 取请求身份（SDK 工具 handler 的 ctx 即 HTTP 请求 context，
// 与 Worker 侧 audit.ActorFromContext 同一传播机制）。
func identityFrom(ctx context.Context) *identity {
	id, _ := ctx.Value(identityKey{}).(*identity)
	return id
}

// bearerToken 提取 Authorization: Bearer <secret>。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return ""
}

// Middleware MCP 端点中间件链：鉴权 → 工具调用计数。不走平台会话 token
// （两类凭据模型独立，见设计方案 §4.1）。失败 401 并带 WWW-Authenticate
// 供客户端发现。
func (h *Handler) Middleware() gin.HandlerFunc {
	auth := h.authMW()
	usage := h.usageMW()
	return func(c *gin.Context) {
		auth(c)
		if c.IsAborted() {
			return
		}
		usage(c)
	}
}

// authMW bearer 校验 + 身份/基础地址注入。
func (h *Handler) authMW() gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := h.authenticateAny(bearerToken(c.Request))
		if !ok {
			c.Header("WWW-Authenticate", `Bearer realm="opsguard-mcp"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "missing or invalid MCP token (configure Authorization: Bearer <mcp token secret>)",
			})
			return
		}
		id.Actor = id.Name + "@" + c.ClientIP()
		proto := c.GetHeader("X-Forwarded-Proto")
		if proto == "" {
			if c.Request.TLS != nil {
				proto = "https"
			} else {
				proto = "http"
			}
		}
		ctx := withIdentity(c.Request.Context(), id)
		ctx = withBaseURL(ctx, proto+"://"+c.Request.Host)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// writeGuard 写工具统一守卫：write scope + 明确错误文本（助手能向用户
// 解释为什么被拒、该怎么开通）。
func (h *Handler) writeGuard(ctx context.Context, tool string) error {
	id := identityFrom(ctx)
	if id == nil {
		return fmt.Errorf("unauthorized: no MCP token identity (bug: middleware not applied)")
	}
	if !id.canWrite() {
		return fmt.Errorf("token %q is read-only; tool %q requires write scope — "+
			"ask the platform admin to issue a scope=write token for changes", id.Name, tool)
	}
	return nil
}
