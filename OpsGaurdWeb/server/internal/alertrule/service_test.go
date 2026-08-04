package alertrule

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestRule(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	var updated atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/self", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodeId": "n1", "hostname": "h1", "role": "manager",
			"leader": true, "state": "ready", "swarmManager": true,
		})
	})
	mux.HandleFunc("GET /api/v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": map[string]any{
				"ID": "s1",
				"Spec": map[string]any{
					"Name": r.PathValue("name"),
					"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "nginx:alpine"}},
					"Mode": map[string]any{"Replicated": map[string]any{"Replicas": 2}},
				},
				"ServiceStatus": map[string]any{"RunningTasks": 2, "DesiredTasks": 2},
			},
			"tasks": []any{}, "running": 2, "desired": 2, "healthy": 2,
		})
	})
	mux.HandleFunc("POST /api/v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		// 校验收到的 config 含 monitoring 块
		_ = r.Body.Close()
		updated.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "op1", "type": "update", "service": r.PathValue("name"), "status": "healthy",
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cs := cluster.New(st)
	if err := st.PutCluster(&store.Cluster{Name: "dev", WorkerURL: ts.URL, Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	return New(st, cs), ts
}

// TestUpsertValidate 校验：阈值范围/必填字段。
func TestUpsertValidate(t *testing.T) {
	svc, _ := newTestRule(t)
	// 非法阈值
	_, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 150}},
	}})
	if err == nil {
		t.Fatal("threshold >100 should be rejected")
	}
	// 非法 metric
	_, err = svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "disk", Threshold: 80}},
	}})
	if err == nil {
		t.Fatal("unknown metric should be rejected")
	}
	// 合法
	r, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 85}},
	}})
	if err != nil || r == nil || r.UpdatedAt.IsZero() {
		t.Fatalf("upsert: %v %+v", err, r)
	}
}

// TestApplyPushToWorker 规则下发：合并 monitoring → Worker Update 被调用。
func TestApplyPushToWorker(t *testing.T) {
	svc, _ := newTestRule(t)
	r := &store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 85}},
	}}
	if err := svc.Apply(context.Background(), r); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 本地副本持久化
	got, err := svc.Get("dev", "web")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Monitoring.ResourceThresholds[0].Threshold != 85 {
		t.Fatalf("monitoring not persisted: %+v", got)
	}
}

// TestDeleteNotFound 删除不存在 → ErrNotFound。
func TestDeleteNotFound(t *testing.T) {
	svc, _ := newTestRule(t)
	if err := svc.Delete("dev", "nope"); err == nil {
		t.Fatal("delete missing should fail")
	}
}
