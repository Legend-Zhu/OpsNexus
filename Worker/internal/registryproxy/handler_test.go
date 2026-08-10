package registryproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStreamer implements TunnelStreamer with a canned response body.
type fakeStreamer struct {
	status  int
	headers http.Header
	body    string
	bodyErr error
	attach  bool
	gotPath string
	gotMeth string
	gotBody string // request body captured by RoundTripStream
}

func (f *fakeStreamer) Available() bool { return f.attach }

func (f *fakeStreamer) RoundTripStream(ctx context.Context, method, path string, headers http.Header, body io.Reader) (int, http.Header, io.ReadCloser, error) {
	f.gotPath = path
	f.gotMeth = method
	if body != nil {
		if b, err := io.ReadAll(body); err == nil {
			f.gotBody = string(b)
		}
	}
	if f.bodyErr != nil {
		return 0, nil, nil, f.bodyErr
	}
	return f.status, f.headers, io.NopCloser(strings.NewReader(f.body)), nil
}

func doReq(t *testing.T, s TunnelStreamer, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	return doReqCache(t, s, nil, method, target)
}

func doReqCache(t *testing.T, s TunnelStreamer, cache *BlobCache, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	return doReqBody(t, s, cache, method, target, nil)
}

// doReqBody is doReqCache with an explicit request body (for push methods).
func doReqBody(t *testing.T, s TunnelStreamer, cache *BlobCache, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	Handler(s, cache, nil).ServeHTTP(rr, req)
	return rr
}

func TestRegistryProxyGetStreamsFullPath(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Length", "5")
	h.Set("Docker-Content-Digest", "sha256:abc")
	s := &fakeStreamer{attach: true, status: 200, headers: h, body: "hello"}
	rr := doReq(t, s, http.MethodGet, "http://node/v2/library/data-server/blobs/sha256:abc")

	if s.gotPath != "/v2/library/data-server/blobs/sha256:abc" {
		t.Fatalf("forwarded path = %q, want full /v2 path", s.gotPath)
	}
	if s.gotMeth != http.MethodGet {
		t.Fatalf("forwarded method = %q", s.gotMeth)
	}
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	if rr.Header().Get("Docker-Content-Digest") != "sha256:abc" {
		t.Fatalf("digest header not relayed")
	}
	if rr.Body.String() != "hello" {
		t.Fatalf("body = %q", rr.Body.String())
	}
}

func TestRegistryProxyHeadNoBody(t *testing.T) {
	s := &fakeStreamer{attach: true, status: 200, body: "should-not-leak"}
	rr := doReq(t, s, http.MethodHead, "http://node/v2/library/x/manifests/latest")
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("HEAD relayed a body: %q", rr.Body.String())
	}
}

func TestRegistryProxyPushForwarded(t *testing.T) {
	s := &fakeStreamer{attach: true, status: 201, body: ""}
	payload := []byte("blob-chunk")
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		rr := doReqBody(t, s, nil, m, "http://node/v2/library/x/blobs/uploads/uuid1", payload)
		if rr.Code != 201 {
			t.Fatalf("%s status = %d, want 201", m, rr.Code)
		}
		if s.gotMeth != m {
			t.Fatalf("%s forwarded as %q", m, s.gotMeth)
		}
		if s.gotBody != string(payload) {
			t.Fatalf("%s body = %q, want %q", m, s.gotBody, payload)
		}
	}
}

func TestRegistryProxyRejectsOtherMethods(t *testing.T) {
	s := &fakeStreamer{attach: true, status: 200}
	for _, m := range []string{http.MethodDelete, http.MethodOptions} {
		rr := doReqBody(t, s, nil, m, "http://node/v2/library/x/blobs/uploads/uuid1", nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d, want 405", m, rr.Code)
		}
	}
}

func TestRegistryProxyNotAttached(t *testing.T) {
	s := &fakeStreamer{attach: false}
	rr := doReq(t, s, http.MethodGet, "http://node/v2/_catalog")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestRegistryProxyTunnelError(t *testing.T) {
	s := &fakeStreamer{attach: true, bodyErr: errors.New("tunnel down")}
	rr := doReq(t, s, http.MethodGet, "http://node/v2/library/x/manifests/latest")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

// TestRegistryProxyBlobCacheMissThenHit verifies the cache path: the first GET
// crosses the tunnel and is cached on success; a second GET for the same blob
// is served from disk without calling the tunnel.
func TestRegistryProxyBlobCacheMissThenHit(t *testing.T) {
	cache, err := OpenBlobCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("OpenBlobCache: %v", err)
	}
	payload := "layer-data"
	h := http.Header{}
	h.Set("Docker-Content-Digest", digest64("aa"))
	s := &fakeStreamer{attach: true, status: 200, headers: h, body: payload}

	target := "http://node/v2/library/app/blobs/" + digest64("aa")
	// Miss: streamed through and cached.
	rr := doReqCache(t, s, cache, http.MethodGet, target)
	if rr.Code != 200 || rr.Body.String() != payload {
		t.Fatalf("miss: status=%d body=%q", rr.Code, rr.Body.String())
	}
	if s.gotPath == "" {
		t.Fatal("tunnel should have been used on the miss")
	}

	// Hit: served from cache, tunnel untouched.
	s.gotPath = ""
	rr = doReqCache(t, s, cache, http.MethodGet, target)
	if rr.Code != 200 || rr.Body.String() != payload {
		t.Fatalf("hit: status=%d body=%q", rr.Code, rr.Body.String())
	}
	if s.gotPath != "" {
		t.Fatalf("tunnel used on cached blob: %q", s.gotPath)
	}
	if rr.Header().Get("Docker-Content-Digest") != digest64("aa") {
		t.Fatal("hit response missing Docker-Content-Digest")
	}
}

// TestRegistryProxyBlobNon200NotCached ensures an upstream failure does not
// poison the cache.
func TestRegistryProxyBlobNon200NotCached(t *testing.T) {
	cache, err := OpenBlobCache(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("OpenBlobCache: %v", err)
	}
	s := &fakeStreamer{attach: true, status: 404, body: `{"errors":[...]}`}
	target := "http://node/v2/library/app/blobs/" + digest64("bb")
	doReqCache(t, s, cache, http.MethodGet, target)
	if _, _, ok := cache.Get(digest64("bb")); ok {
		t.Fatal("404 response must not be cached")
	}
}
