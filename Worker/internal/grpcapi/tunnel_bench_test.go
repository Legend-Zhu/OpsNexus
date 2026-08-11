package grpcapi

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Benchmarks for the reverse tunnel. They wire the real TunnelManager to a real
// gRPC stack over bufconn (so HTTP/2 framing + flow control actually run), then
// drive RoundTripStream while a helper goroutine plays the management-server
// role: recv the request, stream a blob back as chunked response frames.
//
// These exist to (a) confirm the pooled + flow-control-raised design delivers
// real throughput, and (b) guard against regressions. The server/client windows
// mirror the production settings in cmd/worker/main.go and
// workerproxy/client.go (32MiB/stream, 64MiB/conn).

const benchBufSize = 64 * 1024 * 1024

// benchMgmtServer is a minimal ManagementServiceServer whose Tunnel delegates
// to the TunnelManager under test; all other RPCs stay Unimplemented.
type benchMgmtServer struct {
	pb.UnimplementedManagementServiceServer
	mgr *TunnelManager
}

func (s *benchMgmtServer) Tunnel(stream pb.ManagementService_TunnelServer) error {
	return s.mgr.Tunnel(stream)
}

// benchEnv wires a TunnelManager (pool=nStreams) to a bufconn gRPC server and
// opens nStreams client-side Tunnel streams the benchmark uses to play the
// management-server role.
type benchEnv struct {
	mgr    *TunnelManager
	clis   []pb.ManagementService_TunnelClient
	cancel context.CancelFunc
}

func newBenchEnv(b testing.TB, nStreams int) *benchEnv {
	b.Helper()
	lis := bufconn.Listen(benchBufSize)
	mgr := NewTunnelManager(nil, nStreams)
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(16<<20),
		grpc.MaxSendMsgSize(16<<20),
		grpc.InitialWindowSize(32<<20),
		grpc.InitialConnWindowSize(64<<20),
	)
	pb.RegisterManagementServiceServer(srv, &benchMgmtServer{mgr: mgr})
	go func() { _ = srv.Serve(lis) }()
	b.Cleanup(srv.GracefulStop)

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(32<<20),
		grpc.WithInitialConnWindowSize(64<<20),
	)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	b.Cleanup(func() { _ = conn.Close() })

	stub := pb.NewManagementServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	env := &benchEnv{mgr: mgr, cancel: cancel}
	for i := 0; i < nStreams; i++ {
		cli, err := stub.Tunnel(ctx)
		if err != nil {
			b.Fatalf("open tunnel stream %d: %v", i, err)
		}
		env.clis = append(env.clis, cli)
	}
	// Wait until the worker side has registered every stream into the pool.
	for i := 0; i < 400 && !mgr.Available(); i++ {
		time.Sleep(2 * time.Millisecond)
	}
	if !mgr.Available() {
		b.Fatal("tunnel streams never attached")
	}
	b.Cleanup(func() {
		cancel()
		for _, c := range env.clis {
			_ = c.CloseSend()
		}
	})
	return env
}

// playServer recvs one request off cli (draining any chunked request body to
// eof) then streams blob back as chunked response frames, mirroring the real
// relayResponse framing. Returns when the full response has been sent.
func playServer(cli pb.ManagementService_TunnelClient, blob []byte, status int) error {
	req, err := cli.Recv()
	if err != nil {
		return err
	}
	for !req.ChunkEof {
		req, err = cli.Recv()
		if err != nil {
			return err
		}
	}
	cs := requestChunkSize
	seq := int32(0)
	first := true
	for off := 0; off < len(blob); off += cs {
		end := off + cs
		if end > len(blob) {
			end = len(blob)
		}
		f := &pb.TunnelFrame{
			Id:       req.Id,
			Body:     blob[off:end],
			ChunkSeq: seq,
			ChunkEof: end == len(blob),
		}
		if first {
			f.Status = int32(status)
			first = false
		}
		seq++
		if err := cli.Send(f); err != nil {
			return err
		}
	}
	if first {
		_ = cli.Send(&pb.TunnelFrame{Id: req.Id, Status: int32(status), ChunkEof: true})
	}
	return nil
}

// BenchmarkTunnelRTT measures single-frame round-trip latency (1-byte body).
func BenchmarkTunnelRTT(b *testing.B) {
	env := newBenchEnv(b, 1)
	cli := env.clis[0]
	payload := []byte("x")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done := make(chan struct{})
		go func() { defer close(done); _ = playServer(cli, payload, 200) }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, rc, err := env.mgr.RoundTripStream(ctx, "GET", "/v2/x", nil, nil)
		if err != nil {
			cancel()
			b.Fatalf("RoundTripStream: %v", err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			cancel()
			b.Fatalf("read: %v", err)
		}
		rc.Close()
		cancel()
		<-done
	}
}

// BenchmarkTunnelThroughput measures single-stream download throughput over the
// real gRPC transport (a 64MiB blob per iteration). Reported in bytes/op.
func BenchmarkTunnelThroughput(b *testing.B) {
	env := newBenchEnv(b, 1)
	cli := env.clis[0]
	const blobMB = 64
	blob := make([]byte, blobMB<<20)
	b.SetBytes(int64(len(blob)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		done := make(chan struct{})
		go func() { defer close(done); _ = playServer(cli, blob, 200) }()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, _, rc, err := env.mgr.RoundTripStream(ctx, "GET", "/v2/x/blobs/sha256:y", nil, nil)
		if err != nil {
			cancel()
			b.Fatalf("RoundTripStream: %v", err)
		}
		n, err := io.Copy(io.Discard, rc)
		rc.Close()
		cancel()
		<-done
		if err != nil {
			b.Fatalf("read: %v", err)
		}
		if i == 0 && n != int64(len(blob)) {
			b.Fatalf("transferred %d bytes, want %d", n, len(blob))
		}
	}
}

// BenchmarkTunnelConcurrent measures aggregate throughput with pool-sized
// concurrency: pool=4 streams, each pulling a 16MiB blob simultaneously. The
// per-op cost covers all 4 transfers; SetBytes reports the total payload.
func BenchmarkTunnelConcurrent(b *testing.B) {
	const pool = 4
	env := newBenchEnv(b, pool)
	const blobMB = 16
	blob := make([]byte, blobMB<<20)
	b.SetBytes(int64(len(blob)) * pool)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		errs := make(chan error, pool)
		for j := 0; j < pool; j++ {
			wg.Add(1)
			cli := env.clis[j]
			go func() {
				defer wg.Done()
				if err := playServer(cli, blob, 200); err != nil {
					errs <- err
				}
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_, _, rc, err := env.mgr.RoundTripStream(ctx, "GET", "/v2/x/blobs/sha256:c", nil, nil)
				if err != nil {
					errs <- err
					return
				}
				if _, err := io.Copy(io.Discard, rc); err != nil {
					errs <- err
				}
				rc.Close()
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				b.Fatalf("concurrent transfer: %v", err)
			}
		}
	}
}
