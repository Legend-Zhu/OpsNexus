// Package alertrule manages per-service monitoring configuration (决策⑦):
// the management plane owns the "alert rules" = the Worker's monitoring block
// for a (cluster, service). Rules are persisted here as the source of truth,
// and "apply" merges the monitoring block into the service config and pushes
// it to the Worker via its update endpoint.
package alertrule

import (
	"context"
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

// List 列出全部规则。
func (s *Service) List() ([]*store.AlertRule, error) { return s.st.ListAlertRules() }

// Upsert 保存规则（校验 + 更新 timestamp）。
func (s *Service) Upsert(r *store.AlertRule) (*store.AlertRule, error) {
	if r.Cluster == "" || r.Service == "" {
		return nil, ErrInvalid{"cluster and service are required"}
	}
	if err := validateMonitoring(&r.Monitoring); err != nil {
		return nil, err
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

// Apply 将规则下发到 Worker：读服务当前详情（image/副本/端口）→ 合并
// monitoring 块 → 构造完整 config → Worker Update。
func (s *Service) Apply(ctx context.Context, r *store.AlertRule) error {
	cli, err := s.clusters.WorkerClient(r.Cluster)
	if err != nil {
		return err
	}
	d, err := cli.GetWorkload(ctx, r.Service)
	if err != nil {
		return fmt.Errorf("get workload: %w", err)
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

// validateMonitoring 校验 monitoring 块基本约束。
func validateMonitoring(m *store.Monitoring) error {
	for i, rc := range m.ResourceThresholds {
		if rc.Metric != "cpu" && rc.Metric != "memory" {
			return ErrInvalid{fmt.Sprintf("resourceThresholds[%d].metric must be cpu|memory", i)}
		}
		if rc.Threshold <= 0 || rc.Threshold > 100 {
			return ErrInvalid{fmt.Sprintf("resourceThresholds[%d].threshold must be 1-100", i)}
		}
	}
	for i, pc := range m.PortChecks {
		if pc.Port == "" {
			return ErrInvalid{fmt.Sprintf("portChecks[%d].port is required", i)}
		}
	}
	for i, hc := range m.HTTPChecks {
		if hc.URL == "" {
			return ErrInvalid{fmt.Sprintf("httpChecks[%d].url is required", i)}
		}
	}
	for i, lc := range m.LogChecks {
		if lc.Pattern == "" {
			return ErrInvalid{fmt.Sprintf("logChecks[%d].pattern is required", i)}
		}
	}
	return nil
}
