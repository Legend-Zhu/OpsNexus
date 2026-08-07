// Package idpproxy exposes a local HTTP entry point (/idp-proxy/) that
// in-cluster services (e.g. r-nacos OIDC console) use as their IdP issuer.
// Requests are forwarded over the reverse gRPC tunnel
// (grpcapi.TunnelManager) to the management server, which proxies them to its
// local IdP. This avoids opening a reverse firewall hole from the cluster to
// the management plane.
//
// Endpoint rewriting: OIDC authorize/logout are browser redirects and must
// reach the management server directly over the public network, while
// discovery/token/jwks/userinfo are server-to-server calls that ride the
// tunnel. The proxy therefore rewrites the discovery document it relays:
//   - authorization_endpoint, end_session_endpoint -> PublicIssuer (browser)
//   - token_endpoint, jwks_uri, userinfo_endpoint, introspection_endpoint
//     -> TunnelBase + /idp-proxy + original path (server-to-server via tunnel)
package idpproxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// TunnelSender is the Worker-side capability needed to forward a request over
// the reverse tunnel (implemented by *grpcapi.TunnelManager). It returns the
// raw response fields directly (no pb dependency in this package).
type TunnelSender interface {
	Available() bool
	RoundTrip(method, path string, headers http.Header, body []byte, timeout time.Duration) (status int, respHeader http.Header, respBody []byte, err error)
}

// Config controls discovery-document rewriting.
type Config struct {
	// PublicIssuer is the management server's externally reachable base
	// (scheme+host[:port]), e.g. "http://172.28.50.176:8080". Browser-bound
	// endpoints (authorize, end_session) are rewritten to this base.
	PublicIssuer string
	// TunnelBase is this Worker's externally reachable base for in-cluster
	// services, e.g. "http://10.60.171.232:8080". Server-to-server endpoints
	// (token, jwks, userinfo, introspect) are rewritten to TunnelBase/idp-proxy.
	TunnelBase string
}

// Handler returns the /idp-proxy/ http.Handler. It is safe to mount under the
// "/idp-proxy/" prefix.
func Handler(cfg Config, sender TunnelSender, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(cfg, sender, log, w, r)
	})
}

func serve(cfg Config, sender TunnelSender, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	if !sender.Available() {
		http.Error(w, `{"error":"idp tunnel unavailable","error_description":"management server not connected"}`, http.StatusBadGateway)
		return
	}

	// Reconstruct the upstream IdP path. Clients request /idp-proxy/<idp-path>;
	// the management server proxies to its local IdP at /<idp-path>, so we strip
	// the /idp-proxy prefix (keeping the leading slash) before forwarding.
	upstreamPath := strings.TrimPrefix(r.URL.Path, "/idp-proxy")
	if upstreamPath == "" {
		upstreamPath = "/"
	}
	if !strings.HasPrefix(upstreamPath, "/") {
		upstreamPath = "/" + upstreamPath
	}
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	// Read request body (non-nil even when empty is fine).
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"invalid_request","error_description":"read body"}`, http.StatusBadRequest)
		return
	}

	resp, respHeader, respBody, err := sender.RoundTrip(r.Method, upstreamPath, r.Header, body, 15*time.Second)
	if err != nil {
		log.Warn("idp-proxy tunnel round-trip failed", "path", upstreamPath, "err", err)
		http.Error(w, `{"error":"server_error","error_description":"tunnel failed"}`, http.StatusBadGateway)
		return
	}

	// Rewrite discovery so browser endpoints point to the public issuer and
	// server-to-server endpoints point back through this tunnel.
	contentType := respHeader.Get("Content-Type")
	if resp == http.StatusOK && strings.Contains(contentType, "application/json") && isDiscoveryPath(upstreamPath) {
		respBody = rewriteDiscovery(respBody, cfg)
	}

	// Copy headers back, skipping ones hop-by-hop frameworks should not forward.
	for k, vs := range respHeader {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp)
	_, _ = w.Write(respBody)
}

func isDiscoveryPath(p string) bool {
	// strip query
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p == "/.well-known/openid-configuration"
}

// rewriteDiscovery returns the discovery JSON with browser endpoints pointed at
// PublicIssuer and server endpoints pointed at TunnelBase/idp-proxy. Fields not
// present are left untouched; unknown fields pass through unchanged.
func rewriteDiscovery(body []byte, cfg Config) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body // not valid JSON; return as-is rather than corrupting
	}
	tunnel := strings.TrimRight(cfg.TunnelBase, "/") + "/idp-proxy"
	pub := cfg.PublicIssuer

	browserKeys := []string{"authorization_endpoint", "end_session_endpoint"}
	tunnelKeys := []string{"token_endpoint", "jwks_uri", "userinfo_endpoint", "introspection_endpoint"}
	// issuer is informational for IdP-signed tokens; keep the public issuer so
	// token iss matches what verifiers expect when fetched from the public face.
	if _, ok := doc["issuer"]; ok && pub != "" {
		doc["issuer"] = pub
	}
	for _, k := range browserKeys {
		if _, ok := doc[k]; ok && pub != "" {
			doc[k] = joinBase(pub, originalPath(doc[k]))
		}
	}
	for _, k := range tunnelKeys {
		if _, ok := doc[k]; ok && cfg.TunnelBase != "" {
			doc[k] = joinBase(tunnel, originalPath(doc[k]))
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// originalPath takes an endpoint URL value and returns its path+query, dropping
// scheme/host so it can be reattached to a different base.
func originalPath(v any) string {
	s, _ := v.(string)
	if i := strings.Index(s, "://"); i >= 0 {
		// scheme://host[/path?query] -> /path?query
		rest := s[i+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			return rest[slash:]
		}
		return "/"
	}
	if strings.HasPrefix(s, "/") {
		return s
	}
	return "/" + s
}

func joinBase(base, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.TrimRight(base, "/") + path
}

func isHopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailers", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
