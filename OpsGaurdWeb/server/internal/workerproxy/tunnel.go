package workerproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"
	"google.golang.org/grpc/metadata"
)

// tunnelChunkSize 是每条响应帧的最大 body 字节数。响应体 ≤ chunkSize 时按
// 单帧回(旧 worker 不识别分片字段也兼容);更大 body 拆多帧,末帧 chunk_eof=true。
// 2MiB 远低于 gRPC 消息上限,旧端可按原样收单帧。
const tunnelChunkSize = 2 << 20

// defaultTunnelConcurrency is the number of bidi Tunnel streams the management
// server opens per worker when RelayConfig.TunnelConcurrency is unset. Each
// stream serves one request at a time, so this many requests can be in flight
// concurrently, each on its own HTTP/2 flow-control window. Must match the
// worker's defaultTunnelPoolSize (both driven by OPSGUARD_TUNNEL_POOL).
const defaultTunnelConcurrency = 16

// RelayConfig 隧道中继策略:路径白名单 + 内嵌 registry 的 basic 凭据 + 隧道并发。
type RelayConfig struct {
	// AllowExtraPaths 额外的允许前缀(默认白名单:IdP 端点 + /v2 镜像仓库)。
	AllowExtraPaths []string
	// RegistryUser / RegistryPass 内嵌 OCI 仓库带 basic auth 时代持的账号;
	// 集群侧 dockerd 始终匿名访问本地中继点,由本侧代持注入。
	RegistryUser string
	RegistryPass string
	// TunnelConcurrency 是向 worker 打开的并发 Tunnel bidi 流数量(隧道池大小)。
	// 每条流同一时刻只服务一个请求,故该值决定可并发中继的请求数,每条流独占
	// 一个 HTTP/2 流控窗口。0(未设置)走 defaultTunnelConcurrency。必须与
	// worker 侧 OPSGUARD_TUNNEL_POOL 一致。
	TunnelConcurrency int
}

// allowedPath 判定隧道转发白名单。非白名单路径一律 403——防止集群内主体
// 把隧道当任意 HTTP 代理打到管理端本机(含管理 API)。
func allowedPath(p string, relay RelayConfig) bool {
	if p == "/.well-known/openid-configuration" || strings.HasPrefix(p, "/api/v1/idp/") {
		return true
	}
	if p == "/v2" || strings.HasPrefix(p, "/v2/") {
		return true
	}
	for _, prefix := range relay.AllowExtraPaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// ServeTunnel opens a POOL of reverse tunnel bidi streams to the Worker and
// keeps them open for the cluster's lifetime. Each stream (runOneTunnelStream)
// is independent: it serves one forwarded HTTP request at a time, proxies it to
// this management server's local IdP / embedded OCI registry at localBase, and
// streams the response back over the same stream. A stream that ends (transport
// error, worker restart, poisoned-by-abort) is reopened independently of the
// others, so one bad transfer doesn't take the whole tunnel down.
//
// Opening N streams (rather than the old single shared stream) gives each
// in-flight request its own HTTP/2 flow-control window and recv loop — that,
// plus the raised flow-control windows on the gRPC client/server, is what
// removes the throughput ceiling. Blocks until ctx is canceled.
func (c *Client) ServeTunnel(ctx context.Context, localBase string, relay RelayConfig, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if c.stub == nil {
		return &ErrUnreachable{URL: c.target, Err: fmt.Errorf("grpc client not initialized")}
	}
	n := relay.TunnelConcurrency
	if n <= 0 {
		n = defaultTunnelConcurrency
	}

	// IdP responses are small JSON — 15s covers them. Registry pulls/pushes
	// carry multi-hundred-MB blobs and must not be cut by a fixed deadline;
	// their lifetime is bound to the stream context instead.
	hcIdp := &http.Client{Timeout: 15 * time.Second}
	hcRegistry := &http.Client{}

	log.Info("tunnel pool starting", "target", c.target, "streams", n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runOneTunnelStream(ctx, hcIdp, hcRegistry, localBase, relay, log)
		}()
	}
	wg.Wait()
	log.Info("tunnel pool stopped", "target", c.target)
	return ctx.Err()
}

