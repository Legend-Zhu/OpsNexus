package grpcapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
)

// defaultTunnelPoolSize is the number of bidi Tunnel streams the management
// server pre-opens to this worker. Each forwarded HTTP request borrows one
// stream exclusively, so N concurrent requests each get their own HTTP/2
// flow-control window and recv loop — no head-of-line blocking on a single
// shared stream. Must match the server side's RelayConfig.TunnelConcurrency.
const defaultTunnelPoolSize = 16

// TunnelManager owns a POOL of the reverse-tunnel bidi streams opened by the
// management server. The worker's local HTTP handlers — /idp-proxy/ (IdP) and
// /v2/ (registry image pull/push) — forward in-cluster HTTP requests through
// TunnelManager.RoundTrip / RoundTripStream.
//
// Pool model (replaces the old single-shared-stream multiplexer): each request
// borrows one stream for its lifetime and drives it directly — Send request
// frames, Recv response frames. One stream serves one request at a time, so
// there is no frame interleaving to guard against and no per-id demux: the
// entire sendMu / dispatch / pending / streams machinery is gone. N concurrent
// transfers get N independent HTTP/2 flow-control windows (each sized by the
// gRPC flow-control options in main.go), which is what removes the throughput
// ceiling the old single-stream design hit.
//
// Lifecycle: the management server opens up to poolSize Tunnel streams to this
// worker; each Tunnel handler invocation registers its stream into the pool and
// blocks for the stream's lifetime. A borrower that aborts mid-request (caller
// ctx canceled or a stream error) tears its stream down so the server's relay
// goroutine doesn't stall mid-send; a clean completion returns the stream to
// the pool for reuse. When a stream ends (teardown / server disconnect) the
// handler returns and the server reopens that slot.
type TunnelManager struct {
	log      *slog.Logger
	poolSize int

	mu     sync.Mutex
	pool   chan *tunnelConn         // idle streams available for borrowing (cap = poolSize)
	live   map[*tunnelConn]struct{} // all attached streams (idle + in-flight); membership == alive
	closed bool
	done   chan struct{} // closed when Detach is called, waking all borrowers
}

// tunnelConn wraps one bidi Tunnel stream plus a cancel that tears it down.
type tunnelConn struct {
	stream pb.ManagementService_TunnelServer
	// cancel unblocks the owning Tunnel handler, which returns and thereby
	// closes the gRPC stream. Borrowers call it via teardown when they abort.
	cancel context.CancelFunc
}

// NewTunnelManager constructs a TunnelManager with the given pool size. poolSize
// <= 0 falls back to defaultTunnelPoolSize. No streams are attached until the
// management server opens Tunnel streams (served by Tunnel).
func NewTunnelManager(log *slog.Logger, poolSize int) *TunnelManager {
	if log == nil {
		log = slog.Default()
	}
	if poolSize <= 0 {
		poolSize = defaultTunnelPoolSize
	}
	return &TunnelManager{
		log:      log,
		poolSize: poolSize,
		pool:     make(chan *tunnelConn, poolSize),
		live:     map[*tunnelConn]struct{}{},
		done:     make(chan struct{}),
	}
}

// Tunnel implements the gRPC bidi handler. One invocation per stream the
// management server opens. It registers the stream into the pool (making it
// available to borrowers) and blocks until the stream ends — either the stream
// itself dies (server disconnect / transport error) or a borrower tears it down
// via cancel. On exit the stream is revoked from the live set.
func (m *TunnelManager) Tunnel(stream pb.ManagementService_TunnelServer) error {
	// Derive a cancellable context so borrowers can force this handler to
	// return (closing the stream) when they abort a request mid-flight.
	ctx, cancel := context.WithCancel(stream.Context())
	tc := &tunnelConn{stream: stream, cancel: cancel}
	m.offer(tc)
	defer m.revoke(tc)

	select {
	case <-ctx.Done():
	case <-m.done:
	}
	if err := stream.Context().Err(); err != nil {
		return err
	}
	return nil
}

// Available reports whether at least one tunnel stream is attached and the
// manager isn't shut down. (Idle vs. all-busy is not distinguished — a borrow
// may still block waiting for a stream to be released.)
func (m *TunnelManager) Available() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && len(m.live) > 0
}

// offer registers a freshly attached stream as idle in the pool.
func (m *TunnelManager) offer(tc *tunnelConn) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.live[tc] = struct{}{}
	m.mu.Unlock()
	select {
	case m.pool <- tc:
	default:
		// Pool channel full: the server opened more streams than poolSize.
		// This extra one stays live but unborrowable until something drains.
		// Does not happen when both sides use the same configured size.
	}
}

