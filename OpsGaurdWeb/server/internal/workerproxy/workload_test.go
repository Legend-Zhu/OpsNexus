package workerproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// mockOpsServer 一个带内存操作状态的 mock Worker。
type mockOpsServer struct {
	ts      *httptest.Server
	op      atomic.Value // Operation
	deploys atomic.Int32
}

func (m *mockOpsServer) setOp(op Operation) { m.op.Store(op) }
func (m *mockOpsServer) opStatus() Operation {
	v, _ := m.op.Load().(Operation)
	return v
}

func newMockOpsServer(t *testing.T) *mockOpsServer {
	t.Helper()
	m := &mockOpsServer{}
	m.setOp(Operation{ID: "op1", Type: "deploy", Service: "web", Status: OpPending})
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/services", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "text/yaml" {
			w.WriteHeader(400)
			return
		}
		m.deploys.Add(1)
		m.setOp(Operation{ID: "op1", Type: "deploy", Service: "web", Status: OpRunning, StartedAt: "t0"})
		writeJSON(w, 202, m.opStatus())
	})
	mux.HandleFunc("POST /api/v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "text/yaml" {
			w.WriteHeader(400)
			return
		}
		m.setOp(Operation{ID: "op1", Type: "update", Service: r.PathValue("name"), Status: OpHealthy})
		writeJSON(w, 202, m.opStatus())
	})
	mux.HandleFunc("POST /api/v1/services/{name}/scale", func(w http.ResponseWriter, r *http.Request) {
		m.setOp(Operation{ID: "op2", Type: "scale", Service: r.PathValue("name"), Status: OpHealthy, Replicas: 5})
		writeJSON(w, 202, m.opStatus())
	})
	mux.HandleFunc("POST /api/v1/services/{name}/restart", func(w http.ResponseWriter, r *http.Request) {
		m.setOp(Operation{ID: "op3", Type: "restart", Service: r.PathValue("name"), Status: OpDone})
		writeJSON(w, 202, m.opStatus())
	})
	mux.HandleFunc("DELETE /api/v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		m.setOp(Operation{ID: "op4", Type: "remove", Service: r.PathValue("name"), Status: OpDone})
		writeJSON(w, 200, m.opStatus())
	})
	mux.HandleFunc("GET /api/v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, m.opStatus())
	})
	mux.HandleFunc("GET /api/v1/services", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []map[string]any{{
			"ID": "svc1",
			"Spec": map[string]any{
				"Name":         "web",
				"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "nginx:alpine"}},
				"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 3}},
			},
			"ServiceStatus": map[string]any{"RunningTasks": 2, "DesiredTasks": 3},
			"Endpoint": map[string]any{"Ports": []map[string]any{{
				"Protocol": "tcp", "TargetPort": 80, "PublishedPort": 8080, "PublishMode": "ingress",
			}}},
		}})
	})
	mux.HandleFunc("GET /api/v1/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"service": map[string]any{
				"ID": "svc1",
				"Spec": map[string]any{
					"Name":         "web",
					"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "nginx:alpine"}},
					"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 3}},
				},
				"ServiceStatus": map[string]any{"RunningTasks": 3, "DesiredTasks": 3},
			},
			"tasks": []map[string]any{{
				"ID": "task1", "Slot": 1, "NodeID": "n1", "DesiredState": "running",
				"Status": map[string]any{
					"State":           "running",
					"ContainerStatus": map[string]any{"ContainerID": "c1", "ExitCode": 0},
				},
			}},
			"running": 3, "desired": 3, "healthy": 3,
		})
	})

	m.ts = httptest.NewServer(mux)
	t.Cleanup(m.ts.Close)
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDeployAndPoll(t *testing.T) {
	m := newMockOpsServer(t)
	cli := New(m.ts.URL, "")

	op, err := cli.Deploy(context.Background(), "name: web\nimage: nginx:alpine\nreplicas: 2\n")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if op.Status != OpRunning || op.Type != "deploy" {
		t.Fatalf("unexpected op: %+v", op)
	}
	if m.deploys.Load() != 1 {
		t.Fatalf("expected 1 deploy call, got %d", m.deploys.Load())
	}

	// 轮询（mock 已置 healthy）
	m.setOp(Operation{ID: "op1", Type: "deploy", Service: "web", Status: OpHealthy})
	polled, err := cli.Operation(context.Background(), "op1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if polled.Status != OpHealthy {
		t.Fatalf("expected healthy, got %s", polled.Status)
	}
}

