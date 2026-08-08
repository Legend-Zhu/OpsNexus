package store

import "testing"

// TestInventoryItemPorts 端口列表校验：合法通过、非法拒绝；host-service
// 允许纯 host ref（端口走 Ports 声明）。
func TestInventoryItemPorts(t *testing.T) {
	base := func() *InventoryItem {
		return &InventoryItem{
			Name: "r-nacos", Type: InvStandaloneContainer, Ref: "r-nacos", Node: "node-01",
			Category: "middleware", Ports: []string{"8848", "9848", "9849"},
		}
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("valid multi-port standalone should pass: %v", err)
	}
	for _, bad := range []string{"0", "65536", "80x", "8848,9848", "abc", ""} {
		it := base()
		it.Ports = []string{bad}
		if err := it.Validate(); err == nil {
			t.Fatalf("port %q should be rejected", bad)
		}
	}

	// host-service：纯 host ref + ports 声明合法
	hs := &InventoryItem{Name: "grafana", Type: InvHostService, Ref: "10.0.0.10", Ports: []string{"3000"}}
	if err := hs.Validate(); err != nil {
		t.Fatalf("host-service with ports should pass: %v", err)
	}
	// host-service 无任何端口也接受（probe 层再判 invalid-ref）
	hsNoPort := &InventoryItem{Name: "x", Type: InvHostService, Ref: "10.0.0.10"}
	if err := hsNoPort.Validate(); err != nil {
		t.Fatalf("ref without port should be accepted at store level (probe 层再判 invalid-ref): %v", err)
	}
}