// revoke drops a stream from the live set. Stale copies still queued in the
// pool channel are skipped by borrow's liveness check.
func (m *TunnelManager) revoke(tc *tunnelConn) {
	m.mu.Lock()
	delete(m.live, tc)
	m.mu.Unlock()
}

// borrow takes an idle stream from the pool, skipping any that died while
// queued. It fails fast when no stream is attached at all (tunnel down);
// otherwise it blocks until a stream becomes idle or ctx/manager-close fires.
func (m *TunnelManager) borrow(ctx context.Context) (*tunnelConn, error) {
	for {
		m.mu.Lock()
		if m.closed || len(m.live) == 0 {
			m.mu.Unlock()
			return nil, errTunnelNotAttached
		}
		m.mu.Unlock()
		select {
		case tc := <-m.pool:
			m.mu.Lock()
			_, alive := m.live[tc]
			m.mu.Unlock()
			if !alive {
				continue // died while queued; discard and retry
			}
			return tc, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.done:
			return nil, errTunnelClosed
		}
	}
}

// release returns a healthy stream to the pool for reuse. Dead or
// manager-closed streams are discarded (their handlers will return on their
// own).
func (m *TunnelManager) release(tc *tunnelConn) {
	m.mu.Lock()
	_, alive := m.live[tc]
	closed := m.closed
	m.mu.Unlock()
	if !alive || closed {
		return
	}
	select {
	case m.pool <- tc:
	default:
		// Pool full (size mismatch); drop — revoke runs when the handler exits.
	}
}

// teardown forces a borrowed stream's Tunnel handler to return, closing the
// stream. Used when a request aborts with an error so the server's relay
// goroutine gets an EOF (and reopens that slot) instead of blocking forever on
// a peer that stopped reading. No-op if the stream already ended.
func (m *TunnelManager) teardown(tc *tunnelConn) {
	tc.cancel()
}

// Detach shuts the pool down: current and future borrowers fail fast and
// attached stream handlers return. Called when the worker is stopping.
func (m *TunnelManager) Detach() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	close(m.done)
	m.log.Info("tunnel pool detached")
}

var (
	errTunnelNotAttached = errors.New("tunnel not available (management server not connected)")
	errTunnelClosed      = errors.New("tunnel closed")
)

// RoundTrip forwards one HTTP request over an idle tunnel stream and waits for
// the paired response (or timeout). Returns the response frame, or an error if
// no tunnel is attached, the send fails, or the response does not arrive in
// time. Used by the IdP proxy (small bodies, single response frame).
func (m *TunnelManager) RoundTrip(method, path string, headers http.Header, body []byte, timeout time.Duration) (*pb.TunnelFrame, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	tc, err := m.borrow(ctx)
	if err != nil {
		return nil, err
	}
	stream := tc.stream
	id := randomFrameID()
	if err := stream.Send(&pb.TunnelFrame{
		Id: id, Method: method, Path: path, Headers: headerToPB(headers), Body: body,
		ChunkEof: true, ReqChunked: true,
	}); err != nil {
		m.teardown(tc)
		return nil, fmt.Errorf("tunnel send: %w", err)
	}
	resp, err := recvFrame(ctx, stream)
	if err != nil {
		m.teardown(tc)
		return nil, fmt.Errorf("tunnel recv: %w", err)
	}
	// Error frame is a complete response; the stream is reusable.
	m.release(tc)
	if resp.Error != "" {
		return resp, fmt.Errorf("%s", resp.Error)
	}
	return resp, nil
}

// requestChunkSize 是隧道请求方向单帧 body 上限：push（blob 分块/镜像层）
// 的请求体较大，worker 把它切成 requestChunkSize 的帧流式发送，末帧
// chunk_eof=true；GET/HEAD 等无体请求仍是单帧（chunk_eof=true）。
const requestChunkSize = 2 << 20

