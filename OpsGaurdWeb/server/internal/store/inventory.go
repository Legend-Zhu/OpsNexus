// Package store — inventory types: declares what a cluster manages beyond
// OpsGaurd-deployed swarm services (standalone containers, host services).
//
// Design principle: NO auto-detection. Every managed object is explicitly
// declared in the inventory; "中间件" is a category label, not a type.
// See docs/集群纳管清单方案.md.
package store

import "fmt"

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
	// "host:port" (e.g. "10.60.171.231:3000").
	Ref string `json:"ref" yaml:"ref"`
	// Node is the hostname of the node where the object lives. Required for
	// standalone-container (so we know which node to query); optional for
	// host-service (Ref already carries the host).
	Node string `json:"node,omitempty" yaml:"node,omitempty"`
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
	return nil
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
