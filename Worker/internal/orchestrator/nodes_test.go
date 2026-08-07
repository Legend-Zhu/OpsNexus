package orchestrator

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// fakeDocker is a minimal docker.Client fake for the node-aggregation timing
// test. Only ListContainers/ContainerStats carry behavior; the rest are stubs.
type fakeDocker struct {
	containers []docker.Container
	nodes      []docker.Node // ListNodes return value
	selfID     string        // SelfNode return value
	statsCalls *atomic.Int32 // counts ContainerStats invocations
	statsDelay time.Duration // simulated per-call latency
}

func (f *fakeDocker) Ping(context.Context) error                    { return nil }
func (f *fakeDocker) ServerVersion(context.Context) (string, error) { return "test", nil }
func (f *fakeDocker) Close() error                                  { return nil }

func (f *fakeDocker) ServiceCreate(context.Context, docker.ServiceSpec, string) (string, error) {
	return "", nil
}
func (f *fakeDocker) ServiceUpdate(context.Context, string, docker.ServiceSpec, string) error {
	return nil
}
func (f *fakeDocker) ServiceRemove(context.Context, string) error         { return nil }
func (f *fakeDocker) ServiceScale(context.Context, string, uint64) error  { return nil }
func (f *fakeDocker) ServiceRestart(context.Context, string) error        { return nil }
func (f *fakeDocker) ListServices(context.Context, docker.Filter) ([]docker.Service, error) {
	return nil, nil
}
func (f *fakeDocker) GetService(context.Context, string) (docker.Service, error) {
	return docker.Service{}, nil
}
func (f *fakeDocker) ImagePull(context.Context, string, string) error { return nil }
func (f *fakeDocker) GetSecret(context.Context, string) (docker.Secret, error) {
	return docker.Secret{}, nil
}
func (f *fakeDocker) ListTasks(context.Context, docker.Filter) ([]docker.Task, error) {
	return nil, nil
}
func (f *fakeDocker) ServiceTasks(context.Context, string) ([]docker.Task, error) {
	return nil, nil
}
func (f *fakeDocker) ListNodes(context.Context, docker.Filter) ([]docker.Node, error) {
	return f.nodes, nil
}
func (f *fakeDocker) SelfNode(context.Context) (docker.Node, error) {
	return docker.Node{ID: f.selfID}, nil
}
func (f *fakeDocker) Info(context.Context) (docker.Info, error)     { return docker.Info{}, nil }
func (f *fakeDocker) ServiceLogs(context.Context, string, docker.LogsOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeDocker) ListContainers(context.Context, docker.Filter) ([]docker.Container, error) {
	return f.containers, nil
}
func (f *fakeDocker) ListAllContainers(context.Context) ([]docker.Container, error) {
	return f.containers, nil
}

// ContainerStats simulates a per-call network round trip. Two calls per
// container are made by localNodeAggregate (first + second sample).
func (f *fakeDocker) ContainerStats(ctx context.Context, id string) (docker.Stats, error) {
	if f.statsCalls != nil {
		f.statsCalls.Add(1)
	}
	if f.statsDelay > 0 {
		select {
		case <-ctx.Done():
			return docker.Stats{}, ctx.Err()
		case <-time.After(f.statsDelay):
		}
	}
	return docker.Stats{}, nil
}
func (f *fakeDocker) ContainerInspect(context.Context, string) (docker.ContainerInspect, error) {
	return docker.ContainerInspect{}, nil
}
func (f *fakeDocker) ContainerExecCreate(context.Context, string, []string) (string, error) {
	return "", nil
}
func (f *fakeDocker) ExecStart(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (f *fakeDocker) ExecInspect(context.Context, string) (docker.ExecInspect, error) {
	return docker.ExecInspect{}, nil
}
func (f *fakeDocker) RestartContainer(context.Context, string) error { return nil }

// TestResolveNodeAddrSelfAndByHostname 覆盖 ResolveNodeAddr 的三种解析：
// node ID、hostname、空 id（= 本机）。
func TestResolveNodeAddrSelfAndByHostname(t *testing.T) {
	f := &fakeDocker{
		nodes: []docker.Node{
			{
				ID: "n1",
				Description: docker.NodeDescription{
					Hostname: "manager-01",
				},
				Status: docker.NodeStatus{State: "ready", Addr: "10.0.0.1"},
			},
			{
				ID: "n2",
				Description: docker.NodeDescription{
					Hostname: "worker-01",
				},
				Status: docker.NodeStatus{State: "ready", Addr: "10.0.0.2"},
			},
			{
				ID: "n3",
				Description: docker.NodeDescription{
					Hostname: "down-01",
				},
				Status: docker.NodeStatus{State: "down", Addr: "10.0.0.3"},
			},
		},
	}
	o := New(f, nil, nil)

	ctx := context.Background()

	// 按 node ID 解析。
	addr, err := o.ResolveNodeAddr(ctx, "n2")
	if err != nil || addr != "10.0.0.2" {
		t.Fatalf("by id: addr=%q err=%v", addr, err)
	}
	// 按 hostname 解析。
	addr, err = o.ResolveNodeAddr(ctx, "worker-01")
	if err != nil || addr != "10.0.0.2" {
		t.Fatalf("by hostname: addr=%q err=%v", addr, err)
	}
	// 空 id = 本机（SelfNodeID 返回 n1）。
	f.selfID = "n1"
	addr, err = o.ResolveNodeAddr(ctx, "")
	if err != nil || addr != "10.0.0.1" {
		t.Fatalf("empty id (self): addr=%q err=%v", addr, err)
	}
	// 未 ready 的节点解析失败。
	if _, err = o.ResolveNodeAddr(ctx, "down-01"); err == nil {
		t.Fatal("down node should not resolve")
	}
	// 不存在的节点解析失败。
	if _, err = o.ResolveNodeAddr(ctx, "nope"); err == nil {
		t.Fatal("unknown node should not resolve")
	}
}

