package cluster

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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

// TestAddNameValidation 集群名必须匹配 [a-zA-Z0-9][a-zA-Z0-9._-]{0,62}。
func TestAddNameValidation(t *testing.T) {
	svc, url := newTestService(t, false)
	for _, bad := range []string{"", "a/b", "a b", "#x", "-lead", ".lead", strings.Repeat("a", 64)} {
		if _, err := svc.Add(context.Background(), &store.Cluster{Name: bad, WorkerURL: url}); err == nil {
			t.Fatalf("name %q should be rejected", bad)
		}
	}
	for _, ok := range []string{"dev", "dev-cluster", "c1", "a.b_c-d", strings.Repeat("a", 63)} {
		if _, err := svc.Add(context.Background(), &store.Cluster{Name: ok, WorkerURL: url}); err != nil {
			t.Fatalf("name %q should be accepted: %v", ok, err)
		}
	}
}

// TestPublicCarriesErr Public() 保留探测错误（离线原因对前端可见），仅抹除 token。
func TestPublicCarriesErr(t *testing.T) {
	svc, url := newTestService(t, false)
	c, err := svc.Add(context.Background(), &store.Cluster{Name: "dev", WorkerURL: url, Token: "secret"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	c.Err = "probe: connection refused"
	pub := c.Public()
	if pub.Err != "probe: connection refused" {
		t.Fatalf("Public() should carry Err, got %q", pub.Err)
	}
	if pub.Token != "" || pub.HasToken != true {
		t.Fatalf("Public() should mask token: token=%q hasToken=%v", pub.Token, pub.HasToken)
	}
}

// TestServiceConfigSnapshot 服务配置快照的读写删。
func TestServiceConfigSnapshot(t *testing.T) {
	svc, _ := newTestService(t, false)
	cfg := "service:\n  name: web\n  image: nginx:alpine\n"

	if sc, err := svc.ServiceConfig("dev", "web"); err != nil || sc != nil {
		t.Fatalf("missing snapshot should return nil: %v", err)
	}
	if err := svc.SaveServiceConfig("dev", "web", cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	sc, err := svc.ServiceConfig("dev", "web")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if sc == nil || sc.Config != cfg {
		t.Fatalf("snapshot mismatch: %+v", sc)
	}
	// 覆盖写
	newCfg := cfg + "  replicas: 2\n"
	if err := svc.SaveServiceConfig("dev", "web", newCfg); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	sc, _ = svc.ServiceConfig("dev", "web")
	if sc.Config != newCfg {
		t.Fatalf("overwrite failed: %+v", sc)
	}
	// 删除
	if err := svc.DeleteServiceConfig("dev", "web"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if sc, _ := svc.ServiceConfig("dev", "web"); sc != nil {
		t.Fatal("snapshot should be gone after delete")
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

// TestClusterLifecycleHooks 验证集群增删触发注册回调（事件订阅管理器依赖此
// 跟随启停）。Add 成功 → onAdd(name)；Remove 成功 → onRemove(name)；失败不触发。
func TestClusterLifecycleHooks(t *testing.T) {
	svc, workerURL := newTestService(t, false)

	var added, removed []string
	svc.OnClusterAdd(func(name string) { added = append(added, name) })
	svc.OnClusterRemove(func(name string) { removed = append(removed, name) })

	// Add 触发 onAdd
	if _, err := svc.Add(context.Background(), &store.Cluster{Name: "dev", WorkerURL: workerURL}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(added) != 1 || added[0] != "dev" {
		t.Fatalf("onAdd should fire once with 'dev', got %v", added)
	}

	// Remove 触发 onRemove
	if err := svc.Remove(context.Background(), "dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(removed) != 1 || removed[0] != "dev" {
		t.Fatalf("onRemove should fire once with 'dev', got %v", removed)
	}

	// Remove 不存在的集群 → 不触发 onRemove
	if err := svc.Remove(context.Background(), "nonexistent"); err == nil {
		t.Fatal("remove nonexistent should error")
	}
	if len(removed) != 1 {
		t.Fatalf("failed remove should not fire onRemove, got %v", removed)
	}
}

// --- Worker HTTP 端点（worker_http_url）与 MCP 推导 ---

// startHTTPWorker 启动一个 /healthz 返回 200 的 mock Worker HTTP 端点
// （与 gRPC 端口分离，模拟真实 Worker 的 :8080）。
func startHTTPWorker(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAddWithHTTPEndpoint 显式提供 HTTP 端点：接入时探测 /healthz，MCP 地址
// 从 HTTP 端点推导（而非 gRPC 地址拼接）。
func TestAddWithHTTPEndpoint(t *testing.T) {
	svc, grpcURL := newTestService(t, false)
	httpURL := startHTTPWorker(t)

	added, err := svc.Add(context.Background(), &store.Cluster{
		Name: "dev", WorkerURL: grpcURL, WorkerHTTPURL: httpURL,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.WorkerHTTPURL != httpURL {
		t.Fatalf("worker_http_url = %q, want %q", added.WorkerHTTPURL, httpURL)
	}
	if want := strings.TrimRight(httpURL, "/") + "/mcp"; added.MCPURL != want {
		t.Fatalf("mcp_url = %q, want %q (derived from http endpoint)", added.MCPURL, want)
	}
}

// TestAddHTTPUnreachable HTTP 端点不可达：接入被拒（避免 MCP 通路接入后才暴露）。
func TestAddHTTPUnreachable(t *testing.T) {
	svc, grpcURL := newTestService(t, false)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadHTTP := "http://" + lis.Addr().String()
	_ = lis.Close()

	_, err = svc.Add(context.Background(), &store.Cluster{
		Name: "dev", WorkerURL: grpcURL, WorkerHTTPURL: deadHTTP,
	})
	if err == nil {
		t.Fatal("expected probe error for unreachable http endpoint")
	}
	if _, ok := err.(ErrProbeFailed); !ok {
		t.Fatalf("expected ErrProbeFailed, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "http endpoint") {
		t.Fatalf("unexpected error text: %v", err)
	}
	// 接入失败的集群不应落库。
	if c, _ := svc.GetStatic("dev"); c != nil {
		t.Fatalf("failed add should not persist, got %+v", c)
	}
}

// TestAddHealthzNon200 /healthz 非 200（端口被其他服务占用等）：接入被拒。
func TestAddHealthzNon200(t *testing.T) {
	svc, grpcURL := newTestService(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := svc.Add(context.Background(), &store.Cluster{
		Name: "dev", WorkerURL: grpcURL, WorkerHTTPURL: srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("expected /healthz 404 probe error, got %v", err)
	}
}

// TestUpdateFollowsHTTP 编辑 HTTP 端点时自动推导的 mcp_url 跟随变化；显式
// 覆盖过的 mcp_url 不被跟随。
func TestUpdateFollowsHTTP(t *testing.T) {
	svc, grpcURL := newTestService(t, false)
	http1 := startHTTPWorker(t)
	http2 := startHTTPWorker(t)

	if _, err := svc.Add(context.Background(), &store.Cluster{
		Name: "dev", WorkerURL: grpcURL, WorkerHTTPURL: http1,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// 改 HTTP 端点（不传 mcp_url）→ mcp_url 跟随推导。
	upd, err := svc.Update(context.Background(), &store.Cluster{Name: "dev", WorkerHTTPURL: http2})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if want := strings.TrimRight(http2, "/") + "/mcp"; upd.MCPURL != want {
		t.Fatalf("mcp_url should follow http endpoint, got %q want %q", upd.MCPURL, want)
	}

	// 显式覆盖 mcp_url 后再改 HTTP 端点 → mcp_url 保持不动。
	if _, err := svc.Update(context.Background(), &store.Cluster{
		Name: "dev", WorkerHTTPURL: http1, MCPURL: "http://gateway.example.com/mcp",
	}); err != nil {
		t.Fatalf("update with explicit mcp: %v", err)
	}
	upd, err = svc.Update(context.Background(), &store.Cluster{Name: "dev", WorkerHTTPURL: http2})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.MCPURL != "http://gateway.example.com/mcp" {
		t.Fatalf("explicit mcp_url should be kept, got %q", upd.MCPURL)
	}
}
