package cluster

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
)

// clusterWorkerServer 是 cluster 测试用的 mock Worker（只实现 Self RPC；
// failSwarm 时返回非 manager 角色，模拟 Worker 可达但不是 swarm 控制面）。
// 其余 RPC 走 UnimplementedManagementServiceServer。
type clusterWorkerServer struct {
	pb.UnimplementedManagementServiceServer
	failSwarm bool
}

func (m *clusterWorkerServer) Self(context.Context, *pb.Empty) (*pb.SelfInfo, error) {
	role, swarm := "manager", true
	if m.failSwarm {
		role, swarm = "worker", false
	}
	return &pb.SelfInfo{
		NodeId: "abc123", Hostname: "node-1", Role: role,
		Leader: false, State: "ready", SwarmManager: swarm, Addr: "10.0.0.1",
	}, nil
}

// startGRPCWorker 在随机 TCP 端口上启动一个真实 gRPC 服务器，返回其
// http://127.0.0.1:<port> URL（cluster.Service.probeWorker 经 workerproxy.New
// 拨号，grpcTarget 会剥掉 scheme）。cleanup 注册到 t.Cleanup。
func startGRPCWorker(t *testing.T, srv pb.ManagementServiceServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterManagementServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.GracefulStop() })
	return "http://" + lis.Addr().String()
}

// mockWorker 启动一个模拟 Worker gRPC 服务器，返回其 URL（failSwarm 时 Self
// 返回非 manager，模拟可达但非控制面的 Worker）。
func mockWorker(t *testing.T, failSwarm bool) string {
	t.Helper()
	return startGRPCWorker(t, &clusterWorkerServer{failSwarm: failSwarm})
}

func newTestService(t *testing.T, failSwarm bool) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), mockWorker(t, failSwarm)
}

// TestAddOnline 接入成功：探测通过 → 落库 online。
func TestAddOnline(t *testing.T) {
	svc, url := newTestService(t, false)
	added, err := svc.Add(context.Background(), &store.Cluster{
		Name: "dev", WorkerURL: url, Desc: "开发集群",
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.Status != store.ClusterOnline {
		t.Fatalf("expected online, got %s", added.Status)
	}
	if added.LastSeen.IsZero() {
		t.Fatal("expected last_seen set")
	}
}

// TestAddNonManager 接入失败：Worker 可达但不是 swarm manager。
func TestAddNonManager(t *testing.T) {
	svc, url := newTestService(t, true)
	_, err := svc.Add(context.Background(), &store.Cluster{Name: "dev", WorkerURL: url})
	if err == nil {
		t.Fatal("expected probe error for non-manager worker")
	}
	if _, ok := err.(ErrProbeFailed); !ok {
		t.Fatalf("expected ErrProbeFailed, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "not a swarm manager") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// TestAddDuplicate 重复接入被拒。
func TestAddDuplicate(t *testing.T) {
	svc, url := newTestService(t, false)
	in := &store.Cluster{Name: "dev", WorkerURL: url}
	if _, err := svc.Add(context.Background(), in); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := svc.Add(context.Background(), in); err == nil {
		t.Fatal("expected duplicate error")
	}
}

// TestListProbeAndOffline 列表逐个探测：可达 → online；停掉的 Worker → offline。
func TestListProbeAndOffline(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	svc := New(st)

	// 先接一个在线的
	onlineURL := mockWorker(t, false)
	if _, err := svc.Add(context.Background(), &store.Cluster{Name: "dev", WorkerURL: onlineURL}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// 直接落库一个指向已关闭端口的集群（模拟 Worker 下线）：开一个 listener
	// 立即关闭，得到一个保证空闲的地址，拨号必失败 → offline。
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	offlineURL := "http://" + lis.Addr().String()
	_ = lis.Close()
	if err := st.PutCluster(&store.Cluster{Name: "down", WorkerURL: offlineURL}); err != nil {
		t.Fatalf("put: %v", err)
	}

	items, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(items))
	}
	byName := map[string]*store.Cluster{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if byName["dev"].Status != store.ClusterOnline {
		t.Fatalf("dev should be online: %+v", byName["dev"])
	}
	if byName["down"].Status != store.ClusterOffline {
		t.Fatalf("down should be offline: %+v", byName["down"])
	}
	if byName["down"].Err == "" {
		t.Fatal("down cluster should carry probe error text")
	}
}

// TestRemove 移除存在/不存在。
func TestRemove(t *testing.T) {
	svc, url := newTestService(t, false)
	if _, err := svc.Add(context.Background(), &store.Cluster{Name: "dev", WorkerURL: url}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := svc.Remove(context.Background(), "dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := svc.Remove(context.Background(), "dev"); err == nil {
		t.Fatal("expected ErrNotFound on double remove")
	}
}

// TestProbeTimeout 不可达 Worker 在超时内返回 offline 而不是挂死。
func TestProbeTimeout(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	svc := New(st)
	svc.probeTimeout = 500 * time.Millisecond

	// 黑洞地址（丢弃连接），应快速失败
	if err := st.PutCluster(&store.Cluster{Name: "blackhole", WorkerURL: "http://10.255.255.1:9"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	start := time.Now()
	items, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Status != store.ClusterOffline {
		t.Fatalf("expected offline, got %+v", items)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("probe took too long: %v", elapsed)
	}
}
