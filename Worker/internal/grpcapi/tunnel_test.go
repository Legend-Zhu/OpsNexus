package grpcapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
	"google.golang.org/grpc/metadata"
)

// fakeTunnelStream simulates the server side of the Tunnel bidi stream: the
// test "server" pushes response frames into respCh; TunnelManager.Tunnel
// receives them via Recv and dispatches to the right waiter. Send records the
// forwarded request so tests can inspect what was sent upstream.
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
		reqs:   make(chan *pb.TunnelFrame, 16),
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

// startTunnel runs the Tunnel recv loop against the fake stream in the
// background (so dispatch executes on the real code path) and returns the
// manager. The stream is torn down (ctx canceled) at test end so the recv loop
// exits.
func startTunnel(t *testing.T, f *fakeTunnelStream) *TunnelManager {
	t.Helper()
	m := NewTunnelManager(nil)
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
			t.Error("tunnel loop did not stop")
		}
	})
	// Wait until the stream is attached.
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
// releases the waiter on Close.
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
	// Waiter must have been released by Close.
	m.mu.RLock()
	n := len(m.streams)
	m.mu.RUnlock()
	if n != 0 {
		t.Fatalf("stream waiters left after Close: %d", n)
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
// error (and the waiter is cleaned up).
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
	if n := len(m.streams); n != 0 {
		t.Fatalf("stream waiters left after error: %d", n)
	}
}

// TestRoundTripStreamNotAttached verifies a fast-fail when no tunnel stream is
// connected.
func TestRoundTripStreamNotAttached(t *testing.T) {
	m := NewTunnelManager(nil)
	_, _, _, err := m.RoundTripStream(context.Background(), "GET", "/v2/foo", nil, nil)
	if err == nil {
		t.Fatal("expected error when no tunnel attached")
	}
}

// TestRoundTripStreamCanceled verifies a canceled caller context aborts the
// wait for the first frame.
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

// TestRoundTripStreamStreamLoss verifies Read unblocks with the stream's
// context error when the server drops the stream mid-body.
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
	n, rErr := body.Read(buf)
	if n != 5 || string(buf[:n]) != "part1" {
		t.Fatalf("first read = %d %q, want 5 'part1'", n, buf[:n])
	}
	if _, rErr = body.Read(buf); rErr == nil {
		t.Fatal("expected read error after stream loss")
	}
	body.Close()
}
