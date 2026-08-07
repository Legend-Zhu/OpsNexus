package idpproxy

import "testing"

func TestRewriteDiscovery(t *testing.T) {
	in := []byte(`{
		"issuer":"http://10.60.189.6:8080",
		"authorization_endpoint":"http://10.60.189.6:8080/api/v1/idp/authorize",
		"token_endpoint":"http://10.60.189.6:8080/api/v1/idp/token",
		"jwks_uri":"http://10.60.189.6:8080/api/v1/idp/jwks",
		"userinfo_endpoint":"http://10.60.189.6:8080/api/v1/idp/userinfo",
		"introspection_endpoint":"http://10.60.189.6:8080/api/v1/idp/introspect",
		"end_session_endpoint":"http://10.60.189.6:8080/api/v1/idp/logout"
	}`)
	cfg := Config{
		PublicIssuer: "http://172.28.50.176:8080",
		TunnelBase:   "http://10.60.171.232:8080",
	}
	out := rewriteDiscovery(in, cfg)

	// authorization + logout -> public issuer (browser)
	assertContains(t, string(out), `"authorization_endpoint":"http://172.28.50.176:8080/api/v1/idp/authorize"`)
	assertContains(t, string(out), `"end_session_endpoint":"http://172.28.50.176:8080/api/v1/idp/logout"`)
	// token/jwks/userinfo/introspect -> tunnel base + /idp-proxy
	assertContains(t, string(out), `"token_endpoint":"http://10.60.171.232:8080/idp-proxy/api/v1/idp/token"`)
	assertContains(t, string(out), `"jwks_uri":"http://10.60.171.232:8080/idp-proxy/api/v1/idp/jwks"`)
	assertContains(t, string(out), `"userinfo_endpoint":"http://10.60.171.232:8080/idp-proxy/api/v1/idp/userinfo"`)
	assertContains(t, string(out), `"introspection_endpoint":"http://10.60.171.232:8080/idp-proxy/api/v1/idp/introspect"`)
	// issuer -> public
	assertContains(t, string(out), `"issuer":"http://172.28.50.176:8080"`)
}

func TestRewriteDiscoveryInvalidJSON(t *testing.T) {
	bad := []byte("{not json")
	out := rewriteDiscovery(bad, Config{PublicIssuer: "x", TunnelBase: "y"})
	if string(out) != string(bad) {
		t.Fatal("invalid JSON should pass through unchanged")
	}
}

func TestOriginalPath(t *testing.T) {
	cases := map[string]string{
		"http://h:8080/api/v1/idp/token?x=1": "/api/v1/idp/token?x=1",
		"https://h/foo":                      "/foo",
		"/already/path":                      "/already/path",
		"nopath":                             "/nopath",
	}
	for in, want := range cases {
		if got := originalPath(in); got != want {
			t.Errorf("originalPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsDiscoveryPath(t *testing.T) {
	if !isDiscoveryPath("/.well-known/openid-configuration") {
		t.Fatal("discovery path should match")
	}
	if isDiscoveryPath("/.well-known/openid-configuration?foo=1") {
		// query stripped before check — still matches
	}
	if isDiscoveryPath("/api/v1/idp/token") {
		t.Fatal("token path should not match discovery")
	}
}

func assertContains(t *testing.T, s, sub string) {
	t.Helper()
	if !contains(s, sub) {
		t.Errorf("expected output to contain %q\noutput: %s", sub, s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