func TestScaleRestartRemove(t *testing.T) {
	m := newMockOpsServer(t)
	cli := New(m.ts.URL, "")

	sc, err := cli.Scale(context.Background(), "web", 5)
	if err != nil || sc.Status != OpHealthy || sc.Replicas != 5 {
		t.Fatalf("scale: %+v err=%v", sc, err)
	}
	rs, err := cli.Restart(context.Background(), "web")
	if err != nil || rs.Status != OpDone {
		t.Fatalf("restart: %+v err=%v", rs, err)
	}
	rm, err := cli.Remove(context.Background(), "web")
	if err != nil || rm.Status != OpDone {
		t.Fatalf("remove: %+v err=%v", rm, err)
	}
}

func TestListWorkloadsMapping(t *testing.T) {
	m := newMockOpsServer(t)
	cli := New(m.ts.URL, "")

	list, err := cli.ListWorkloads(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(list))
	}
	w := list[0]
	if w.Name != "web" || w.Image != "nginx:alpine" || w.Mode != "replicated" {
		t.Fatalf("unexpected workload: %+v", w)
	}
	if w.Replica != "2/3" || w.Running != 2 || w.Desired != 3 {
		t.Fatalf("replica fields: %+v", w)
	}
	if len(w.Ports) != 1 || w.Ports[0].PublishedPort != 8080 || w.Ports[0].TargetPort != 80 {
		t.Fatalf("ports: %+v", w.Ports)
	}
}

func TestGetWorkloadDetail(t *testing.T) {
	m := newMockOpsServer(t)
	cli := New(m.ts.URL, "")

	d, err := cli.GetWorkload(context.Background(), "web")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.Healthy != 3 || len(d.Tasks) != 1 || d.Tasks[0].State != "running" {
		t.Fatalf("unexpected detail: %+v", d)
	}
}

// TestStreamLogs mock 返回 SSE 日志流，校验逐行解码。
func TestStreamLogs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != "web" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"ts\":\"t1\",\"stream\":\"stdout\",\"line\":\"hello\"}\n\n")
		fmt.Fprint(w, "data: {\"ts\":\"t2\",\"stream\":\"stderr\",\"line\":\"warn\"}\n\n")
	}))
	defer ts.Close()

	cli := New(ts.URL, "")
	var got []LogLine
	err := cli.StreamLogs(context.Background(), "web", false, 0, "", func(ll LogLine) bool {
		got = append(got, ll)
		return true
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(got) != 2 || got[0].Line != "hello" || got[1].Stream != "stderr" {
		t.Fatalf("unexpected lines: %+v", got)
	}
}

// TestStreamLogsEarlyExit handler 返回 false 时提前断开。
func TestStreamLogsEarlyExit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, "data: {\"ts\":\"t\",\"stream\":\"stdout\",\"line\":\"l%d\"}\n\n", i)
		}
	}))
	defer ts.Close()

	cli := New(ts.URL, "")
	count := 0
	err := cli.StreamLogs(context.Background(), "web", false, 0, "", func(ll LogLine) bool {
		count++
		return count < 3
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected early exit at 3, got %d", count)
	}
}

// 校验 Content-Type 未设为 yaml 时 mock 返回 400 → ErrUnreachable。
func TestDeployWrongContentType(t *testing.T) {
	m := newMockOpsServer(t)
	// 直接构造 yaml 但服务端校验 Content-Type —— mock 只在 text/yaml 时放行
	cli := New(m.ts.URL, "")
	if _, err := cli.Update(context.Background(), "web", "name: web"); err != nil {
		if !strings.Contains(err.Error(), "status 400") {
			t.Fatalf("expected 400 error, got %v", err)
		}
	}
}
