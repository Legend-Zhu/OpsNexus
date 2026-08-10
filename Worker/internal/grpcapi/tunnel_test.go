package grpcapi

import (
	"bytes"
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

// TestConcurrentChunkedRequestsSerialized verifies that concurrent chunked
// request bodies never interleave frames on the shared stream: each request's
// frames (identified by id) must arrive contiguously. Without sendMu the
// server-side synchronous collector would see mixed frames and stall.
func TestConcurrentChunkedRequestsSerialized(t *testing.T) {
	f := newFakeTunnelStream()
	m := startTunnel(t, f)

	const nReq = 6
	const chunksPerReq = 3
	// Body must exceed requestChunkSize so it is split into multiple frames.
	bodySize := chunksPerReq*requestChunkSize + 5
	done := make(chan error, nReq)
	for i := 0; i < nReq; i++ {
		go func(i int) {
			body := bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, bodySize))
			_, _, rc, err := m.RoundTripStream(context.Background(), "PATCH", "/v2/x/blobs/uploads/u", nil, body)
			if rc != nil {
				io.Copy(io.Discard, rc)
				rc.Close()
			}
			done <- err
		}(i)
	}
	// Collect all request frames (from all goroutines) off the fake stream.
	// Each request sends up to chunksPerReq+2 frames: chunksPerReq full chunks
	// + 1 remainder (+1 terminating frame when data and EOF arrive in separate
	// Reads). Collect the upper bound so every request's frames are captured.
	var seq []*pb.TunnelFrame
	for len(seq) < nReq*(chunksPerReq+2) {
		select {
		case fr := <-f.reqs:
			seq = append(seq, fr)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d frames", len(seq))
		}
	}
	// The fake stream doesn't answer responses, so RoundTripStream blocks on
	// first response — nothing to join there; send-side is what we assert.

	// Assert: frames are contiguous per request, and every request's last
	// frame carries chunk_eof=true.
	groups := map[string][]*pb.TunnelFrame{}
	for _, fr := range seq {
		groups[fr.Id] = append(groups[fr.Id], fr)
	}
	for id, frames := range groups {
		if len(frames) < 2 {
			t.Fatalf("request %q only sent %d frames", id, len(frames))
		}
		for i := 1; i < len(frames); i++ {
			if frames[i].ChunkSeq != int32(i) {
				t.Fatalf("request %q chunk_seq not contiguous: frame %d = %d", id, i, frames[i].ChunkSeq)
			}
		}
		if !frames[len(frames)-1].ChunkEof {
			t.Fatalf("request %q last frame missing chunk_eof (sent %d frames)", id, len(frames))
		}
	}
	// The stream must not interleave: consecutive frames of different ids are
	// allowed only at request boundaries (previous id's last frame was eof).
	for i := 1; i < len(seq); i++ {
		if seq[i].Id == seq[i-1].Id {
			continue
		}
		if !seq[i-1].ChunkEof {
			t.Fatalf("frame %d switched id %q -> %q without chunk_eof", i, seq[i-1].Id, seq[i].Id)
		}
	}
	// Every chunked frame must carry the request metadata on seq 0.
	seenFirst := map[string]bool{}
	for _, fr := range seq {
		if fr.ChunkSeq == 0 {
			seenFirst[fr.Id] = true
			if fr.Method == "" || fr.Path == "" {
				t.Fatalf("first frame missing method/path: %+v", fr)
			}
		}
	}
	if len(seenFirst) != nReq {
		t.Fatalf("expected %d first frames, got %d", nReq, len(seenFirst))
	}
}
