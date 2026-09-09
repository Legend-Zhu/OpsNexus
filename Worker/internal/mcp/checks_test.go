package mcp

import (
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"
)

func mkContainers() []orchestrator.NodeContainerInfo {
	return []orchestrator.NodeContainerInfo{
		{ID: "c1", Name: "rnacos", Image: "qingpan/rnacos:stable", State: "running", Type: "standalone", Ports: "8848->8848/tcp"},
		{ID: "c2", Name: "grafana", Image: "grafana/grafana:11", State: "running", Type: "standalone", Ports: "3000->3000/tcp"},
		{ID: "c3", Name: "admin-server.1", Image: "app/admin:1.2", State: "running", Type: "service", Service: "admin-server"},
		{ID: "c4", Name: "opsguard_worker.3", Image: "opsguard-worker:1.0.8", State: "exited", Type: "service", Service: "opsguard_worker"},
	}
}

// TestFilterNodeContainers 子串过滤命中名称/镜像/swarm 服务，大小写不敏感。
func TestFilterNodeContainers(t *testing.T) {
	// 无过滤：全量保留
	if got := filterNodeContainers(mkContainers(), "", ""); len(got) != 4 {
		t.Fatalf("no filter: expected 4, got %d", len(got))
	}
	// 名称子串
	if got := filterNodeContainers(mkContainers(), "rnacos", ""); len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("name filter: got %+v", got)
	}
	// 镜像子串 + 大小写不敏感
	if got := filterNodeContainers(mkContainers(), "GRAFANA", ""); len(got) != 1 || got[0].ID != "c2" {
		t.Fatalf("image filter: got %+v", got)
	}
	// swarm 服务名子串
	if got := filterNodeContainers(mkContainers(), "admin-server", ""); len(got) != 1 || got[0].ID != "c3" {
		t.Fatalf("service filter: got %+v", got)
	}
}

// TestFilterNodeContainersType 类型过滤：standalone 即 list_services 看不到的
// 纳管对象部分；type 与 filter 可叠加。
func TestFilterNodeContainersType(t *testing.T) {
	if got := filterNodeContainers(mkContainers(), "", "standalone"); len(got) != 2 {
		t.Fatalf("standalone filter: expected 2, got %d", len(got))
	}
	if got := filterNodeContainers(mkContainers(), "", "service"); len(got) != 2 {
		t.Fatalf("service filter: expected 2, got %d", len(got))
	}
	got := filterNodeContainers(mkContainers(), "rnacos", "standalone")
	if len(got) != 1 || got[0].ID != "c1" || got[0].Ports == "" {
		t.Fatalf("combined filter: got %+v", got)
	}
	// 组合无命中 → 空切片（非 nil，JSON 序列化为 []）
	got = filterNodeContainers(mkContainers(), "no-such", "service")
	if got == nil || len(got) != 0 {
		t.Fatalf("no-match should be empty slice, got %#v", got)
	}
}

// TestNodeContainersOutShape 输出 shape 携带 standalone 关键字段，
// 供 agent 区分 service/standalone 来源。
func TestNodeContainersOutShape(t *testing.T) {
	got := filterNodeContainers(mkContainers(), "rnacos", "")
	if got[0].Type != "standalone" || got[0].Service != "" {
		t.Fatalf("standalone container shape: %+v", got[0])
	}
	got = filterNodeContainers(mkContainers(), "opsguard_worker.3", "")
	if got[0].Type != "service" || got[0].Service != "opsguard_worker" || !strings.Contains(got[0].State, "exited") {
		t.Fatalf("service container shape: %+v", got[0])
	}
}
