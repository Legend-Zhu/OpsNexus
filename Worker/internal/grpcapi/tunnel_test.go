package grpcapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
	"google.golang.org/grpc/metadata"
)

// fakeTunnelStream simulates the server side of one Tunnel bidi stream. In the
// pool model each borrowed stream is driven directly by the borrower: Send puts
// request frames into reqs; Recv pulls response frames the test injects into
// respCh. One fake == one pooled stream == one request at a time.
type fakeTunnelStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	respCh chan *pb.TunnelFrame
	reqs   chan *pb.TunnelFrame
}

func newFakeTunnelStream() *fakeTunnelStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeTunnelStream{
		ctx:    ctx,
		cancel: cancel,
		respCh: make(chan *pb.TunnelFrame, 64),
		reqs:   make(chan *pb.TunnelFrame, 256),
	}
}

func (f *fakeTunnelStream) Send(m *pb.TunnelFrame) error { f.reqs <- m; return nil }
func (f *fakeTunnelStream) Recv() (*pb.TunnelFrame, error) {
	select {
	case m, ok := <-f.respCh:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}
func (f *fakeTunnelStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeTunnelStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeTunnelStream) SetTrailer(metadata.MD)       {}
func (f *fakeTunnelStream) Context() context.Context     { return f.ctx }
func (f *fakeTunnelStream) SendMsg(any) error            { return nil }
func (f *fakeTunnelStream) RecvMsg(any) error            { return nil }
func (f *fakeTunnelStream) CloseSend() error             { return nil }

// idleCount returns the number of idle streams currently in the pool. It is a
// snapshot of the idle slice length (under the pool mutex). Used to assert
// Close/release put the stream back.
func (m *TunnelManager) idleCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.idle)
}

