package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// mockWorker 返回一个模拟 Worker（self/healthz 固定响应，failSwarm 时非 manager）。
func mockWorker(t *testing.T, failSwarm bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /api/v1/self", func(w http.ResponseWriter, _ *http.Request) {
		role, swarm := "manager", true
		if failSwarm {
			role, swarm = "worker", false
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodeId": "abc123", "hostname": "node-1", "role": role,
			"leader": false, "state": "ready", "swarmManager": swarm, "addr": "10.0.0.1",
		})
	})
	mux.HandleFunc("GET /api/v1/local/stats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node": "node-1",
			"containers": []map[string]any{{
				"containerId": "c1", "service": "web", "cpuPercent": 3.5,
				"memPercent": 20.1, "memUsageBytes": 1048576, "memLimitBytes": 5242880,
			}},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newTestService(t *testing.T, failSwarm bool) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), mockWorker(t, failSwarm).URL
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
	ts := mockWorker(t, false)
	if _, err := svc.Add(context.Background(), &store.Cluster{Name: "dev", WorkerURL: ts.URL}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// 直接落库一个指向已关闭地址的集群（模拟 Worker 下线）
	tsOffline := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	offlineURL := tsOffline.URL
	tsOffline.Close()
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
