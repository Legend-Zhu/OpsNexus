package grpcapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
)

// TunnelManager owns the reverse-tunnel bidi stream opened by the management
// server. The Worker's local HTTP handlers — /idp-proxy/ (IdP) and /v2/
// (registry image pulls) — forward in-cluster HTTP requests through
// TunnelManager.RoundTrip / RoundTripStream; they ride the server-initiated
// Tunnel stream to the server (which proxies them to its local IdP / OCI
// registry) and the response comes back on the same stream, paired by frame id.
//
// Two response modes share one stream (multiplexed by frame id):
//   - unary  (RoundTrip, IdP): one response frame per request id.
//   - stream (RoundTripStream, registry): the server splits large bodies into
//     chunked frames (chunk_seq 0..N-1, last one chunk_eof=true); the worker
//     reassembles them on the fly into the HTTP response body.
//
// Lifecycle: the management server opens exactly one Tunnel stream per Worker
// and keeps it alive (reopening on disconnect, like SubscribeEvents). The
// Worker side learns of the stream when its Tunnel handler runs; it registers
// the stream here so the HTTP handlers can use it. When the stream ends the
// handler unregisters it and in-flight round trips fail fast.
type TunnelManager struct {
	log *slog.Logger

	mu     sync.RWMutex
	stream pb.ManagementService_TunnelServer // current stream; nil when none attached
	// pending: unary waiters — frame id -> chan receiving the single response
	// frame (consumed by RoundTrip).
	pending map[string]chan *pb.TunnelFrame
	// streams: streaming waiters — frame id -> waiter receiving one frame at a
	// time until chunk_eof (consumed by RoundTripStream).
	streams map[string]*streamWaiter
}

// streamWaiter is one in-flight streaming round trip. ch carries response
// frames (buffered so the tunnel recv loop does not stall on a slow HTTP
// writer); cancel is released by the reader's Close or by detach on stream
// loss, unblocking pending Reads.
type streamWaiter struct {
	ch     chan *pb.TunnelFrame
	cancel context.CancelFunc
}

// NewTunnelManager constructs a TunnelManager (no stream attached yet).
func NewTunnelManager(log *slog.Logger) *TunnelManager {
	if log == nil {
		log = slog.Default()
	}
	return &TunnelManager{
		log:     log,
		pending: map[string]chan *pb.TunnelFrame{},
		streams: map[string]*streamWaiter{},
	}
}

// Tunnel implements the gRPC bidi handler. It runs for the lifetime of one
// server-initiated stream: it registers the stream, loops receiving response
// frames (dispatching each to its pending waiter), and unregisters on exit.
// Send-side (forwarding local HTTP requests) happens via RoundTrip[Stream],
// which writes to the registered stream under its own concurrency.
func (m *TunnelManager) Tunnel(stream pb.ManagementService_TunnelServer) error {
	m.attach(stream)
	defer m.detach()

	// Loop receiving response frames. Each carries the id of a pending request;
	// deliver it to that request's waiter.
	for {
		frame, err := stream.Recv()
		if err != nil {
			m.log.Debug("tunnel stream recv ended", "err", err)
			return err
		}
		m.dispatch(frame)
	}
}

// dispatch delivers one response frame to its waiter: unary waiters consume
// exactly one frame (their entry is removed); streaming waiters keep their
// entry until the reader is done or the stream ends. Frames with no waiter
// (late/orphaned after reader Close) are dropped. Sends carry a 5s guard so a
// wedged waiter can never stall the shared tunnel stream.
func (m *TunnelManager) dispatch(frame *pb.TunnelFrame) {
	// Unary path (existing semantics): take the waiter so a second frame for
	// the same id is dropped.
	if ch, ok := m.takePending(frame.Id); ok {
		select {
		case ch <- frame:
		case <-time.After(5 * time.Second):
			m.log.Warn("tunnel dispatch: unary waiter full, dropping frame", "id", frame.Id)
		}
		return
	}
	// Streaming path: peek; keep registered until ChunkEof or reader Close.
	m.mu.RLock()
	w := m.streams[frame.Id]
	m.mu.RUnlock()
	if w == nil {
		m.log.Debug("tunnel frame with no pending waiter", "id", frame.Id)
		return
	}
	select {
	case w.ch <- frame:
	case <-time.After(5 * time.Second):
		// Reader stopped draining without closing (the handler always closes on
		// return, so this is a defensive fallback). Drop rather than stall.
		m.log.Warn("tunnel dispatch: stream waiter full, dropping frame", "id", frame.Id)
	case <-m.streamDone():
	}
}

// streamDone returns a channel closed when the attached stream ends, used by
// dispatch to unblock sends when the server dropped the stream.
func (m *TunnelManager) streamDone() <-chan struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stream == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return m.stream.Context().Done()
}

// Available reports whether a tunnel stream is currently attached and usable.
func (m *TunnelManager) Available() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stream != nil
}

