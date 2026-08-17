// Package alertrule manages per-service monitoring configuration (决策⑦):
// the management plane owns the "alert rules" = the Worker's monitoring block
// for a (cluster, service). Rules are persisted here as the source of truth,
// and "apply" merges the monitoring block into the service config and pushes
// it to the Worker via its update endpoint.
package alertrule

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// ErrNotFound 规则不存在。
type ErrNotFound struct{ Cluster, Service string }

func (e ErrNotFound) Error() string {
	return fmt.Sprintf("alert rule %s/%s not found", e.Cluster, e.Service)
}

// ErrInvalid 规则非法。
type ErrInvalid struct{ Msg string }

func (e ErrInvalid) Error() string { return "invalid alert rule: " + e.Msg }

// Service 告警规则服务。
type Service struct {
	st       *store.Store
	clusters *cluster.Service
}

// New 创建告警规则服务。
func New(st *store.Store, clusters *cluster.Service) *Service {
	return &Service{st: st, clusters: clusters}
}

// Get 读取规则；不存在返回 (nil, nil)。
func (s *Service) Get(clusterName, service string) (*store.AlertRule, error) {
	return s.st.GetAlertRule(clusterName, service)
}

// List 列出规则；clusterName 非空时按集群过滤。
func (s *Service) List(clusterName string) ([]*store.AlertRule, error) {
	return s.st.ListAlertRules(clusterName)
}

// Upsert 保存规则（校验 + 更新 timestamp）。
func (s *Service) Upsert(r *store.AlertRule) (*store.AlertRule, error) {
	if r.Cluster == "" || r.Service == "" {
		return nil, ErrInvalid{"cluster and service are required"}
	}
	if err := store.ValidateMonitoring(&r.Monitoring); err != nil {
		return nil, ErrInvalid{err.Error()}
	}
	r.UpdatedAt = time.Now().UTC()
	if err := s.st.PutAlertRule(r); err != nil {
		return nil, err
	}
	return r, nil
}

// Delete 删除规则。
func (s *Service) Delete(clusterName, service string) error {
	existing, err := s.st.GetAlertRule(clusterName, service)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotFound{Cluster: clusterName, Service: service}
	}
	return s.st.DeleteAlertRule(clusterName, service)
}

// Apply 下发规则：
//   - swarm service：读服务当前详情（image/副本/端口）→ 合并 monitoring 块 →
//     构造完整 config → Worker Update（Worker Update 全量替换语义，监控随之生效）。
//   - inventory item（standalone-container / host-service）：合并 monitoring 进
//     集群纳管清单落库，由 server 侧 invmonitor 探测循环执行（Worker 不参与）。
//
// 同名歧义时 swarm service 优先。
func (s *Service) Apply(ctx context.Context, r *store.AlertRule) error {
	if err := store.ValidateMonitoring(&r.Monitoring); err != nil {
		return ErrInvalid{err.Error()}
	}
	cli, err := s.clusters.WorkerClient(r.Cluster)
	if err != nil {
		return err
	}
	d, werr := cli.GetWorkload(ctx, r.Service)
	if werr != nil {
		// 非 swarm service —— 尝试按纳管对象下发。
		ok, invErr := s.applyToInventory(ctx, r)
		if invErr != nil {
			return invErr
		}
		if !ok {
			return fmt.Errorf("get workload: %v (且 %q 不是集群 %s 的纳管对象)", werr, r.Service, r.Cluster)
		}
		r.UpdatedAt = time.Now().UTC()
		return s.st.PutAlertRule(r)
	}

	// 拼装完整服务 config（Worker 契约：service 块 + monitoring 块）
	svc := map[string]any{
		"name":  r.Service,
		"image": d.Image,
	}
	if d.Mode == "replicated" && d.Desired > 0 {
		svc["replicas"] = d.Desired
	}
	if len(d.Ports) > 0 {
		var ports []map[string]any
		for _, p := range d.Ports {
			ports = append(ports, map[string]any{"target": p.TargetPort, "published": p.PublishedPort, "protocol": p.Protocol})
		}
		svc["ports"] = ports
	}
	cfg := map[string]any{
		"service":    svc,
		"monitoring": r.Monitoring,
	}
	yamlText, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	op, err := cli.Update(ctx, r.Service, string(yamlText))
	if err != nil {
		return fmt.Errorf("worker update: %w", err)
	}
	if op.Status == "failed" {
		return fmt.Errorf("worker update failed: %s", op.Error)
	}
	// 更新本地副本时间戳
	r.UpdatedAt = time.Now().UTC()
	return s.st.PutAlertRule(r)
}

// applyToInventory 把规则合并进集群纳管清单中同名条目的 Monitoring 字段。
// 返回 ok=false 表示清单中没有该条目。
func (s *Service) applyToInventory(ctx context.Context, r *store.AlertRule) (bool, error) {
	rec, err := s.clusters.GetStatic(r.Cluster)
	if err != nil {
		return false, fmt.Errorf("get cluster: %w", err)
	}
	if rec.Inventory == nil {
		return false, nil
	}
	for i := range rec.Inventory.Items {
		if rec.Inventory.Items[i].Name == r.Service {
			m := r.Monitoring
			rec.Inventory.Items[i].Monitoring = &m
			if _, err := s.clusters.UpdateInventory(ctx, r.Cluster, rec.Inventory); err != nil {
				return false, fmt.Errorf("update inventory: %w", err)
			}
			return true, nil
		}
	}
	return false, nil
}

// SyncFromInventory 纳管清单保存后同步规则记录：清单中带 monitoring 的条目
// 各自 upsert 一条 (cluster, item.Name) 规则，使规则列表始终是"全集群监控
// 配置"的统一视图；条目从清单移除时删除对应规则（仅当规则 monitoring 与
// 旧清单条目一致时才删，避免误删用户在规则侧另建的配置）。
func (s *Service) SyncFromInventory(clusterName string, oldInv, newInv *store.InventoryConfig) error {
	if newInv != nil {
		for i := range newInv.Items {
			item := &newInv.Items[i]
			if item.Monitoring == nil || !item.Monitoring.Enabled {
				continue
			}
			r := &store.AlertRule{
				Cluster:   clusterName,
				Service:   item.Name,
				UpdatedAt: time.Now().UTC(),
			}
			r.Monitoring = *item.Monitoring
			if err := s.st.PutAlertRule(r); err != nil {
				return err
			}
		}
	}
	if oldInv != nil {
		for i := range oldInv.Items {
			name := oldInv.Items[i].Name
			if newInv != nil && inventoryHasItem(newInv, name) {
				continue
			}
			existing, err := s.st.GetAlertRule(clusterName, name)
			if err != nil {
				return err
			}
			if existing == nil {
				continue
			}
			// 仅在规则与旧清单条目内容一致时删除（规则侧另有修改的保留）。
			if oldInv.Items[i].Monitoring == nil || monitoringEqual(existing.Monitoring, *oldInv.Items[i].Monitoring) {
				if err := s.st.DeleteAlertRule(clusterName, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func inventoryHasItem(inv *store.InventoryConfig, name string) bool {
	for i := range inv.Items {
		if inv.Items[i].Name == name {
			return true
		}
	}
	return false
}

func monitoringEqual(a, b store.Monitoring) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(aj) == string(bj)
}
