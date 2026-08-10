package workerproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"
	"google.golang.org/grpc/metadata"
)

// tunnelChunkSize 是每条响应帧的最大 body 字节数。响应体 ≤ chunkSize 时按
// 单帧回(旧 worker 不识别分片字段也兼容);更大 body 拆多帧,末帧 chunk_eof=true。
// 2MiB 远低于 gRPC 默认 4MiB 消息上限,旧端可按原样收单帧。
const tunnelChunkSize = 2 << 20

// RelayConfig 隧道中继策略:路径白名单 + 内嵌 registry 的 basic 凭据。
type RelayConfig struct {
	// AllowExtraPaths 额外的允许前缀(默认白名单:IdP 端点 + /v2 镜像仓库)。
	AllowExtraPaths []string
	// RegistryUser / RegistryPass 内嵌 OCI 仓库带 basic auth 时代持的账号;
	// 集群侧 dockerd 始终匿名访问本地中继点,由本侧代持注入。
	RegistryUser string
	RegistryPass string
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

// ServeTunnel opens the reverse tunnel bidi stream to the Worker and runs
// it for the lifetime of one stream: it loops receiving request frames the
// Worker forwards from in-cluster services (r-nacos IdP, docker registry
// pulls), proxies each to the management server's local IdP / embedded OCI
// registry at localBase, and sends the response frame(s) back over the same
// stream.
//
// localBase is the loopback base of this management server (e.g.
// "http://127.0.0.1:8080"); the Worker's request path is appended to it.
//
// The stream is server-initiated (honoring the one-way network policy) and kept
// open; the idptunnel manager reconnects on error. This call blocks until the
// stream ends (ctx cancel, transport error, or Worker disconnect).
func (c *Client) ServeTunnel(ctx context.Context, localBase string, relay RelayConfig, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if c.stub == nil {
		return &ErrUnreachable{URL: c.target, Err: fmt.Errorf("grpc client not initialized")}
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// gRPC bidi streams carry per-RPC metadata from the context; the auth token
	// must be attached here (unary RPCs do it in callCtx, but streams open the
	// context directly). Without it the Worker's stream interceptor rejects with
	// Unauthenticated.
	if c.token != "" {
		streamCtx = metadata.AppendToOutgoingContext(streamCtx, "authorization", "Bearer "+c.token)
	}
	stream, err := c.stub.Tunnel(streamCtx)
	if err != nil {
		return c.wrapErr(err)
	}
	log.Info("tunnel stream opened", "target", c.target)

	// IdP responses are small JSON — 15s covers them. Registry pulls carry
	// multi-hundred-MB blobs and must not be cut by a fixed deadline; their
	// lifetime is bound to the stream context instead.
	hcIdp := &http.Client{Timeout: 15 * time.Second}
	hcRegistry := &http.Client{}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reqFrame, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("tunnel stream closed by worker")
			}
			return fmt.Errorf("tunnel recv: %w", err)
		}
		// Proxy one request in a per-frame goroutine so a slow upstream does not
		// block reading the next request frame.
		go func(f *pb.TunnelFrame) {
			proxyOneStream(stream, hcIdp, hcRegistry, localBase, relay, f)
		}(reqFrame)
	}
}

// proxyOneStream executes one tunneled HTTP request against the local
// IdP/registry and streams the response back as one or more frames (single
// frame when the body fits in tunnelChunkSize, chunked otherwise; every
// completed response carries chunk_eof=true so new workers can trust the flag).
// On any proxying failure it returns a single error frame so the Worker can
// surface it to the caller (dockerd fails the pull and retries).
func proxyOneStream(stream pb.ManagementService_TunnelClient, hcIdp, hcRegistry *http.Client, localBase string, relay RelayConfig, f *pb.TunnelFrame) {
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

	req, err := http.NewRequestWithContext(stream.Context(), f.Method, strings.TrimRight(localBase, "/")+f.Path, bytes.NewReader(f.Body))
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

	headers := headerToPB(resp.Header)
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
				Id:       f.Id,
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
					Id: f.Id, Status: int32(resp.StatusCode), Headers: headers, ChunkEof: true,
				})
			} else if !lastEof {
				// 最后一段数据与 EOF 分帧到达:补一条空终止帧,worker 无需依赖
				// Content-Length 判断结束(旧 worker 也能识别 chunk_eof)。
				_ = stream.Send(&pb.TunnelFrame{Id: f.Id, ChunkSeq: seq, ChunkEof: true})
			}
			return
		}
		if rErr != nil {
			_ = stream.Send(&pb.TunnelFrame{Id: f.Id, ChunkSeq: seq, ChunkEof: true, Error: "proxy read: " + rErr.Error()})
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
