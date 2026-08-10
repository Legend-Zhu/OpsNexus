package workerproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"
	"google.golang.org/grpc/metadata"
)

// fakeTunnelClient implements pb.ManagementService_TunnelClient for tests: it
// records every frame proxyOneStream sends and ignores Recv (never called by
// the proxy).
type fakeTunnelClient struct {
	ctx  context.Context
	sent chan *pb.TunnelFrame
}

func (f *fakeTunnelClient) Send(m *pb.TunnelFrame) error { f.sent <- m; return nil }
func (f *fakeTunnelClient) Recv() (*pb.TunnelFrame, error) {
	<-make(chan struct{})
	return nil, io.EOF
}
func (f *fakeTunnelClient) Header() (metadata.MD, error) { return nil, nil }
func (f *fakeTunnelClient) Trailer() metadata.MD         { return nil }
func (f *fakeTunnelClient) CloseSend() error             { return nil }
func (f *fakeTunnelClient) Context() context.Context     { return f.ctx }
func (f *fakeTunnelClient) SendMsg(any) error            { return nil }
func (f *fakeTunnelClient) RecvMsg(any) error            { return nil }

// collect runs proxyOneStream to completion and gathers the sent frames.
func collect(t *testing.T, srvBase string, relay RelayConfig, f *pb.TunnelFrame) []*pb.TunnelFrame {
	t.Helper()
	return collectBody(t, srvBase, relay, f, nil)
}

// collectBody is collect with an explicit request body reader.
func collectBody(t *testing.T, srvBase string, relay RelayConfig, f *pb.TunnelFrame, body []byte) []*pb.TunnelFrame {
	t.Helper()
	cli := &fakeTunnelClient{ctx: context.Background(), sent: make(chan *pb.TunnelFrame, 64)}
	proxyOneStream(cli, &http.Client{Timeout: 5 * time.Second}, &http.Client{}, srvBase, relay, f, bytes.NewReader(body))
	var frames []*pb.TunnelFrame
	for len(cli.sent) > 0 {
		frames = append(frames, <-cli.sent)
	}
	return frames
}

// TestProxyOneStreamChunksLargeBody feeds a 5MiB upstream body through
// proxyOneStream and verifies it is split into ≤ chunk-size frames with the
// first carrying status+headers and the last carrying chunk_eof.
func TestProxyOneStreamChunksLargeBody(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 5<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "sha256:abc")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	frames := collect(t, srv.URL, RelayConfig{}, &pb.TunnelFrame{Id: "f1", Method: http.MethodGet, Path: "/v2/library/x/blobs/sha256:abc"})
	if len(frames) < 3 {
		t.Fatalf("got %d frames, want ≥3 for a 5MiB body", len(frames))
	}
	var total int
	for i, fr := range frames {
		if fr.Id != "f1" {
			t.Fatalf("frame %d has id %q", i, fr.Id)
		}
		if int(fr.ChunkSeq) != i {
			t.Fatalf("frame %d has chunk_seq %d", i, fr.ChunkSeq)
		}
		if len(fr.Body) > tunnelChunkSize {
			t.Fatalf("frame %d body %d > chunk size %d", i, len(fr.Body), tunnelChunkSize)
		}
		total += len(fr.Body)
	}
	if total != len(payload) {
		t.Fatalf("relayed %d bytes, want %d", total, len(payload))
	}
	if !frames[len(frames)-1].ChunkEof {
		t.Fatal("last frame must carry chunk_eof")
	}
	if frames[0].Status != http.StatusOK {
		t.Fatalf("first frame status = %d", frames[0].Status)
	}
	if got := frameHeader(frames[0], "Docker-Content-Digest"); got != "sha256:abc" {
		t.Fatalf("digest header not on first frame: %q", got)
	}
}

// TestProxyOneStreamSingleFrameSmallBody verifies a small response stays a
// single frame (status+headers+chunk_eof).
func TestProxyOneStreamSingleFrameSmallBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	frames := collect(t, srv.URL, RelayConfig{}, &pb.TunnelFrame{Id: "m1", Method: http.MethodGet, Path: "/v2/library/x/manifests/latest"})
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Status != http.StatusOK || !frames[0].ChunkEof {
		t.Fatalf("status=%d chunk_eof=%v", frames[0].Status, frames[0].ChunkEof)
	}
	if string(frames[0].Body) != "{}" {
		t.Fatalf("body = %q", frames[0].Body)
	}
}

// TestProxyOneStreamDeniesNonWhitelistedPath verifies the tunnel relay refuses
// paths outside the IdP/registry allowlist.
func TestProxyOneStreamDeniesNonWhitelistedPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	frames := collect(t, srv.URL, RelayConfig{}, &pb.TunnelFrame{Id: "x1", Method: http.MethodGet, Path: "/api/v1/health"})
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", frames[0].Status)
	}
	if frames[0].Error == "" {
		t.Fatal("denied frame must carry an error message")
	}
}

