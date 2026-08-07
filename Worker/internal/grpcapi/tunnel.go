package grpcapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
)

// TunnelManager owns the reverse-tunnel bidi stream opened by the management
// server. The Worker's local /idp-proxy/ HTTP handler forwards in-cluster IdP
// requests through TunnelManager.RoundTrip; they ride the server-initiated
// Tunnel stream to the server (which proxies them to its local IdP) and the
// response comes back on the same stream, paired by frame id.
//
// Lifecycle: the management server opens exactly one Tunnel stream per Worker
// and keeps it alive (reopening on disconnect, like SubscribeEvents). The
// Worker side learns of the stream when its Tunnel handler runs; it registers
// the stream here so the HTTP handler can use it. When the stream ends the
// handler unregisters it and round-trips in flight fail fast.
type TunnelManager struct {
	log *slog.Logger

	mu     sync.RWMutex
	stream pb.ManagementService_TunnelServer // current stream; nil when none attached
	// pending requests: frame id -> chan that receives the response frame.
	pending map[string]chan *pb.TunnelFrame
}

// NewTunnelManager constructs a TunnelManager (no stream attached yet).
func NewTunnelManager(log *slog.Logger) *TunnelManager {
	if log == nil {
		log = slog.Default()
	}
	return &TunnelManager{log: log, pending: map[string]chan *pb.TunnelFrame{}}
}

// Tunnel implements the gRPC bidi handler. It runs for the lifetime of one
// server-initiated stream: it registers the stream, loops receiving response
// frames (dispatching each to its pending waiter), and unregisters on exit.
// Send-side (forwarding local HTTP requests) happens via RoundTrip, which
// writes to the registered stream under its own concurrency.
func (m *TunnelManager) Tunnel(stream pb.ManagementService_TunnelServer) error {
	m.attach(stream)
	defer m.detach()

	// Loop receiving response frames. Each carries the id of a pending request;
	// deliver it to that request's waiter and delete the pending entry.
	for {
		frame, err := stream.Recv()
		if err != nil {
			m.log.Debug("tunnel stream recv ended", "err", err)
			return err
		}
		ch, ok := m.takePending(frame.Id)
		if !ok {
			// Late/duplicate response for an already-timed-out request; drop.
			m.log.Debug("tunnel frame with no pending waiter", "id", frame.Id)
			continue
		}
		ch <- frame
	}
}

// Available reports whether a tunnel stream is currently attached and usable.
func (m *TunnelManager) Available() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stream != nil
}

// RoundTrip forwards one HTTP request over the tunnel and waits for the paired
// response (or timeout). Returns the response frame, or an error if no tunnel
// is attached, the send fails, or the response does not arrive in time.
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

// attach registers a freshly opened server-side stream.
func (m *TunnelManager) attach(stream pb.ManagementService_TunnelServer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stream = stream
	m.log.Info("idp tunnel attached")
}

// detach clears the stream and fails all pending waiters (their channels are
// closed so they return immediately). A subsequent server reconnect re-attaches.
func (m *TunnelManager) detach() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stream = nil
	for id, ch := range m.pending {
		close(ch)
		delete(m.pending, id)
	}
	m.log.Info("idp tunnel detached")
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
