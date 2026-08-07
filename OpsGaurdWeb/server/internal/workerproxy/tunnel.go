package workerproxy

import (
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

// ServeTunnel opens the reverse IdP tunnel bidi stream to the Worker and runs
// it for the lifetime of one stream: it loops receiving request frames the
// Worker forwards from in-cluster services (e.g. r-nacos), proxies each to the
// management server's local IdP at localBase, and sends the response frame back
// over the same stream.
//
// localBase is the loopback base of this management server (e.g.
// "http://127.0.0.1:8080"); the Worker's request path is appended to it.
//
// The stream is server-initiated (honoring the one-way network policy) and kept
// open; the idptunnel manager reconnects on error. This call blocks until the
// stream ends (ctx cancel, transport error, or Worker disconnect).
func (c *Client) ServeTunnel(ctx context.Context, localBase string, log *slog.Logger) error {
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
	log.Info("idp tunnel stream opened", "target", c.target)

	hc := &http.Client{Timeout: 15 * time.Second}
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
		// Proxy one request in a per-frame goroutine so a slow IdP call does not
		// block reading the next request frame.
		go func(f *pb.TunnelFrame) {
			resp := proxyOne(hc, localBase, f)
			if err := stream.Send(resp); err != nil {
				log.Warn("tunnel send response failed", "id", f.Id, "err", err)
				cancel()
			}
		}(reqFrame)
	}
}

// proxyOne executes one tunneled HTTP request against the local IdP and builds
// the response frame. On any proxying failure it returns a frame carrying an
// error string (status 502) so the Worker can surface it to the caller.
func proxyOne(hc *http.Client, localBase string, f *pb.TunnelFrame) *pb.TunnelFrame {
	url := strings.TrimRight(localBase, "/") + f.Path
	req, err := http.NewRequest(f.Method, url, strings.NewReader(string(f.Body)))
	if err != nil {
		return &pb.TunnelFrame{Id: f.Id, Status: http.StatusBadGateway, Error: "build request: " + err.Error()}
	}
	for _, h := range f.Headers {
		req.Header.Add(h.Key, h.Value)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return &pb.TunnelFrame{Id: f.Id, Status: http.StatusBadGateway, Error: "proxy: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return &pb.TunnelFrame{
		Id:      f.Id,
		Status:  int32(resp.StatusCode),
		Headers: headerToPB(resp.Header),
		Body:    body,
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
