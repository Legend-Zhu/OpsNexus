// Package registryproxy exposes a local /v2/ entry point that in-cluster
// docker engines use to pull images hosted on the management server's embedded
// OCI registry, without a cluster→management network policy: requests ride the
// reverse gRPC tunnel (TunnelManager.RoundTripStream) to the server, which
// proxies them to its local /v2 registry and streams the response back in
// chunks.
//
// The handler is a byte-forwarder, not a registry client: it forwards the full
// request path/headers unchanged and relays the streamed response body, so all
// OCI protocol semantics (manifest content-types, digest headers, blobs) stay
// with the server-side registry implementation. Pull-only: GET/HEAD are
// forwarded; everything else gets 405.
package registryproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// TunnelStreamer is the Worker-side capability needed to relay one registry
// HTTP request over the reverse tunnel with a streaming response body
// (implemented by *grpcapi.TunnelManager).
type TunnelStreamer interface {
	Available() bool
	RoundTripStream(ctx context.Context, method, path string, headers http.Header, body []byte) (status int, respHeader http.Header, bodyStream io.ReadCloser, err error)
}

// Handler returns the /v2/ http.Handler. Mount under "/v2/" so dockerd's
// requests arrive with the full path (/v2/<name>/...), which is forwarded
// unchanged to the management server's registry. cache may be nil (relay
// without the blob cache).
func Handler(s TunnelStreamer, cache *BlobCache, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(s, cache, log, w, r)
	})
}

func serve(s TunnelStreamer, cache *BlobCache, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	if !s.Available() {
		http.Error(w, `{"errors":[{"code":"UNKNOWN","message":"registry tunnel unavailable: management server not connected"}]}`, http.StatusBadGateway)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, `{"errors":[{"code":"DENIED","message":"registry relay is pull-only (GET/HEAD)"}]}`, http.StatusMethodNotAllowed)
		return
	}
	// Forward the full path + query; the /v2/ prefix is significant to the
	// server-side registry and must not be stripped.
	p := r.URL.RequestURI()

	// Cache hit fast path (GET blobs only): serve the layer straight from disk,
	// digest-verified, without crossing the tunnel. Manifests/tags stay passthrough.
	digest, isBlob := blobDigestFromPath(p)
	if isBlob && cache != nil {
		if size, rc, ok := cache.Get(digest); ok {
			defer rc.Close()
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodHead {
				return
			}
			if _, err := io.Copy(w, rc); err != nil {
				log.Warn("registry cache read aborted", "path", p, "err", err)
			}
			return
		}
	}

	status, respHeader, stream, err := s.RoundTripStream(r.Context(), r.Method, p, r.Header, nil)
	if err != nil {
		log.Warn("registry tunnel round trip failed", "path", p, "err", err)
		http.Error(w, `{"errors":[{"code":"UNKNOWN","message":"registry tunnel failed"}]}`, http.StatusBadGateway)
		return
	}
	defer stream.Close()

	// Copy headers back, skipping hop-by-hop ones the local server owns.
	for k, vs := range respHeader {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// A successful GET blob response may be written to the local cache on the
	// fly; the cache only stores verified (200) content.
	var (
		cw       *cacheTmpWriter
		cacheErr error
	)
	if isBlob && cache != nil && status == http.StatusOK && r.Method == http.MethodGet {
		if cw, cacheErr = cache.PutWriter(digest); cacheErr != nil {
			log.Warn("registry cache write disabled", "path", p, "err", cacheErr)
		}
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	// Stream the body through as frames arrive; flush so dockerd sees data
	// progressively instead of one big buffer at the end. When caching, the
	// same bytes are written to the temp file (published only on full success).
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var wrote int64
	for {
		n, rErr := stream.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				log.Debug("registry relay client write aborted", "path", p, "err", wErr)
				return
			}
			if fl != nil {
				fl.Flush()
			}
			wrote += int64(n)
			if cw != nil {
				if _, cErr := cw.Write(buf[:n]); cErr != nil {
					_ = cw.Close() // discard partial temp file
					cw = nil
				}
			}
		}
		if rErr == io.EOF {
			if cw != nil {
				// Full body received: atomically publish the cached blob.
				_ = cw.Publish()
			}
			return
		}
		if rErr != nil {
			log.Warn("registry relay stream aborted mid-body", "path", p, "err", rErr)
			return
		}
	}
}

// blobDigestFromPath extracts the blob digest from a /v2/<name>/blobs/<digest>
// request path, returning ok=false for non-blob requests (manifests, tags,
// ping, catalog).
func blobDigestFromPath(p string) (digest string, ok bool) {
	path := p
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "blobs" && validDigest(segs[i+1]) {
			return segs[i+1], true
		}
	}
	return "", false
}

func isHopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailers", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