// RoundTripStream forwards one HTTP request over an idle tunnel stream and
// returns the response status/headers plus a streaming body reader. The request
// body (io.Reader) is streamed as chunked request frames; the response is read
// back as chunked frames directly off the (exclusively borrowed) stream. Close
// on the returned reader returns the stream to the pool on a clean EOF, or
// tears it down on an abort/error so the server doesn't stall.
func (m *TunnelManager) RoundTripStream(ctx context.Context, method, path string, headers http.Header, body io.Reader) (int, http.Header, io.ReadCloser, error) {
	tc, err := m.borrow(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	stream := tc.stream
	id := randomFrameID()
	if err := m.sendFrames(stream, id, method, path, headers, body); err != nil {
		m.teardown(tc)
		return 0, nil, nil, fmt.Errorf("tunnel send: %w", err)
	}
	respFirst, err := recvFrame(ctx, stream)
	if err != nil {
		m.teardown(tc)
		return 0, nil, nil, fmt.Errorf("tunnel recv: %w", err)
	}
	status := int(respFirst.Status)
	respHeader := PBHeaderToHTTP(respFirst.Headers)
	if respFirst.Error != "" {
		// Error frame is a complete response; reuse the stream.
		m.release(tc)
		return status, respHeader, nil, fmt.Errorf("%s", respFirst.Error)
	}
	r := &tunnelStreamReader{
		mgr:    m,
		conn:   tc,
		stream: stream,
		ctx:    ctx,
		cl:     contentLengthOf(respHeader),
		buf:    respFirst.Body,
		eof:    respFirst.ChunkEof,
	}
	return status, respHeader, r, nil
}

// sendFrames streams one request's body as chunked frames on the exclusively
// borrowed stream. One stream serves one request at a time, so there's nothing
// to interleave with — no mutex needed (the old shared-stream design needed
// sendMu precisely because concurrent requests shared one stream).
func (m *TunnelManager) sendFrames(stream pb.ManagementService_TunnelServer, id, method, path string, headers http.Header, body io.Reader) error {
	if body == nil {
		body = bytes.NewReader(nil)
	}
	buf := make([]byte, requestChunkSize)
	seq := int32(0)
	first := true
	lastEof := false // whether the last emitted frame carried chunk_eof
	for {
		n, rErr := body.Read(buf)
		if n > 0 {
			lastEof = rErr == io.EOF // data and EOF can arrive in the same Read
			frame := &pb.TunnelFrame{
				Id:         id,
				Body:       buf[:n],
				ChunkSeq:   seq,
				ChunkEof:   lastEof,
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
				return err
			}
		}
		if rErr == io.EOF {
			if first {
				// Empty body: still emit one frame carrying method/path/headers.
				return stream.Send(&pb.TunnelFrame{
					Id: id, Method: method, Path: path, Headers: headerToPB(headers),
					ChunkEof: true, ReqChunked: true,
				})
			}
			if !lastEof {
				// Data and EOF arrived in separate Reads: append an empty
				// terminating frame so the server's pump sees chunk_eof.
				return stream.Send(&pb.TunnelFrame{Id: id, ChunkSeq: seq, ChunkEof: true, ReqChunked: true})
			}
			return nil
		}
		if rErr != nil {
			return fmt.Errorf("request body read: %w", rErr)
		}
		if n == 0 {
			continue // (0, nil) — transient; loop again.
		}
	}
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

// tunnelStreamReader is an io.ReadCloser over the response frames arriving on
// the borrowed stream. Read serves pending bytes of the current frame (or Recvs
// the next one) and reports io.EOF when the server sent chunk_eof or
// Content-Length is satisfied. Close releases the stream back to the pool on a
// clean EOF, or tears it down if the read was aborted (caller cancel / stream
// error) so the server-side relay goroutine doesn't block waiting on a peer
// that stopped draining.
type tunnelStreamReader struct {
	mgr      *TunnelManager
	conn     *tunnelConn
	stream   pb.ManagementService_TunnelServer
	ctx      context.Context
	cl       int64 // upstream Content-Length, -1 unknown
	received int64
	eof      bool
	buf      []byte
	aborted  bool
	closeOnce sync.Once
}

func (r *tunnelStreamReader) Read(p []byte) (int, error) {
	for {
		if r.eof && len(r.buf) == 0 {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			r.aborted = true
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
		// Fetch the next frame directly off the stream (ctx-cancellable).
		f, err := recvFrame(r.ctx, r.stream)
		if err != nil {
			r.aborted = true
			return 0, err
		}
		if f.Error != "" {
			r.aborted = true
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
	}
}

func (r *tunnelStreamReader) Close() error {
	r.closeOnce.Do(func() {
		if r.aborted {
			r.mgr.teardown(r.conn)
		} else {
			r.mgr.release(r.conn)
		}
	})
	return nil
}

// recvFrame reads one frame from the stream, aborting early (returning the ctx
// error) if ctx fires before Recv returns. When ctx fires the stream is left
// with a pending Recv the caller must resolve by tearing the stream down (the
// resulting close makes the leaked Recv goroutine return).
func recvFrame(ctx context.Context, stream pb.ManagementService_TunnelServer) (*pb.TunnelFrame, error) {
	type recvResult struct {
		f   *pb.TunnelFrame
		err error
	}
	ch := make(chan recvResult, 1)
	go func() {
		f, err := stream.Recv()
		ch <- recvResult{f, err}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