// TestProxyOneStreamInjectsRegistryCreds verifies the relay injects the
// configured basic credentials for /v2 requests only.
func TestProxyOneStreamInjectsRegistryCreds(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	relay := RelayConfig{RegistryUser: "relay", RegistryPass: "secret"}
	frames := collect(t, srv.URL, relay, &pb.TunnelFrame{Id: "r1", Method: http.MethodGet, Path: "/v2/library/x/blobs/sha256:abc"})
	if gotUser != "relay" || gotPass != "secret" {
		t.Fatalf("registry basic auth = %q/%q, want relay/secret", gotUser, gotPass)
	}
	if frames[0].Status != http.StatusOK {
		t.Fatalf("status = %d", frames[0].Status)
	}
}

// TestAllowedPath verifies the relay allowlist.
func TestAllowedPath(t *testing.T) {
	relay := RelayConfig{}
	allowed := []string{
		"/.well-known/openid-configuration",
		"/api/v1/idp/token",
		"/api/v1/idp/jwks",
		"/v2",
		"/v2/",
		"/v2/library/x/manifests/latest",
	}
	denied := []string{"/api/v1/auth/login", "/healthz", "/api/v1/services", "/v2evil"}
	for _, p := range allowed {
		if !allowedPath(p, relay) {
			t.Errorf("%q should be allowed", p)
		}
	}
	for _, p := range denied {
		if allowedPath(p, relay) {
			t.Errorf("%q should be denied", p)
		}
	}
}

func frameHeader(f *pb.TunnelFrame, key string) string {
	for _, h := range f.Headers {
		if h.Key == key {
			return h.Value
		}
	}
	return ""
}

// TestProxyOneStreamForwardsRequestBody verifies a push request body is
// streamed to the upstream registry.
func TestProxyOneStreamForwardsRequestBody(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	payload := []byte("blob-upload-chunk")
	frames := collectBody(t, srv.URL, RelayConfig{}, &pb.TunnelFrame{Id: "p1", Method: http.MethodPatch, Path: "/v2/library/x/blobs/uploads/uuid1"}, payload)
	if len(frames) != 1 || frames[0].Status != http.StatusCreated || !frames[0].ChunkEof {
		t.Fatalf("frames = %+v", frames)
	}
	if string(got) != string(payload) {
		t.Fatalf("upstream body = %q, want %q", got, payload)
	}
}

// TestProxyOneStreamLocationRelativized verifies an absolute Location header
// from the upstream is rewritten to a path so the caller (dockerd) resolves it
// against the relay endpoint, not the management server's host.
func TestProxyOneStreamLocationRelativized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://10.60.189.6:8080/v2/library/x/blobs/uploads/uuid9")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	frames := collectBody(t, srv.URL, RelayConfig{}, &pb.TunnelFrame{Id: "u1", Method: http.MethodPost, Path: "/v2/library/x/blobs/uploads/"}, nil)
	if len(frames) != 1 {
		t.Fatalf("frames = %d", len(frames))
	}
	if got := frameHeader(frames[0], "Location"); got != "/v2/library/x/blobs/uploads/uuid9" {
		t.Fatalf("Location = %q, want relativized path", got)
	}
}

// TestCollectRequestBodyMultiFrame verifies multi-frame request body assembly.
func TestCollectRequestBodyMultiFrame(t *testing.T) {
	chunks := [][]byte{[]byte("aaa"), []byte("bbb"), []byte("ccc")}
	first := &pb.TunnelFrame{Id: "m1", ChunkSeq: 0, ReqChunked: true, Body: chunks[0]}
	var queue []*pb.TunnelFrame
	for i, c := range chunks[1:] {
		queue = append(queue, &pb.TunnelFrame{Id: "m1", ChunkSeq: int32(i + 1), ReqChunked: true, Body: c, ChunkEof: i == len(chunks)-2})
	}
	cli := &seqTunnelClient{queue: queue}
	rd, err := collectRequestBody(cli, first)
	if err != nil {
		t.Fatalf("collectRequestBody: %v", err)
	}
	all, _ := io.ReadAll(rd)
	if string(all) != "aaabbbccc" {
		t.Fatalf("assembled = %q", all)
	}
}

// seqTunnelClient feeds a fixed queue of frames to collectRequestBody.
type seqTunnelClient struct {
	queue []*pb.TunnelFrame
}

func (f *seqTunnelClient) Send(*pb.TunnelFrame) error { return nil }
func (f *seqTunnelClient) Recv() (*pb.TunnelFrame, error) {
	if len(f.queue) == 0 {
		return nil, io.EOF
	}
	m := f.queue[0]
	f.queue = f.queue[1:]
	return m, nil
}
func (f *seqTunnelClient) Header() (metadata.MD, error) { return nil, nil }
func (f *seqTunnelClient) Trailer() metadata.MD         { return nil }
func (f *seqTunnelClient) CloseSend() error             { return nil }
func (f *seqTunnelClient) Context() context.Context     { return context.Background() }
func (f *seqTunnelClient) SendMsg(any) error            { return nil }
func (f *seqTunnelClient) RecvMsg(any) error            { return nil }
