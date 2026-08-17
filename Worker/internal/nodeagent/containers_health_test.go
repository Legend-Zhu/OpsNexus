package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// fakeHealthDocker is a minimal docker.Client fake: only ContainerInspect
// carries behavior (driven by the health map), the rest are stubs.
type fakeHealthDocker struct {
	docker.Client // embedded: stubs panic if an unexpected method is called
	health        map[string]*docker.Health
	inspectErr    map[string]error
}

func (f *fakeHealthDocker) ContainerInspect(_ context.Context, id string) (docker.ContainerInspect, error) {
	if err := f.inspectErr[id]; err != nil {
		return docker.ContainerInspect{}, err
	}
	h, ok := f.health[id]
	if !ok {
		return docker.ContainerInspect{}, fmt.Errorf("No such container: %s", id)
	}
	return docker.ContainerInspect{ID: id, State: docker.ContainerState{Status: "running", Health: h}}, nil
}

// TestContainersHealth 覆盖批量健康查询：healthy/unhealthy/starting 透传、
// 无 healthcheck（Health==nil）与容器不存在/查询失败的 id 不出现在结果中。
func TestContainersHealth(t *testing.T) {
	f := &fakeHealthDocker{
		health: map[string]*docker.Health{
			"c1": {Status: "healthy"},
			"c2": {Status: "unhealthy"},
			"c3": {Status: "starting"},
			"c4": nil, // 容器存在但未配置 healthcheck
		},
		inspectErr: map[string]error{"c6": fmt.Errorf("engine boom")},
	}
	a := New(f, nil, nil)

	srv := httptest.NewServer(http.HandlerFunc(a.containersHealth))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"ids":["c1","c2","c3","c4","c5","c6",""]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, data)
	}
	var out containersHealthResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{"c1": "healthy", "c2": "unhealthy", "c3": "starting"}
	if len(out.Health) != len(want) {
		t.Fatalf("health map = %v, want %v", out.Health, want)
	}
	for id, status := range want {
		if out.Health[id] != status {
			t.Fatalf("health[%s] = %q, want %q", id, out.Health[id], status)
		}
	}
}

// TestContainersHealthEmptyIDs 空 ids 直接返回空 map，不触碰 engine。
func TestContainersHealthEmptyIDs(t *testing.T) {
	a := New(&fakeHealthDocker{}, nil, nil)
	srv := httptest.NewServer(http.HandlerFunc(a.containersHealth))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"ids":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out containersHealthResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Health == nil || len(out.Health) != 0 {
		t.Fatalf("expected empty non-nil map, got %v", out.Health)
	}
}
