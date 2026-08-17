package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// fakeHealthDocker is a docker.Client fake for the cross-node healthy
// aggregation tests: SelfNode/ListNodes are field-driven, ContainerInspect
// answers per-container health locally.
type fakeHealthDocker struct {
	nodes   []docker.Node
	selfID  string
	health  map[string]*docker.Health // local inspect answers
	selfErr error                     // SelfNode failure injection
}

func (f *fakeHealthDocker) Ping(context.Context) error                    { return nil }
func (f *fakeHealthDocker) ServerVersion(context.Context) (string, error) { return "test", nil }
func (f *fakeHealthDocker) Close() error                                  { return nil }
func (f *fakeHealthDocker) ServiceCreate(context.Context, docker.ServiceSpec, string) (string, error) {
	return "", nil
}
func (f *fakeHealthDocker) ServiceUpdate(context.Context, string, docker.ServiceSpec, string) error {
	return nil
}
func (f *fakeHealthDocker) ServiceRemove(context.Context, string) error        { return nil }
func (f *fakeHealthDocker) ServiceScale(context.Context, string, uint64) error { return nil }
func (f *fakeHealthDocker) ServiceRestart(context.Context, string) error       { return nil }
func (f *fakeHealthDocker) ListServices(context.Context, docker.Filter) ([]docker.Service, error) {
	return nil, nil
}
func (f *fakeHealthDocker) GetService(context.Context, string) (docker.Service, error) {
	return docker.Service{}, nil
}
func (f *fakeHealthDocker) ImagePull(context.Context, string, string) error { return nil }
func (f *fakeHealthDocker) GetSecret(context.Context, string) (docker.Secret, error) {
	return docker.Secret{}, nil
}
func (f *fakeHealthDocker) ListTasks(context.Context, docker.Filter) ([]docker.Task, error) {
	return nil, nil
}
func (f *fakeHealthDocker) ServiceTasks(context.Context, string) ([]docker.Task, error) {
	return nil, nil
}
func (f *fakeHealthDocker) ListNodes(context.Context, docker.Filter) ([]docker.Node, error) {
	return f.nodes, nil
}
func (f *fakeHealthDocker) SelfNode(context.Context) (docker.Node, error) {
	if f.selfErr != nil {
		return docker.Node{}, f.selfErr
	}
	return docker.Node{ID: f.selfID}, nil
}
func (f *fakeHealthDocker) Info(context.Context) (docker.Info, error) { return docker.Info{}, nil }
func (f *fakeHealthDocker) ServiceLogs(context.Context, string, docker.LogsOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeHealthDocker) ListContainers(context.Context, docker.Filter) ([]docker.Container, error) {
	return nil, nil
}
func (f *fakeHealthDocker) ListAllContainers(context.Context) ([]docker.Container, error) {
	return nil, nil
}
func (f *fakeHealthDocker) ContainerStats(context.Context, string) (docker.Stats, error) {
	return docker.Stats{}, nil
}
func (f *fakeHealthDocker) ContainerInspect(_ context.Context, id string) (docker.ContainerInspect, error) {
	h, ok := f.health[id]
	if !ok {
		return docker.ContainerInspect{}, fmt.Errorf("No such container: %s", id)
	}
	return docker.ContainerInspect{ID: id, State: docker.ContainerState{Status: "running", Health: h}}, nil
}
func (f *fakeHealthDocker) ContainerExecCreate(context.Context, string, []string) (string, error) {
	return "", nil
}
func (f *fakeHealthDocker) ExecStart(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeHealthDocker) ExecInspect(context.Context, string) (docker.ExecInspect, error) {
	return docker.ExecInspect{}, nil
}
func (f *fakeHealthDocker) RestartContainer(context.Context, string) error { return nil }

func runningTask(id, nodeID, cid string) docker.Task {
	return docker.Task{
		ID:           id,
		NodeID:       nodeID,
		DesiredState: "running",
		Status: docker.TaskStatus{
			State:           "running",
			ContainerStatus: docker.ContainerStatus{ContainerID: cid},
		},
	}
}

// TestCountHealthyLocalOnly 单节点 swarm：task 无节点归属或归属本机时全部走
// 本地 engine，与原路径一致。
func TestCountHealthyLocalOnly(t *testing.T) {
	f := &fakeHealthDocker{
		selfID: "n1",
		health: map[string]*docker.Health{
			"c1": {Status: "healthy"},
			"c2": {Status: "unhealthy"},
			"c3": nil, // 容器存在但无 healthcheck 数据
		},
	}
	o := New(f, nil, nil)

	tasks := []docker.Task{
		runningTask("t1", "n1", "c1"),
		runningTask("t2", "n1", "c2"),
		runningTask("t3", "n1", "c3"),
		runningTask("t4", "", "c4"), // NodeID 空 → 本机路径；c4 不存在 → 不健康
	}
	if got := o.countHealthy(context.Background(), tasks); got != 1 {
		t.Fatalf("countHealthy = %d, want 1", got)
	}
}