// runOneTunnelStream opens one bidi stream and serves requests on it until it
// ends, then reopens with exponential backoff. A stream that cannot be opened
// (worker unreachable) keeps retrying rather than killing the pool.
//
// Each stream gets its own cancellable context. When serveOneStream returns the
// context is canceled — this FULLY terminates the gRPC stream (CloseSend alone
// only half-closes the client→server direction, which the worker can't observe
// because it never reads a pooled stream until it borrows it for a relay). With
// the cancel, the worker's Tunnel handler sees ctx.Done() and revokes the dead
// stream from its pool; without it the stale stream stays borrowable and relay
// requests sent on it hang until the caller times out (the "context canceled"
// relay errors that accumulate over long uptime).
func (c *Client) runOneTunnelStream(ctx context.Context, hcIdp, hcRegistry *http.Client, localBase string, relay RelayConfig, log *slog.Logger) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		streamCtx, streamCancel := context.WithCancel(ctx)
		// gRPC bidi streams carry per-RPC metadata from the context; the auth
		// token must be attached here (unary RPCs do it in callCtx, but streams
		// open the context directly). Without it the Worker rejects with
		// Unauthenticated.
		if c.token != "" {
			streamCtx = metadata.AppendToOutgoingContext(streamCtx, "authorization", "Bearer "+c.token)
		}
		stream, err := c.stub.Tunnel(streamCtx)
		if err != nil {
			streamCancel()
			log.Warn("tunnel stream open failed, retrying", "target", c.target, "err", err)
		} else {
			err = c.serveOneStream(ctx, stream, hcIdp, hcRegistry, localBase, relay, log)
			_ = stream.CloseSend()
			streamCancel() // fully terminate so the worker revokes this stream from its pool
			if err != nil && ctx.Err() == nil {
				log.Debug("tunnel stream ended, reopening", "target", c.target, "err", err)
			}
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// serveOneStream reads forwarded requests off one bidi stream and proxies each
// to the local IdP/registry, streaming the response back. One stream serves one
// request at a time (the worker borrows it exclusively per request), so there's
// no per-id demux and no concurrent Send on this stream — only this loop sends
// (via proxyOneStream/relayResponse); a request-body pump goroutine only Recvs.
// Returns (ending the stream) on a transport error or when a multi-frame
// request body wasn't fully consumed (a poisoned stream); runOneTunnelStream
// reopens a fresh one.
func (c *Client) serveOneStream(ctx context.Context, stream pb.ManagementService_TunnelClient, hcIdp, hcRegistry *http.Client, localBase string, relay RelayConfig, log *slog.Logger) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("tunnel stream closed by worker")
			}
			return fmt.Errorf("tunnel recv: %w", err)
		}
		// Build the request body reader. A multi-frame request body is streamed
		// through an io.Pipe (pumpRequestBody recvs remaining frames and writes
		// them) so hundred-MB push layers never sit fully in memory.
		var (
			body io.Reader = bytes.NewReader(frame.Body)
			pump *bodyPump
		)
		if frame.GetReqChunked() && !frame.ChunkEof {
			// Multi-frame chunked request: first frame carries method/path/
			// headers + the first body chunk; the pump feeds the rest.
			pr, pw := io.Pipe()
			body = pr
			pump = &bodyPump{done: make(chan struct{})}
			go pumpRequestBody(stream, frame, pw, pump)
		}
		// proxyOneStream sends the response (success chunked or an error frame).
		proxyOneStream(stream, hcIdp, hcRegistry, localBase, relay, frame, body)
		if pump != nil {
			<-pump.done
			if !pump.consumed {
				// The reader gave up before EOF (worker aborted, or the local
				// registry rejected before consuming the body). Leftover body
				// frames may still be queued on this stream, so we can't safely
				// recv the next request on it — recycle the whole stream.
				return fmt.Errorf("request body not fully consumed; recycling stream")
			}
		}
	}
}

// bodyPump carries the request-body pump goroutine's completion state back to
// serveOneStream. consumed is set before done is closed, so reading it after
// <-done is race-free (channel close establishes happens-before).
type bodyPump struct {
	done     chan struct{}
	consumed bool
}

// pumpRequestBody drains the remaining chunked request-body frames (the first
// frame's body is already in first) into pw as the HTTP request body fed to the
// local registry. It exits when EOF is reached (consumed=true), when the reader
// stops draining (consumed=false — the request body wasn't fully consumed), or
// on a stream error (consumed=false).
func pumpRequestBody(stream pb.ManagementService_TunnelClient, first *pb.TunnelFrame, pw *io.PipeWriter, bp *bodyPump) {
	defer close(bp.done)
	defer pw.Close()
	if len(first.Body) > 0 {
		if _, err := pw.Write(first.Body); err != nil {
			return // reader gave up; body not fully consumed
		}
	}
	seq := first.ChunkSeq
	for {
		nf, err := stream.Recv()
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if nf.ChunkSeq != seq+1 {
			pw.CloseWithError(fmt.Errorf("tunnel body: out-of-order chunk_seq %d (want %d)", nf.ChunkSeq, seq+1))
			return
		}
		if len(nf.Body) > 0 {
			if _, err := pw.Write(nf.Body); err != nil {
				return // reader stopped; body not fully consumed
			}
		}
		seq = nf.ChunkSeq
		if nf.ChunkEof {
			bp.consumed = true
			return
		}
	}
}

