package workerproxy

import (
	"context"
	"net"
	"testing"

	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// --- gRPC mock server helpers (bufconn, in-memory) ---

const testBufconnBufSize = 1024 * 1024

// startBufconnServer registers srv as a ManagementServiceServer on an
// in-memory gRPC server and returns a *Client wired to it via bufconn. The
// returned Client's token is set from the token argument (passed as bearer
// metadata just like New would). Cleanup is registered with t.Cleanup.
func startBufconnServer(t *testing.T, srv pb.ManagementServiceServer, token string) *Client {
	t.Helper()
	lis := bufconn.Listen(testBufconnBufSize)
	s := grpc.NewServer()
	pb.RegisterManagementServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.GracefulStop() })

	dialer := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{target: "bufnet", token: token, conn: conn, stub: pb.NewManagementServiceClient(conn)}
}

// errSelfServer implements only Self (returns a gRPC Unauthenticated error to
// simulate a missing/invalid bearer token).
type errSelfServer struct {
	pb.UnimplementedManagementServiceServer
	requireToken bool
}

func (m *errSelfServer) Self(ctx context.Context, _ *pb.Empty) (*pb.SelfInfo, error) {
	if m.requireToken {
		// Mimic the server rejecting the call for missing auth.
		return nil, status.Error(codes.Unauthenticated, "missing or invalid authorization")
	}
	return &pb.SelfInfo{}, nil
}

// --- tests ---

// TestSelfDecode 解码 Worker Self RPC 响应（原 /api/v1/self）。
func TestSelfDecode(t *testing.T) {
	srv := &selfServer{}
	cli := startBufconnServer(t, srv, "tok123")

	si, err := cli.Self(context.Background())
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if !si.SwarmManager || si.Role != "manager" || si.NodeID != "n1" {
		t.Fatalf("unexpected self: %+v", si)
	}
}

// TestAuthRequired 无 token → gRPC Unauthenticated → ErrUnreachable。
// 原测试断言 Status==401；gRPC 没有真实 HTTP 状态，因此这里改成断言返回的是
// *ErrUnreachable 且底层错误是 Unauthenticated（等价于旧的 401 拒绝语义）。
func TestAuthRequired(t *testing.T) {
	cli := startBufconnServer(t, &errSelfServer{requireToken: true}, "")

	_, err := cli.Self(context.Background())
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	ue, ok := err.(*ErrUnreachable)
	if !ok {
		t.Fatalf("expected *ErrUnreachable, got %T", err)
	}
	// 原测试断言 status 401；gRPC 下对应 Unauthenticated。保留对 ErrUnreachable
	// 类型的断言，并把 HTTP 状态等价检查改为 codes.Unauthenticated。
	if status.Code(ue.Err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", ue.Err)
	}
}

// TestNodeStats 解码 NodeStats RPC（原 /api/v1/local/stats）。
func TestNodeStats(t *testing.T) {
	srv := &nodeStatsServer{}
	cli := startBufconnServer(t, srv, "")

	ns, err := cli.NodeStats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if ns.Node != "node-1" || len(ns.Containers) != 1 || ns.Containers[0].Service != "web" {
		t.Fatalf("unexpected stats: %+v", ns)
	}
}

// TestUnreachable 连接失败 → ErrUnreachable（status=0）。
// 用一个立即关闭的 listener 模拟不可达的 Worker。
func TestUnreachable(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close() // 立即关闭，拨号必失败

	cli := New(addr, "")
	_, err = cli.Self(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable worker")
	}
	if _, ok := err.(*ErrUnreachable); !ok {
		t.Fatalf("expected *ErrUnreachable, got %T", err)
	}
}

// --- mock servers used by client_test.go ---

type selfServer struct {
	pb.UnimplementedManagementServiceServer
}

func (m *selfServer) Self(context.Context, *pb.Empty) (*pb.SelfInfo, error) {
	return &pb.SelfInfo{
		NodeId: "n1", Hostname: "h1", Role: "manager",
		Leader: true, State: "ready", SwarmManager: true, Addr: "10.0.0.1",
	}, nil
}

type nodeStatsServer struct {
	pb.UnimplementedManagementServiceServer
}

func (m *nodeStatsServer) NodeStats(context.Context, *pb.Empty) (*pb.NodeStatsResponse, error) {
	return &pb.NodeStatsResponse{
		Node: "node-1",
		Containers: []*pb.ContainerStat{{
			ContainerId: "c1", Service: "web",
			CpuPercent: 3.5, MemPercent: 20.1,
			MemUsage: 1048576, MemLimit: 5242880,
		}},
	}, nil
}