// RoundTrip forwards one HTTP request over the tunnel and waits for the paired
// response (or timeout). Returns the response frame, or an error if no tunnel
// is attached, the send fails, or the response does not arrive in time. Used
// by the IdP proxy (small bodies, single response frame).
func (m *TunnelManager) RoundTrip(method, path string, headers http.Header, body []byte, timeout time.Duration) (*pb.TunnelFrame, error) {
	m.mu.RLock()
	stream := m.stream
	m.mu.RUnlock()
	if stream == nil {
		return nil, fmt.Errorf("idp tunnel not available (management server not connected)")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	id := randomFrameID()
	ch := m.addPending(id)
	defer m.takePending(id) // ensure cleanup on any return path

	reqFrame := &pb.TunnelFrame{
		Id:      id,
		Method:  method,
		Path:    path,
		Headers: headerToPB(headers),
		Body:    body,
	}
	if err := stream.Send(reqFrame); err != nil {
		return nil, fmt.Errorf("tunnel send: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return resp, fmt.Errorf("%s", resp.Error)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("tunnel round-trip timed out after %s", timeout)
	case <-stream.Context().Done():
		return nil, fmt.Errorf("tunnel stream closed: %w", stream.Context().Err())
	}
}

// requestChunkSize 是隧道请求方向单帧 body 上限：push（blob 分块/镜像层）
// 的请求体较大，worker 把它切成 requestChunkSize 的帧流式发送，末帧
// chunk_eof=true；GET/HEAD 等无体请求仍是单帧（chunk_eof=true）。
const requestChunkSize = 2 << 20

// RoundTripStream forwards one HTTP request over the tunnel and returns the
// response status/headers plus a streaming body reader. The server replies
// with one frame (chunk_eof=true) or several chunked frames; the reader
// reassembles them and reports EOF at chunk_eof (or when Content-Length is
// met — belt-and-braces for old servers that send a single frame without the
// eof flag). The request body (io.Reader) is streamed as chunked request
// frames, so large push bodies (blob uploads) never sit in memory. ctx
// cancels the round trip (callers pass the HTTP request context); Close on
// the returned reader releases the tunnel slot.
func (m *TunnelManager) RoundTripStream(ctx context.Context, method, path string, headers http.Header, body io.Reader) (int, http.Header, io.ReadCloser, error) {
	m.mu.RLock()
	stream := m.stream
	m.mu.RUnlock()
	if stream == nil {
		return 0, nil, nil, fmt.Errorf("registry tunnel not available (management server not connected)")
	}

	id := randomFrameID()
	ctx2, cancel := context.WithCancel(ctx)
	w := &streamWaiter{ch: make(chan *pb.TunnelFrame, 16), cancel: cancel}
	m.addStream(id, w)
	// The waiter slot is released by the reader's Close (not here — the
	// reader outlives this function); error paths below release it explicitly.

	// Stream the request body as frames: first frame carries method/path/
	// headers + first chunk; subsequent frames carry only body; the last frame
	// sets chunk_eof=true. A body that fits in one chunk (or is nil) becomes a
	// single frame with chunk_eof=true. Every frame marks ReqChunked=true so the
	// server distinguishes new-worker chunked requests from legacy single-frame
	// ones (old workers never set it).
	if body == nil {
		body = bytes.NewReader(nil)
	}
	buf := make([]byte, requestChunkSize)
	seq := int32(0)
	first := true
	for {
		n, rErr := body.Read(buf)
		if n > 0 {
			frame := &pb.TunnelFrame{
				Id:         id,
				Body:       buf[:n],
				ChunkSeq:   seq,
				ChunkEof:   rErr == io.EOF,
				ReqChunked: true,
			}
			if first {
				frame.Method = method
				frame.Path = path
				frame.Headers = headerToPB(headers)
				first = false
			}
			seq++
			if err := stream.Send(frame); err != nil {
				m.removeStream(id)
				return 0, nil, nil, fmt.Errorf("tunnel send: %w", err)
			}
		}
		if rErr == io.EOF {
			if first {
				// Empty body: still emit one frame carrying method/path/headers.
				frame := &pb.TunnelFrame{
					Id: id, Method: method, Path: path, Headers: headerToPB(headers), ChunkEof: true, ReqChunked: true,
				}
				if err := stream.Send(frame); err != nil {
					m.removeStream(id)
					return 0, nil, nil, fmt.Errorf("tunnel send: %w", err)
				}
			}
			break
		}
		if rErr != nil {
			m.removeStream(id)
			return 0, nil, nil, fmt.Errorf("tunnel request body read: %w", rErr)
		}
		if n == 0 {
			// (0, nil) — transient; loop again.
			continue
		}
	}

	// First response frame carries status/headers; subsequent frames only body.
	var respFirst *pb.TunnelFrame
	select {
	case respFirst = <-w.ch:
	case <-ctx2.Done():
		m.removeStream(id)
		return 0, nil, nil, fmt.Errorf("tunnel stream round trip canceled: %w", ctx2.Err())
	case <-stream.Context().Done():
		m.removeStream(id)
		return 0, nil, nil, fmt.Errorf("tunnel stream closed: %w", stream.Context().Err())
	}
	status := int(respFirst.Status)
	respHeader := PBHeaderToHTTP(respFirst.Headers)
	if respFirst.Error != "" {
		m.removeStream(id)
		return status, respHeader, nil, fmt.Errorf("%s", respFirst.Error)
	}
	r := &tunnelStreamReader{
		m:        m,
		id:       id,
		w:        w,
		ctx:      ctx2,
		cl:       contentLengthOf(respHeader),
		received: 0,
		eof:      respFirst.ChunkEof,
		buf:      respFirst.Body,
	}
	return status, respHeader, r, nil
}

// contentLengthOf returns the integer Content-Length of h, or -1 if absent.
func contentLengthOf(h http.Header) int64 {
	if v := h.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// tunnelStreamReader is an io.ReadCloser over chunked frames. Read serves the
// pending bytes of the current frame (or waits for the next one) and reports
// io.EOF when the server sent chunk_eof or Content-Length is satisfied. Close
// releases the tunnel slot (cancels the round trip and removes the waiter).
type tunnelStreamReader struct {
	m         *TunnelManager
	id        string
	w         *streamWaiter
	ctx       context.Context // canceled on Close or stream loss
	cl        int64           // upstream Content-Length, -1 unknown
	received  int64           // bytes served so far
	eof       bool            // no more data is coming
	buf       []byte          // pending bytes of the current frame
	closeOnce sync.Once
}

func (r *tunnelStreamReader) Read(p []byte) (int, error) {
	for {
		if r.eof && len(r.buf) == 0 {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		// Serve pending bytes of the current frame first.
		if len(r.buf) > 0 {
			n := copy(p, r.buf)
			r.buf = r.buf[n:]
			r.received += int64(n)
			if r.cl > 0 && r.received >= r.cl {
				r.eof = true
			}
			return n, nil
		}
		// Fetch the next frame.
		select {
		case f := <-r.w.ch:
			if f.Error != "" {
				r.eof = true
				return 0, fmt.Errorf("%s", f.Error)
			}
			r.buf = f.Body
			if f.ChunkEof {
				r.eof = true
			}
			// Content-Length short-circuit: old servers send a single frame
			// without the eof flag; stop once the advertised length is met.
			if r.cl > 0 && r.received+int64(len(f.Body)) >= r.cl {
				r.eof = true
			}
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

func (r *tunnelStreamReader) Close() error {
	r.closeOnce.Do(func() { r.m.removeStream(r.id) })
	return nil
}

// attach registers a freshly opened server-side stream.
func (m *TunnelManager) attach(stream pb.ManagementService_TunnelServer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stream = stream
	m.log.Info("tunnel attached")
}

// detach clears the stream and fails all pending waiters (their channels are
// closed so they return immediately; stream waiters are canceled so their
// reads unblock with an error). A subsequent server reconnect re-attaches.
func (m *TunnelManager) detach() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stream = nil
	for id, ch := range m.pending {
		close(ch)
		delete(m.pending, id)
	}
	for id, w := range m.streams {
		w.cancel()
		delete(m.streams, id)
	}
	m.log.Info("tunnel detached")
}

func (m *TunnelManager) addPending(id string) chan *pb.TunnelFrame {
	ch := make(chan *pb.TunnelFrame, 1)
	m.mu.Lock()
	m.pending[id] = ch
	m.mu.Unlock()
	return ch
}

// takePending removes and returns the pending channel for id; ok=false if none.
func (m *TunnelManager) takePending(id string) (chan *pb.TunnelFrame, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	return ch, ok
}

func (m *TunnelManager) addStream(id string, w *streamWaiter) {
	m.mu.Lock()
	m.streams[id] = w
	m.mu.Unlock()
}

func (m *TunnelManager) removeStream(id string) {
	m.mu.Lock()
	if w, ok := m.streams[id]; ok {
		w.cancel()
		delete(m.streams, id)
	}
	m.mu.Unlock()
}

// RoundTripHTTP is an idpproxy.TunnelSender-compatible adapter around
// RoundTrip: it forwards the request and unpacks the response frame into the
// raw (status, header, body) fields the HTTP handler expects.
func (m *TunnelManager) RoundTripHTTP(method, path string, headers http.Header, body []byte, timeout time.Duration) (int, http.Header, []byte, error) {
	resp, err := m.RoundTrip(method, path, headers, body, timeout)
	if err != nil && resp == nil {
		return 0, nil, nil, err
	}
	return int(resp.Status), PBHeaderToHTTP(resp.Headers), resp.Body, err
}

// headerToPB converts an http.Header to repeated TunnelHeader entries
// (one entry per value, preserving multi-valued headers).
func headerToPB(h http.Header) []*pb.TunnelHeader {
	out := make([]*pb.TunnelHeader, 0, len(h))
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, &pb.TunnelHeader{Key: k, Value: v})
		}
	}
	return out
}

// PBHeaderToHTTP is the inverse, exported for the idpproxy handler.
func PBHeaderToHTTP(hs []*pb.TunnelHeader) http.Header {
	h := http.Header{}
	for _, h2 := range hs {
		h.Add(h2.Key, h2.Value)
	}
	return h
}

func randomFrameID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
