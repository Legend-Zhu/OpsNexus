// Package authz provides bearer-token authentication middleware for the
// Worker's HTTP API and MCP endpoint. Tokens are configured in the agent
// config (`auth.tokens`: name -> secret); the token name becomes the audit
// actor. An OAuth 2.1 Protected Resource Metadata endpoint is served for
// standards-based discovery (RFC 9728).
package authz

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
)

// Middleware wraps a handler with token authentication.
type Middleware struct {
	enabled bool
	tokens  map[string]string // name -> secret
	// PublicPaths are exempt from auth (e.g. OAuth metadata discovery,
	// health checks). Matching is exact on the request path.
	PublicPaths []string
	// AuthorizationServer is the OpsGaurd IdP issuer URL advertised in the
	// RFC 9728 protected-resource metadata (authorization_servers field), so
	// MCP/OAuth clients can discover where to obtain a token. Empty = omit
	// (bearer-token-only deployments).
	AuthorizationServer string
}

// New builds the middleware from the agent auth config.
func New(cfg *agent.AuthConfig) *Middleware {
	m := &Middleware{}
	if cfg != nil {
		m.enabled = cfg.Enabled
		m.tokens = cfg.Tokens
		m.AuthorizationServer = cfg.AuthorizationServer
	}
	return m
}

// Wrap returns a handler that requires a valid bearer token when enabled.
// PublicPaths are exempt.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if !m.enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range m.PublicPaths {
			// Entries ending in '/' match as a path prefix (e.g. "/idp-proxy/"
			// matches "/idp-proxy/.well-known/..."); others match exactly.
			if strings.HasSuffix(p, "/") {
				if strings.HasPrefix(r.URL.Path, p) {
					next.ServeHTTP(w, r)
					return
				}
			} else if r.URL.Path == p {
				next.ServeHTTP(w, r)
				return
			}
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeUnauthorized(w, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		// constant-time comparison over all configured tokens
		for name, secret := range m.tokens {
			if len(secret) == len(token) && subtle.ConstantTimeCompare([]byte(secret), []byte(token)) == 1 {
				// record the token name as the audit actor
				next.ServeHTTP(w, r.WithContext(audit.ContextWithActor(r.Context(), name)))
				return
			}
		}
		writeUnauthorized(w, "invalid bearer token")
	})
}

// ProtectedResourceMetadata is the OAuth 2.1 resource-server metadata
// (RFC 9728) advertised at /.well-known/oauth-protected-resource for
// standards-based client discovery. The authorization-server field is a
// placeholder for deployments that hook an AS; the bearer-token flow itself
// is handled by the middleware above.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
}

// MetadataHandler serves the OAuth 2.1 protected-resource metadata JSON.
// 当 Middleware 配置了 AuthorizationServer（OpsGaurd IdP issuer）时，
// 填入 authorization_servers 字段，供 MCP/OAuth 客户端按 RFC 9728 发现授权服务器。
func MetadataHandler(m *Middleware) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		doc := ProtectedResourceMetadata{
			Resource:               "https://" + r.Host + "/",
			ScopesSupported:        []string{"worker:read", "worker:write"},
			BearerMethodsSupported: []string{"header"},
		}
		if m != nil && m.AuthorizationServer != "" {
			doc.AuthorizationServers = []string{m.AuthorizationServer}
		}
		data, err := json.Marshal(doc)
		if err != nil {
			http.Error(w, `{"error":"metadata serialization failed"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
	})
}

// TLSConfigForTokenAuth is a helper to keep TLS-required deployments honest;
// it is a no-op placeholder so callers can document the intent.
func TLSConfigForTokenAuth() *tls.Config { return nil }

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="opsguard-worker"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized","message":` + quote(msg) + `}`))
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
