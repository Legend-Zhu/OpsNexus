// Package ingest persists monitor events drained from the Worker over gRPC
// (internal/ingest/subscriber.go opens a SubscribeEvents stream per cluster)
// and aggregates them into alerts. The former HTTP webhook push path is gone.
//
// Alerts are deduped/aggregated per (cluster, service, type) while active; a
// recovery event or a manual recover/ack transitions the alert.
package ingest

import (
	"fmt"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// AlertTitle 由事件生成默认告警标题。
func AlertTitle(cluster string, e *store.IngestEvent) string {
	switch e.Type {
	case store.EventPortDown:
		return fmt.Sprintf("[%s] 端口不可达：%s", cluster, e.Msg)
	case store.EventHTTPUnhealthy:
		return fmt.Sprintf("[%s] HTTP 健康检查失败：%s", cluster, e.Msg)
	case store.EventLogMatch:
		return fmt.Sprintf("[%s] 日志异常匹配：%s", cluster, e.Msg)
	case store.EventResourceOver:
		return fmt.Sprintf("[%s] 资源超限：%s", cluster, e.Msg)
	case store.EventContainerDown:
		return fmt.Sprintf("[%s] 容器不可用：%s", cluster, e.Msg)
	default:
		return fmt.Sprintf("[%s] %s：%s", cluster, e.Type, e.Msg)
	}
}

// RecoverTypeOf 判断事件是否触发恢复（resource_recovered / recovered 通用恢复）。
func RecoverTypeOf(e *store.IngestEvent) bool {
	return e.Type == store.EventResourceRecover || e.Type == store.EventRecovered
}

// Service 处理 webhook 推送并维护告警。
type Service struct {
	st *store.Store
	// dedupe 防止 Worker 重试导致同一事件重复计数。
	dedupe map[string]time.Time // event id -> 时间戳（内存滑动窗口）
}

// New 创建 ingest 服务。
func New(st *store.Store) *Service {
	return &Service{st: st, dedupe: make(map[string]time.Time)}
}

// HandleEvent 处理一条监控事件：落库 + 更新/新建告警。
func (s *Service) HandleEvent(cluster string, e *store.IngestEvent) error {
	e.Cluster = cluster
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	if e.ID == "" {
		e.ID = fmt.Sprintf("ev-%d", time.Now().UnixNano())
	}
	// 幂等去重（10 分钟窗口）
	if ts, ok := s.dedupe[e.ID]; ok && time.Since(ts) < 10*time.Minute {
		return nil
	}
	s.dedupe[e.ID] = time.Now().UTC()
	// 定期清理过期去重项
	if len(s.dedupe) > 10000 {
		for k, ts := range s.dedupe {
			if time.Since(ts) > 10*time.Minute {
				delete(s.dedupe, k)
			}
		}
	}

	if _, err := s.st.SaveEvent(e); err != nil {
		return fmt.Errorf("save event: %w", err)
	}

	// 恢复事件 → 关闭 active/acked 告警
	if RecoverTypeOf(e) {
		return s.recoverByService(cluster, e.Service)
	}

	// 告警聚合 key：(cluster, service, type) → 确定性斜杠安全 ID
	alert := &store.Alert{
		ID:          store.AlertID(e.Cluster, e.Service, e.Type),
		Cluster:     e.Cluster,
		Service:     e.Service,
		Type:        e.Type,
		Level:       e.Level,
		Title:       AlertTitle(cluster, e),
		Status:      store.AlertActive,
		Count:       1,
		FirstTS:     e.TS,
		LastTS:      e.TS,
		LastEventID: e.ID,
	}
	return s.st.UpsertAlert(alert)
}

// recoverByService 将指定集群+服务的 active/acked 告警标记为 recovered。
func (s *Service) recoverByService(cluster, service string) error {
	alerts, err := s.st.ListAlerts("", cluster)
	if err != nil {
		return err
	}
	for _, a := range alerts {
		if a.Service == service && (a.Status == store.AlertActive || a.Status == store.AlertAcked) {
			if _, err := s.st.SetAlertStatus(a.ID, store.AlertRecovered, "system"); err != nil {
				return err
			}
		}
	}
	return nil
}