// proxyOneStream executes one tunneled HTTP request against the local
// IdP/registry and streams the response back as one or more frames (single
// frame when the body fits in tunnelChunkSize, chunked otherwise; every
// completed response carries chunk_eof=true so the worker can trust the flag).
// rd is the request body reader (a bytes.Reader for single-frame requests, or
// an io.PipeReader streaming a chunked push body). On any proxying failure it
// returns a single error frame so the worker can surface it to the caller
// (dockerd fails and retries).
func proxyOneStream(stream pb.ManagementService_TunnelClient, hcIdp, hcRegistry *http.Client, localBase string, relay RelayConfig, f *pb.TunnelFrame, rd io.Reader) {
	if !allowedPath(f.Path, relay) && f.Method != "" {
		_ = stream.Send(&pb.TunnelFrame{
			Id: f.Id, Status: http.StatusForbidden, ChunkEof: true,
			Error: "path not allowed by tunnel relay policy",
		})
		return
	}
	isRegistry := f.Path == "/v2" || strings.HasPrefix(f.Path, "/v2/")
	hc := hcIdp
	if isRegistry {
		hc = hcRegistry
	}

	req, err := http.NewRequestWithContext(stream.Context(), f.Method, strings.TrimRight(localBase, "/")+f.Path, rd)
	if err != nil {
		_ = stream.Send(&pb.TunnelFrame{Id: f.Id, Status: http.StatusBadGateway, ChunkEof: true, Error: "build request: " + err.Error()})
		return
	}
	for _, h := range f.Headers {
		req.Header.Add(h.Key, h.Value)
	}
	// Registry pull-through: authenticate server-side with the relay account so
	// in-cluster dockerd stays anonymous against the local relay endpoint.
	if isRegistry && relay.RegistryUser != "" {
		req.SetBasicAuth(relay.RegistryUser, relay.RegistryPass)
	}

	resp, err := hc.Do(req)
	if err != nil {
		_ = stream.Send(&pb.TunnelFrame{Id: f.Id, Status: http.StatusBadGateway, ChunkEof: true, Error: "proxy: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	relayResponse(stream, resp, f.Id)
}

// relayResponse streams resp back over the tunnel as one or more frames.
// Registry push responses carry an upload-session Location header; if the
// embedded registry ever returns an absolute URL (pointing at the management
// server's own host), it is rewritten to a path-only value so the caller
// (dockerd) resolves it against the relay endpoint it is talking to — the
// Location must stay inside the tunnel.
func relayResponse(stream pb.ManagementService_TunnelClient, resp *http.Response, id string) {
	headers := headerToPB(resp.Header)
	for _, h := range headers {
		if strings.EqualFold(h.Key, "Location") {
			h.Value = relativizeLocation(h.Value)
		}
	}
	buf := make([]byte, tunnelChunkSize)
	var (
		seq     int32
		first   = true
		sent    bool
		lastEof bool // 最后一条数据帧是否已带 chunk_eof
	)
	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			lastEof = rErr == io.EOF // 数据与 EOF 同帧到达是可能的,能省则省
			frame := &pb.TunnelFrame{
				Id:       id,
				Body:     buf[:n],
				ChunkSeq: seq,
				ChunkEof: lastEof,
			}
			if first {
				frame.Status = int32(resp.StatusCode)
				frame.Headers = headers
			}
			seq++
			first = false
			sent = true
			if err := stream.Send(frame); err != nil {
				// Stream gone (worker disconnected / server shutting down);
				// abort the upstream request via the stream context.
				return
			}
		}
		if rErr == io.EOF {
			if !sent {
				// Empty body (or HEAD): one frame carrying status+headers.
				_ = stream.Send(&pb.TunnelFrame{
					Id: id, Status: int32(resp.StatusCode), Headers: headers, ChunkEof: true,
				})
			} else if !lastEof {
				// 最后一段数据与 EOF 分帧到达:补一条空终止帧,worker 无需依赖
				// Content-Length 判断结束(旧 worker 也能识别 chunk_eof)。
				_ = stream.Send(&pb.TunnelFrame{Id: id, ChunkSeq: seq, ChunkEof: true})
			}
			return
		}
		if rErr != nil {
			_ = stream.Send(&pb.TunnelFrame{Id: id, ChunkSeq: seq, ChunkEof: true, Error: "proxy read: " + rErr.Error()})
			return
		}
	}
}

func headerToPB(h http.Header) []*pb.TunnelHeader {
	out := make([]*pb.TunnelHeader, 0, len(h))
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, &pb.TunnelHeader{Key: k, Value: v})
		}
	}
	return out
}

// relativizeLocation converts an absolute Location URL into a path-only value
// (scheme/host dropped, path+query kept). Relative locations pass through.
func relativizeLocation(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return v
	}
	return u.RequestURI()
}
