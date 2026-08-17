// Package ingest persists monitor events drained from the Worker over gRPC
// (internal/ingest/subscriber.go opens a SubscribeEvents stream per cluster)
// and aggregates them into alerts. The former HTTP webhook push path is gone.
//
// Alerts are deduped/aggregated per (cluster, service, type) while active; a
// recovery event or a manual recover/ack transitions the alert. Newly created
// (or reactivated) alerts, count milestones and recoveries are dispatched to
// the notifier asynchronously, so channel latency never stalls event acks.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
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

// Notifier 告警通知出口（*notify.Service 满足；测试注入 fake）。
type Notifier interface {
	NotifyAlert(ctx context.Context, alert *store.Alert, subject string) error
}

// notifyJob 一条待分发的告警通知。
type notifyJob struct {
	alert   *store.Alert
	subject string
}

// Service 消费监控事件、维护告警并异步分发告警通知。
type Service struct {
	st *store.Store
	// dedupe 防止 Worker 重试导致同一事件重复计数。
	dedupe map[string]time.Time // event id -> 时间戳（内存滑动窗口）

	// notifier 为 nil 时退化为纯落库（测试/未接通知场景）。
	notifier Notifier
	// notifyQ 有界通知队列：HandleEvent 只入队不阻塞，渠道 RTT 不影响事件
	// ack/游标节奏；队列满丢弃（此时渠道已不可用，继续排队只会放大雪崩）。
	notifyQ chan notifyJob
	done    chan struct{}
	wg      sync.WaitGroup
}

// New 创建 ingest 服务；notifier 非 nil 时启动通知分发 worker。
func New(st *store.Store, notifier Notifier) *Service {
	s := &Service{st: st, dedupe: make(map[string]time.Time)}
	if notifier != nil {
		s.notifier = notifier
		s.notifyQ = make(chan notifyJob, 64)
		s.done = make(chan struct{})
		s.wg.Add(1)
		go s.notifyLoop()
	}
	return s
}

// Stop 停止通知分发 worker（未发送的队列项随关闭丢弃，告警本身已在库）。
func (s *Service) Stop() {
	if s.notifier == nil {
		return
	}
	close(s.done)
	wait := make(chan struct{})
	go func() { s.wg.Wait(); close(wait) }()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
	}
}

// notifyLoop 串行消费通知队列：保序、并发度恒为 1，单渠道耗时由 notify 的
// http client（10s 超时）兜底；发送错误只记日志，逐渠道明细见发送记录。
func (s *Service) notifyLoop() {
	defer s.wg.Done()
	for {
		select {
		case job := <-s.notifyQ:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.notifier.NotifyAlert(ctx, job.alert, job.subject); err != nil {
				slog.Warn("alert notify failed", "alert", job.alert.ID, "err", err)
			}
			cancel()
		case <-s.done:
			return
		}
	}
}

// enqueueNotify 非阻塞入队；队列满丢弃并记日志（风暴保护）。
func (s *Service) enqueueNotify(a *store.Alert, subject string) {
	if s.notifier == nil {
		return
	}
	select {
	case s.notifyQ <- notifyJob{alert: a, subject: subject}:
	default:
		slog.Warn("notify queue full, dropping notification", "alert", a.ID, "subject", subject)
	}
}

// milestoneCount 判断聚合次数是否到达提醒里程碑（10 的幂，从 10 起）。
func milestoneCount(n int) bool {
	if n < 10 {
		return false
	}
	for n%10 == 0 {
		n /= 10
	}
	return n == 1
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
	stored, created, err := s.st.UpsertAlert(alert)
	if err != nil {
		return err
	}
	switch {
	case created:
		// 新建（含 recovered 复发重激活）→ 通知
		s.enqueueNotify(stored, stored.Title)
	case milestoneCount(stored.Count):
		// 持续未恢复的告警在 10/100/1000… 次聚合时提醒一次
		s.enqueueNotify(stored, fmt.Sprintf("%s（已累计 %d 次，仍未恢复）", stored.Title, stored.Count))
	}
	return nil
}

// recoverByService 将指定集群+服务的 active/acked 告警标记为 recovered，
// 并对每条关闭的告警发恢复通知。
func (s *Service) recoverByService(cluster, service string) error {
	alerts, err := s.st.ListAlerts("", cluster)
	if err != nil {
		return err
	}
	for _, a := range alerts {
		if a.Service == service && (a.Status == store.AlertActive || a.Status == store.AlertAcked) {
			recovered, err := s.st.SetAlertStatus(a.ID, store.AlertRecovered, "system")
			if err != nil {
				return err
			}
			if recovered != nil {
				s.enqueueNotify(recovered, fmt.Sprintf("[%s/%s] 告警已恢复：%s", recovered.Cluster, recovered.Service, recovered.Title))
			}
		}
	}
	return nil
}