// startTunnel registers one fake stream into a fresh manager's pool and waits
// for it to attach. The stream's ctx is canceled at test end so the handler
// returns.
func startTunnel(t *testing.T, f *fakeTunnelStream) *TunnelManager {
	t.Helper()
	m := NewTunnelManager(nil, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Tunnel(f)
	}()
	t.Cleanup(func() {
		f.cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	for i := 0; i < 200 && !m.Available(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !m.Available() {
		t.Fatal("tunnel never attached")
	}
	return m
}

func frameResp(id string, status int, headers http.Header, body []byte, seq int32, eof bool) *pb.TunnelFrame {
	var hs []*pb.TunnelHeader
	for k, vs := range headers {
		for _, v := range vs {
			hs = append(hs, &pb.TunnelHeader{Key: k, Value: v})
		}
	}
	return &pb.TunnelFrame{Id: id, Status: int32(status), Headers: hs, Body: body, ChunkSeq: seq, ChunkEof: eof}
}

// TestRoundTripStreamChunked drives a 5MiB body split into 2MiB frames and
// verifies the reader reassembles it, honors the Content-Length header and
// returns the stream to the pool on Close.
func TestRoundTripStreamChunked(t *testing.T) {
	f := newFakeTunnelStream()
	m := startTunnel(t, f)

	const total = 5 << 20
	full := make([]byte, total)
	for i := range full {
		full[i] = byte(i * 31)
	}

	go func() {
		req := <-f.reqs
		h := http.Header{}
		h.Set("Content-Length", strconv.Itoa(total))
		f.respCh <- frameResp(req.Id, 200, h, full[:2<<20], 0, false)
		f.respCh <- frameResp(req.Id, 0, nil, full[2<<20:4<<20], 1, false)
		f.respCh <- frameResp(req.Id, 0, nil, full[4<<20:], 2, true)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, headers, body, err := m.RoundTripStream(ctx, "GET", "/v2/foo/blobs/sha256:abc", nil, nil)
	if err != nil {
		t.Fatalf("RoundTripStream: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := headers.Get("Content-Length"); got != strconv.Itoa(total) {
		t.Fatalf("Content-Length header = %q, want %d", got, total)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	body.Close()
	if len(got) != total {
		t.Fatalf("read %d bytes, want %d", len(got), total)
	}
	for i := range got {
		if got[i] != full[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], full[i])
		}
	}
	// Clean EOF must have returned the stream to the pool for reuse.
	if n := m.idleCount(); n != 1 {
		t.Fatalf("idle pool count after Close = %d, want 1 (stream not released)", n)
	}
}

// TestRoundTripStreamSingleFrameNoEOF covers an old server that answers with a
// single frame and no chunk_eof: the reader must stop at Content-Length.
func TestRoundTripStreamSingleFrameNoEOF(t *testing.T) {
	f := newFakeTunnelStream()
	m := startTunnel(t, f)

	payload := []byte("single-frame-body")
	go func() {
		req := <-f.reqs
		h := http.Header{}
		h.Set("Content-Length", strconv.Itoa(len(payload)))
		// chunk_eof left false (old server behavior)
		f.respCh <- frameResp(req.Id, 200, h, payload, 0, false)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, body, err := m.RoundTripStream(ctx, "GET", "/v2/foo", nil, nil)
	if err != nil {
		t.Fatalf("RoundTripStream: %v", err)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	body.Close()
	if string(got) != string(payload) {
		t.Fatalf("body = %q, want %q", got, payload)
	}
}

// TestRoundTripStreamErrorFrame checks an error first frame surfaces as an
// error and the stream is released back to the pool (error frame is a complete
// response).
func TestRoundTripStreamErrorFrame(t *testing.T) {
	f := newFakeTunnelStream()
	m := startTunnel(t, f)

	go func() {
		req := <-f.reqs
		f.respCh <- &pb.TunnelFrame{Id: req.Id, Status: 502, ChunkEof: true, Error: "upstream exploded"}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, _, err := m.RoundTripStream(ctx, "GET", "/v2/foo", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if n := m.idleCount(); n != 1 {
		t.Fatalf("idle pool count after error frame = %d, want 1 (stream not released)", n)
	}
}

// TestRoundTripStreamNotAttached verifies a fast-fail when no tunnel stream is
// connected.
func TestRoundTripStreamNotAttached(t *testing.T) {
	m := NewTunnelManager(nil, 4)
	_, _, _, err := m.RoundTripStream(context.Background(), "GET", "/v2/foo", nil, nil)
	if err == nil {
		t.Fatal("expected error when no tunnel attached")
	}
}

// TestRoundTripStreamCanceled verifies a canceled caller context aborts the
// wait for the first frame (and tears down the borrowed stream).
func TestRoundTripStreamCanceled(t *testing.T) {
	f := newFakeTunnelStream()
	m := startTunnel(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-f.reqs // request goes out
		cancel()
	}()
	_, _, _, err := m.RoundTripStream(ctx, "GET", "/v2/foo", nil, nil)
	if err == nil {
		t.Fatal("expected cancel error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

// TestRoundTripStreamStreamLoss verifies Read unblocks with an error when the
// server drops the stream mid-body (the borrowed stream is torn down).
func TestRoundTripStreamStreamLoss(t *testing.T) {
	f := newFakeTunnelStream()
	m := startTunnel(t, f)

	go func() {
		req := <-f.reqs
		h := http.Header{}
		h.Set("Content-Length", "1000")
		f.respCh <- frameResp(req.Id, 200, h, []byte("part1"), 0, false)
		// Give the first frame time to reach the reader, then drop the stream.
		time.Sleep(50 * time.Millisecond)
		f.cancel()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, body, err := m.RoundTripStream(ctx, "GET", "/v2/foo", nil, nil)
	if err != nil {
		t.Fatalf("RoundTripStream: %v", err)
	}
	buf := make([]byte, 1024)
	n, _ := body.Read(buf)
	if n != 5 || string(buf[:n]) != "part1" {
		t.Fatalf("first read = %d %q, want 5 'part1'", n, buf[:n])
	}
	if _, rErr := body.Read(buf); rErr == nil {
		t.Fatal("expected read error after stream loss")
	}
	body.Close()
}

// TestTunnelPoolConcurrent verifies the pool lets N concurrent requests
// proceed in parallel (each on its own stream), the structural replacement for
// the old single-stream non-interleave property. Each fake acts as the server
// for the one request it receives: drain the chunked request body to eof, then
// reply with one response frame.
func TestTunnelPoolConcurrent(t *testing.T) {
	const n = 6
	m := NewTunnelManager(nil, n)
	fakes := make([]*fakeTunnelStream, n)
	for i := range fakes {
		fakes[i] = newFakeTunnelStream()
		f := fakes[i]
		done := make(chan struct{})
		go func() { defer close(done); _ = m.Tunnel(f) }()
		t.Cleanup(func() { f.cancel(); <-done })
	}
	for i := 0; i < 200 && !m.Available(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !m.Available() {
		t.Fatal("tunnel pool never attached")
	}

	// Server-side goroutine per fake: consume the request (drain chunked body
	// to eof) and send a small response so the worker's RoundTripStream returns.
	for _, f := range fakes {
		f := f
		go func() {
			req, ok := <-f.reqs
			if !ok {
				return
			}
			for !req.ChunkEof {
				next, ok := <-f.reqs
				if !ok {
					return
				}
				req = next
			}
			f.respCh <- frameResp(req.Id, 200, nil, []byte("ok"), 0, true)
		}()
	}

	// Body must exceed requestChunkSize so it is split into multiple frames.
	bodySize := 2*requestChunkSize + 5
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines simultaneously
			body := bytes.NewReader(bytes.Repeat([]byte{'x'}, bodySize))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, rc, err := m.RoundTripStream(ctx, "PATCH", "/v2/x/blobs/uploads/u", nil, body)
			if rc != nil {
				io.Copy(io.Discard, rc)
				rc.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent roundtrip failed: %v", err)
		}
	}
}