// TestCountHealthyCrossNode 本机 + 远端混合：远端 task 按节点分组、每节点一次
// 批量 HTTP 调用；不可达节点（HTTP 失败）与无地址节点（not ready）按不健康计。
func TestCountHealthyCrossNode(t *testing.T) {
	// 模拟 n2 的节点 worker：c1/c3 healthy、c2 unhealthy。
	var n2Calls int
	srvN2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2Calls++
		var req struct {
			IDs []string `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if len(req.IDs) != 3 {
			t.Errorf("n2 batch should carry 3 ids, got %v", req.IDs)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node":   "worker-01",
			"health": map[string]string{"c1": "healthy", "c2": "unhealthy", "c3": "healthy"},
		}) //nolint:errcheck
	}))
	t.Cleanup(srvN2.Close)
	// 模拟 n3 的节点 worker：一律 500（含旧版 worker 无此端点的场景）。
	srvN3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srvN3.Close)

	hostN2 := strings.TrimPrefix(srvN2.URL, "http://")
	hostN3 := strings.TrimPrefix(srvN3.URL, "http://")
	f := &fakeHealthDocker{
		selfID: "n1",
		nodes: []docker.Node{
			{ID: "n1", Status: docker.NodeStatus{State: "ready", Addr: "127.0.0.1"}},
			{ID: "n2", Status: docker.NodeStatus{State: "ready", Addr: hostN2}},
			{ID: "n3", Status: docker.NodeStatus{State: "ready", Addr: hostN3}},
			// n4 不在 nodeAddrs 结果中（not ready）→ 无地址，按不健康计。
		},
		health: map[string]*docker.Health{
			"c0": {Status: "healthy"}, // 本机容器
		},
	}
	o := New(f, nil, nil)

	tasks := []docker.Task{
		runningTask("t0", "n1", "c0"), // 本机 healthy
		runningTask("t1", "n2", "c1"), // 远端 healthy
		runningTask("t2", "n2", "c2"), // 远端 unhealthy
		runningTask("t3", "n2", "c3"), // 远端 healthy
		runningTask("t4", "n3", "c4"), // 节点 HTTP 失败 → 不计
		runningTask("t5", "n4", "c5"), // 节点无地址 → 不计
		runningTask("t6", "n1", "c6"), // 本机不存在 → 不计
	}
	if got := o.countHealthy(context.Background(), tasks); got != 3 {
		t.Fatalf("countHealthy = %d, want 3", got)
	}
	if n2Calls != 1 {
		t.Fatalf("n2 should be queried exactly once (batched), got %d", n2Calls)
	}
}

// TestCountHealthySelfUnknown SelfNode 失败时全部按本机处理（保持旧的
// 单引擎语义），远端容器在本地查不到 → 不计健康，但绝不 panic / error。
func TestCountHealthySelfUnknown(t *testing.T) {
	f := &fakeHealthDocker{
		selfErr: fmt.Errorf("not part of a swarm"),
		health:  map[string]*docker.Health{"c1": {Status: "healthy"}},
	}
	o := New(f, nil, nil)

	tasks := []docker.Task{
		runningTask("t1", "n9", "c1"), // 非本机声明，但 fallback 后走本地 → healthy
		runningTask("t2", "n9", "c2"), // 本地查不到 → 不计
	}
	if got := o.countHealthy(context.Background(), tasks); got != 1 {
		t.Fatalf("countHealthy = %d, want 1", got)
	}
}

// TestCountHealthySkipsDefaultPort NodeClient 默认会补 :8080 端口；httptest
// 地址自带端口，验证 hasPort 分支不会拼出非法地址（顺带覆盖 addr 带端口路径）。
func TestCountHealthyNodeAddrWithPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node": "w", "health": map[string]string{"c1": "healthy"},
		}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHealthDocker{
		selfID: "n1",
		nodes:  []docker.Node{{ID: "n2", Status: docker.NodeStatus{State: "ready", Addr: u.Host}}},
	}
	o := New(f, nil, nil)
	if got := o.countHealthy(context.Background(), []docker.Task{runningTask("t1", "n2", "c1")}); got != 1 {
		t.Fatalf("countHealthy = %d, want 1", got)
	}
}
