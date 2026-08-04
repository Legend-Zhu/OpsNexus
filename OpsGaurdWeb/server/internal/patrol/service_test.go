package patrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// TestParseFlow 合法/非法 YAML。
func TestParseFlow(t *testing.T) {
	valid := `
name: nightly
checks:
  - type: resource
    cluster: dev
    service: web
    cpu_threshold: 85
  - type: health
    cluster: dev
    service: api
    min_replicas: 2
`
	f, err := ParseFlow(valid)
	if err != nil {
		t.Fatalf("valid flow rejected: %v", err)
	}
	if f.Name != "nightly" || len(f.Checks) != 2 || f.Checks[1].MinReplicas != 2 {
		t.Fatalf("unexpected flow: %+v", f)
	}

	for _, bad := range []string{"", "name: x", "name: x\nchecks: []",
		"name: x\nchecks:\n  - type: bogus\n    cluster: c\n    service: s"} {
		if _, err := ParseFlow(bad); err == nil {
			t.Fatalf("invalid flow accepted: %q", bad)
		}
	}
}

// TestValidCron 校验 cron 表达式。
func TestValidCron(t *testing.T) {
	if err := ValidCron("0 2 * * *"); err != nil {
		t.Fatalf("valid cron rejected: %v", err)
	}
	if err := ValidCron("bad cron"); err == nil {
		t.Fatal("invalid cron accepted")
	}
}

func mockWorker(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/self", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodeId": "n1", "hostname": "h1", "role": "manager",
			"leader": true, "state": "ready", "swarmManager": true,
		})
	})
	mux.HandleFunc("GET /api/v1/local/stats", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"node": "h1", "containers": []map[string]any{
			{"containerId": "c1", "service": "web", "cpuPercent": 95.0, "memPercent": 88.0,
				"memUsageBytes": 100, "memLimitBytes": 200},
		}})
	})
	mux.HandleFunc("GET /api/v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": map[string]any{
				"ID": "s1", "Spec": map[string]any{"Name": r.PathValue("name")},
				// workerproxy 从 Docker 原生 ServiceStatus 取 running/desired
				"ServiceStatus": map[string]any{"RunningTasks": 2, "DesiredTasks": 2},
			},
			"tasks": []any{}, "running": 2, "desired": 2, "healthy": 2,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newTestPatrol(t *testing.T) (*Service, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	url := mockWorker(t).URL
	cs := cluster.New(st)
	// 直接落库一个在线集群
	if err := st.PutCluster(&store.Cluster{Name: "dev", WorkerURL: url, Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	return New(st, cs, nil), url
}

// TestCreateAndRun 创建流程 → 立即执行 → 异常采集 + 保底报告。
func TestCreateAndRun(t *testing.T) {
	svc, _ := newTestPatrol(t)

	p, err := svc.Create("nightly", "desc", "0 2 * * *", `
name: nightly
checks:
  - type: resource
    cluster: dev
    service: web
    cpu_threshold: 80
  - type: health
    cluster: dev
    service: web
    min_replicas: 1
`, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" || !p.Enabled {
		t.Fatalf("unexpected patrol: %+v", p)
	}

	run, err := svc.Run(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != store.RunSuccess || run.Anomalies == nil || len(run.Anomalies) != 2 {
		t.Fatalf("unexpected run: %+v", run)
	}
	// resource 检查：CPU 95% > 80% → 异常
	if run.Anomalies[0].OK {
		t.Fatalf("resource check should fail: %+v", run.Anomalies[0])
	}
	// health 检查：2/2 正常
	if !run.Anomalies[1].OK {
		t.Fatalf("health check should pass: %+v", run.Anomalies[1])
	}
	// 保底报告（无 AI）
	rep, err := svc.st.GetReport(run.ID)
	if err != nil || rep == nil {
		t.Fatalf("report missing: %v", err)
	}
	if rep.Summary == "" || !contains(rep.Summary, "nightly") {
		t.Fatalf("unexpected report: %q", rep.Summary)
	}
}

// TestRunValidation 非法 YAML 创建被拒。
func TestRunValidation(t *testing.T) {
	svc, _ := newTestPatrol(t)
	if _, err := svc.Create("bad", "", "0 2 * * *", "name: x", false); err == nil {
		t.Fatal("invalid flow should be rejected")
	}
	if _, err := svc.Create("bad2", "", "not a cron", "name: x\nchecks:\n  - type: resource\n    cluster: c\n    service: s", false); err == nil {
		t.Fatal("invalid cron should be rejected")
	}
}

// TestDeleteAndRuns 删除流程 + 执行记录列表。
func TestDeleteAndRuns(t *testing.T) {
	svc, _ := newTestPatrol(t)
	p, err := svc.Create("x", "", "0 2 * * *", "name: x\nchecks:\n  - type: health\n    cluster: dev\n    service: web", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Run(context.Background(), p.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	runs, err := svc.Runs(p.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v len=%d", err, len(runs))
	}
	if err := svc.Delete(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(p.ID); err == nil {
		t.Fatal("should be not found after delete")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
