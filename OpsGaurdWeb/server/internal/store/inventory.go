// Package store — inventory types: declares what a cluster manages beyond
// OpsGaurd-deployed swarm services (standalone containers, host services).
//
// Design principle: NO auto-detection. Every managed object is explicitly
// declared in the inventory; "中间件" is a category label, not a type.
// See docs/集群纳管清单方案.md.
package store

import (
	"fmt"
	"strconv"
)

// InventoryConfig is the cluster's managed-object inventory (external objects
// not deployed by OpsGaurd). Stored inside store.Cluster.Inventory.
type InventoryConfig struct {
	Items []InventoryItem `json:"items" yaml:"items"`
}

// InventoryItem declares one externally-managed object.
type InventoryItem struct {
	// Name is the display name (unique within a cluster's inventory).
	Name string `json:"name" yaml:"name"`
	// Type: "standalone-container" (docker run) or "host-service" (systemd /
	// bare process / port). swarm-service is NOT an inventory type — those are
	// declared by config.Service at deploy time.
	Type string `json:"type" yaml:"type"`
	// Ref: standalone-container → container name (docker ps); host-service →
	// "host" 或 "host:port"（兼容旧格式，ref 里的端口并入 Ports 探测列表）。
	Ref string `json:"ref" yaml:"ref"`
	// Node is the hostname of the node where the object lives. Required for
	// standalone-container (so we know which node to query); optional for
	// host-service (Ref already carries the host).
	Node string `json:"node,omitempty" yaml:"node,omitempty"`
	// Ports 显式声明的端口列表（如 r-nacos 的 8848/9848/9849）。
	//   - standalone-container: 声明时作为对外端口展示；未声明则取容器实况端口
	//   - host-service: 与 ref 中的端口合并为探活端口列表，逐个探活后聚合状态
	Ports []string `json:"ports,omitempty" yaml:"ports,omitempty"`
	// Category is a classification label: "middleware", "business", "infra", ...
	// It is NOT the type — a swarm service, standalone container, and
	// host-service can all be "middleware".
	Category string `json:"category" yaml:"category"`
	// Desc is an optional human-readable description.
	Desc string `json:"desc,omitempty" yaml:"desc,omitempty"`
	// Monitoring reuses store.Monitoring (PortCheck/HTTPCheck/LogCheck/
	// ResourceThreshold) — the same schema as config.Monitoring and AlertRule,
	// so no new monitoring type is introduced.
	Monitoring *Monitoring `json:"monitoring,omitempty" yaml:"monitoring,omitempty"`
}

// Inventory type constants.
const (
	InvStandaloneContainer = "standalone-container"
	InvHostService         = "host-service"
)

// Validate checks basic field requirements. Returns nil if valid.
func (item *InventoryItem) Validate() error {
	if item.Name == "" {
		return fmt.Errorf("inventory item: name is required")
	}
	if item.Type != InvStandaloneContainer && item.Type != InvHostService {
		return fmt.Errorf("inventory item %q: invalid type %q (want %q or %q)",
			item.Name, item.Type, InvStandaloneContainer, InvHostService)
	}
	if item.Ref == "" {
		return fmt.Errorf("inventory item %q: ref is required", item.Name)
	}
	if item.Type == InvStandaloneContainer && item.Node == "" {
		return fmt.Errorf("inventory item %q: node is required for standalone-container", item.Name)
	}
	for _, p := range item.Ports {
		if !validPort(p) {
			return fmt.Errorf("inventory item %q: invalid port %q (want 1-65535)", item.Name, p)
		}
	}
	return nil
}

// validPort 校验端口字符串（1-65535 的数字）。
func validPort(p string) bool {
	if p == "" {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

// Validate checks all items in the config. Returns nil if all valid.
func (ic *InventoryConfig) Validate() error {
	seen := map[string]bool{}
	for i := range ic.Items {
		if err := ic.Items[i].Validate(); err != nil {
			return err
		}
		if seen[ic.Items[i].Name] {
			return fmt.Errorf("inventory item name %q is duplicated", ic.Items[i].Name)
		}
		seen[ic.Items[i].Name] = true
	}
	return nil
}
