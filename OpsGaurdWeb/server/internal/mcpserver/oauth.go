// OAuth 2.1 接入（RFC 9728）：/mcp 作为 OAuth 保护资源对外宣告元数据，
// 并在静态/运行时 token 表未命中时，回退接受本进程内嵌 IdP 签发的
// access token（验签 + 吊销同口径）。scope 映射：token scope 含
// mcp:write → write、mcp:exec → exec，其余一律 read（最小授权）。
package mcpserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// IdPTokenValidator 内嵌 IdP 的 token 校验能力（idp.Service 适配注入；
// nil = 未启用 IdP，/mcp 仅接受静态/运行时 token）。
type IdPTokenValidator interface {
	// ValidateAccessToken 校验 IdP access token，返回 subject/username/scope。
	ValidateAccessToken(raw string) (subject, username, scope string, err error)
}

// idpScopeScope 把 IdP token 的 scope 串映射为 MCP 身份档位：
// mcp:exec > mcp:write > 默认 read。
func idpScopeScope(scope string) Scope {
	for _, s := range strings.Fields(scope) {
		switch s {
		case "mcp:exec":
			return ScopeExec
		case "mcp:write":
			return ScopeWrite
		}
	}
	return ScopeRead
}

// authenticateIdP 静态表未命中时的 IdP 回退校验。
func (h *Handler) authenticateIdP(secret string) (*identity, bool) {
	if h.deps.IdP == nil || secret == "" {
		return nil, false
	}
	sub, username, scope, err := h.deps.IdP.ValidateAccessToken(secret)
	if err != nil || (sub == "" && username == "") {
		return nil, false
	}
	name := username
	if name == "" {
		name = sub
	}
	return &identity{Name: "idp:" + name, Scope: idpScopeScope(scope)}, true
}

// --- RFC 9728 资源元数据 ---

type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
}

// Metadata godoc: GET /.well-known/oauth-protected-resource
// RFC 9728 保护资源元数据（公开端点，供 OAuth 客户端/助手发现授权服务器
// 与所支持的 scopes）。authorization_servers 指向内嵌 IdP（未启用时省略，
// 仅宣告静态 token 语义）。
func (h *Handler) Metadata(c *gin.Context) {
	proto := c.GetHeader("X-Forwarded-Proto")
	if proto == "" {
		if c.Request.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	doc := protectedResourceMetadata{
		Resource:               proto + "://" + c.Request.Host + "/",
		ScopesSupported:        []string{"mcp:read", "mcp:write", "mcp:exec"},
		BearerMethodsSupported: []string{"header"},
	}
	if h.deps.IdP != nil && h.deps.IdPIssuer != "" {
		doc.AuthorizationServers = []string{h.deps.IdPIssuer}
	}
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, doc)
}

// 供鉴权中间件调用的统一入口：先查 token 表，再回退 IdP。
func (h *Handler) authenticateAny(secret string) (*identity, bool) {
	if id, ok := h.auth.authenticate(secret); ok {
		return id, true
	}
	return h.authenticateIdP(secret)
}
