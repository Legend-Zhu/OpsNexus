// Package alertrule manages per-service monitoring configuration (决策⑦):
// the management plane owns the "alert rules" = the Worker's monitoring block
// for a (cluster, service). Rules are persisted here as the source of truth,
// and "apply" merges the monitoring block into the service config and pushes
// it to the Worker via its update endpoint.
package alertrule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNotFound 规则不存在。
type ErrNotFound struct{ Cluster, Service string }

func (e ErrNotFound) Error() string {
	return fmt.Sprintf("alert rule %s/%s not found", e.Cluster, e.Service)
}

// ErrInvalid 规则非法。
type ErrInvalid struct{ Msg string }

func (e ErrInvalid) Error() string { return "invalid alert rule: " + e.Msg }

// ErrStopFailed 停止生效监控失败（规则未删除；可重试或 force 强删）。
type ErrStopFailed struct{ Err error }

func (e ErrStopFailed) Error() string { return "停止监控失败，规则未删除: " + e.Err.Error() }
func (e ErrStopFailed) Unwrap() error { return e.Err }

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

// DeleteResult 删除结果。
type DeleteResult struct {
	Deleted string `json:"deleted"`            // "cluster/service"
	Stopped bool   `json:"stopped"`            // 生效监控是否已成功停止
	StopErr string `json:"stopError,omitempty"` // force 删除时停止失败的原因
}

// Delete 删除规则，并先停止该规则已生效的监控（Apply 的逆操作）：
//   - swarm service：向 Worker 下发 enabled: false 配置（Update 全量替换，监控必停）；
//   - inventory item：清除清单同名条目的 Monitoring 声明（invmonitor 探测循环随即不再执行）。
//
// 停止失败时：默认返回错误并保留规则（fail-fast，状态仍可补救）；force=true 时
// 强删规则，并在 DeleteResult 中如实报告监控未停止。目标本无生效监控可停
// （从未下发、服务/条目已不存在、集群已删除）时视为已停止，直接删除。
func (s *Service) Delete(ctx context.Context, clusterName, service string, force bool) (*DeleteResult, error) {
	existing, err := s.st.GetAlertRule(clusterName, service)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound{Cluster: clusterName, Service: service}
	}

	stopped, stopErr := s.stopActive(ctx, clusterName, service)
	if stopErr != nil && !force {
		return nil, ErrStopFailed{Err: stopErr}
	}
	if err := s.st.DeleteAlertRule(clusterName, service); err != nil {
		return nil, err
	}
	res := &DeleteResult{Deleted: clusterName + "/" + service, Stopped: stopped}
	if stopErr != nil {
		res.StopErr = stopErr.Error()
	}
	return res, nil
}

// RemoveCluster 删除集群下全部规则（集群删除回调）。集群既已删除，
// 规则的生效点不复存在、也永不可再下发，直接清理避免孤儿记录。
func (s *Service) RemoveCluster(clusterName string) error {
	rules, err := s.st.ListAlertRules(clusterName)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if err := s.st.DeleteAlertRule(clusterName, r.Service); err != nil {
			return err
		}
	}
	return nil
}

// stopActive 停止规则在生效点的监控（幂等，可重复执行）。
// 目标判定与 Apply 一致：swarm service 优先，否则按纳管清单条目处理；
// 与 Apply 不同的是，Worker 不可达等传输错误视为停止失败（无法确认监控状态，
// 不能像 Apply 那样静默降级到清单），仅服务确不存在（NotFound）才回退清单。
func (s *Service) stopActive(ctx context.Context, clusterName, service string) (bool, error) {
	cli, err := s.clusters.WorkerClient(clusterName)
	if err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			return true, nil // 集群已删除，生效点不存在
		}
		return false, fmt.Errorf("worker client: %w", err)
	}
	d, werr := cli.GetWorkload(ctx, service)
	if werr != nil {
		if !isServiceNotFound(werr) {
			return false, fmt.Errorf("get workload: %w", werr)
		}
		// 服务确不存在 —— 清除纳管清单同名条目的监控声明（若存在）。
		if _, invErr := s.disableInInventory(ctx, clusterName, service); invErr != nil {
			return false, invErr
		}
		return true, nil
	}
	yamlText, err := buildWorkloadConfig(service, d, store.Monitoring{Enabled: false})
	if err != nil {
		return false, err
	}
	op, err := cli.Update(ctx, service, yamlText)
	if err != nil {
		return false, fmt.Errorf("worker update: %w", err)
	}
	if op.Status == "failed" {
		return false, fmt.Errorf("worker update failed: %s", op.Error)
	}
	return true, nil
}

// disableInInventory 清除纳管清单中同名条目的 Monitoring 字段（applyToInventory 的逆操作）。
// 返回 ok=false 表示清单中没有该条目或条目本就未声明监控。
func (s *Service) disableInInventory(ctx context.Context, clusterName, service string) (bool, error) {
	rec, err := s.clusters.GetStatic(clusterName)
	if err != nil {
		return false, fmt.Errorf("get cluster: %w", err)
	}
	if rec.Inventory == nil {
		return false, nil
	}
	for i := range rec.Inventory.Items {
		if rec.Inventory.Items[i].Name == service {
			if rec.Inventory.Items[i].Monitoring == nil {
				return false, nil
			}
			rec.Inventory.Items[i].Monitoring = nil
			if _, err := s.clusters.UpdateInventory(ctx, clusterName, rec.Inventory); err != nil {
				return false, fmt.Errorf("update inventory: %w", err)
			}
			return true, nil
		}
	}
	return false, nil
}

// isServiceNotFound 判断 GetWorkload 错误是否为"服务不存在"（Worker 返回
// codes.NotFound）；连接失败/超时等传输错误不是。
func isServiceNotFound(err error) bool {
	var unreach *workerproxy.ErrUnreachable
	if errors.As(err, &unreach) {
		return status.Code(unreach.Err) == codes.NotFound
	}
	return status.Code(err) == codes.NotFound
}

// buildWorkloadConfig 按工作负载现状与监控块拼装完整服务 config YAML
// （Worker 契约：service 块 + monitoring 块；Update 为全量替换语义）。
func buildWorkloadConfig(service string, d workerproxy.WorkloadDetail, monitoring store.Monitoring) (string, error) {
	svc := map[string]any{
		"name":  service,
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
		"monitoring": monitoring,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	return string(data), nil
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
	yamlText, err := buildWorkloadConfig(r.Service, d, r.Monitoring)
	if err != nil {
		return err
	}
	op, err := cli.Update(ctx, r.Service, yamlText)
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
