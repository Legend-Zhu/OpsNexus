// provider call 用量计量 collector（MLOps P2，方案 §4.3/§4.7）。
//
// Service 实现 usage.Sink：Metered 装饰器在每次底层调用结束后回调
// Record。Record 只做非阻塞入队（有界队列，满则丢弃计数），消费循环
// 单 goroutine 串行完成计价快照 + 落库（明细 + call_id 幂等索引 +
// 日聚合同一批提交）。计量永不阻塞用户响应。
package mlops

import (
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// UsageSettings collector 配置（main 从 MlopsConfig 装配）。
type UsageSettings struct {
	Timezone   *time.Location // 业务时区（nil = 本机时区）
	Currency   string         // 当前固定 CNY
	RetainDays int            // 明细保留天数（默认 90；日聚合永久保留）
	QueueSize  int            // 异步队列容量（默认 2048）
	GCInterval time.Duration  // 周期 GC 间隔（默认 24h；启动时先跑一次）
}

const opDayCap = 16384 // operation 去重表防膨胀上限（超限清空，Operations 为估算值）

// StartUsage 启动计量 collector（幂等；main 在 SetUsageSink 之前调用）。
func (s *Service) StartUsage(cfg UsageSettings) {
	if cfg.Timezone != nil {
		s.loc = cfg.Timezone
	}
	if cfg.Currency != "" {
		s.currency = cfg.Currency
	}
	s.retainDays = cfg.RetainDays
	if s.retainDays <= 0 {
		s.retainDays = 90
	}
	gcEvery := cfg.GCInterval
	if gcEvery <= 0 {
		gcEvery = 24 * time.Hour
	}
	qsize := cfg.QueueSize
	if qsize <= 0 {
		qsize = 2048
	}

	s.usageMu.Lock()
	if s.usageRunning {
		s.usageMu.Unlock()
		return
	}
	s.usageQueue = make(chan usage.Record, qsize)
	s.usageStop = make(chan struct{})
	s.usageRunning = true
	stop := s.usageStop
	queue := s.usageQueue
	s.usageMu.Unlock()

	s.usageWG.Add(2)
	go s.usageConsumeLoop(queue, stop)
	go s.usageGCLoop(gcEvery, stop)
}

// StopUsage 停止 collector：排空队列中已有记录后返回（关停不丢已入队数据）。
func (s *Service) StopUsage() {
	s.usageMu.Lock()
	if !s.usageRunning {
		s.usageMu.Unlock()
		return
	}
	s.usageRunning = false
	close(s.usageStop)
	s.usageMu.Unlock()
	s.usageWG.Wait()
}

// Record 实现 usage.Sink：非阻塞入队；队列满丢弃并计数（绝不阻塞调用方）。
func (s *Service) Record(r usage.Record) {
	s.usageMu.Lock()
	q := s.usageQueue
	s.usageMu.Unlock()
	if q == nil {
		return // 未启用计量
	}
	select {
	case q <- r:
	default:
		n := s.usageDropped.Add(1)
		if n%1000 == 1 {
			s.logger.Printf("usage queue full, dropping records (dropped=%d)", n)
		}
	}
}

// usageConsumeLoop 消费队列：计价快照 + 落库。stop 触发时先排空再退出。
func (s *Service) usageConsumeLoop(q <-chan usage.Record, stop <-chan struct{}) {
	defer s.usageWG.Done()
	for {
		select {
		case r := <-q:
			s.persistUsage(r)
		case <-stop:
			for {
				select {
				case r := <-q:
					s.persistUsage(r)
				default:
					return
				}
			}
		}
	}
}

// persistUsage 计价并落库一条记录。
func (s *Service) persistUsage(r usage.Record) {
	rec := &store.MLUsageRecord{
		OperationID:      r.OperationID,
		CallID:           r.CallID,
		ParentCallID:     r.ParentCallID,
		Provider:         r.Provider,
		Model:            r.Model,
		Scenario:         r.Scenario,
		EntryPoint:       r.EntryPoint,
		Round:            r.Round,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		TotalTokens:      r.TotalTokens,
		UsagePresent:     r.UsagePresent,
		OK:               r.OK,
		Status:           r.Status,
		Error:            r.Error,
		StartedAt:        r.StartedAt.UTC(),
		FinishedAt:       r.FinishedAt.UTC(),
		LatencyMs:        r.LatencyMs,
		Day:              r.StartedAt.In(s.loc).Format("2006-01-02"),
	}
	// 计价快照：有 usage 且配置了单价才计价（priced=true 金额可为 0 = 免费；
	// 无单价或无 usage 均为 priced=false，只计 token 不计费用）。
	if p := s.pricingFor(r.Provider, r.Model); p != nil && r.UsagePresent {
		rec.Priced = true
		rec.Currency = p.Currency
		rec.PricingVersion = p.Version
		rec.CostMinor = usageCostMinor(p, r.PromptTokens, r.CompletionTokens)
	}
	bumpOp := false
	if r.OperationID != "" {
		bumpOp = s.markOperationDay(r.OperationID, rec.Day)
	}
	saved, err := s.st.RecordMLUsage(rec, bumpOp)
	if err != nil {
		s.usageFailed.Add(1)
		s.logger.Printf("persist usage call %s failed: %v", r.CallID, err)
		return
	}
	if !saved {
		s.usageDeduped.Add(1) // call_id 重复完成事件，幂等跳过
	}
}

// usageCostMinor 定点计价：tokens × 微元单价 / 1e6，整数截断到微元。
func usageCostMinor(p *store.MLPricing, promptTokens, completionTokens int) int64 {
	in := int64(promptTokens) * p.PriceInPerMMicro
	out := int64(completionTokens) * p.PriceOutPerMMicro
	return (in + out) / 1_000_000
}

// markOperationDay operation 当日首条入账返回 true（日聚合 Operations
// 计数用）。重启/超限清空后为估算值，可接受轻微偏差。
func (s *Service) markOperationDay(opID, day string) bool {
	s.opDayMu.Lock()
	defer s.opDayMu.Unlock()
	if d, ok := s.opDay[opID]; ok && d == day {
		return false
	}
	if len(s.opDay) >= opDayCap {
		s.opDay = make(map[string]string, opDayCap/2)
	}
	s.opDay[opID] = day
	return true
}

// usageGCLoop 启动先清理一次，之后周期执行（限量分批）。
func (s *Service) usageGCLoop(every time.Duration, stop <-chan struct{}) {
	defer s.usageWG.Done()
	s.runUsageGC()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.runUsageGC()
		case <-stop:
			return
		}
	}
}

// runUsageGC 清理超过保留期的明细（含幂等索引）；日聚合永久保留。
func (s *Service) runUsageGC() {
	cutoff := time.Now().AddDate(0, 0, -s.retainDays)
	total := 0
	for {
		n, err := s.st.GCMLUsageBefore(cutoff, 500)
		if err != nil {
			s.logger.Printf("usage gc: %v", err)
			return
		}
		total += n
		if n < 500 {
			break // 清理完毕
		}
	}
	if total > 0 {
		s.logger.Printf("usage gc removed %d expired detail records (retain=%dd)", total, s.retainDays)
	}
}

// CollectorStats collector 运行状态（报表可观测）。
type CollectorStats struct {
	QueueLen  int   `json:"queue_len"`
	QueueCap  int   `json:"queue_cap"`
	Dropped   int64 `json:"dropped"`
	Deduped   int64 `json:"deduped"`
	Failed    int64 `json:"failed"`
	RetainDay int   `json:"retain_days"`
}

func (s *Service) collectorStats() CollectorStats {
	s.usageMu.Lock()
	q := s.usageQueue
	running := s.usageRunning
	s.usageMu.Unlock()
	st := CollectorStats{
		Dropped:   s.usageDropped.Load(),
		Deduped:   s.usageDeduped.Load(),
		Failed:    s.usageFailed.Load(),
		RetainDay: s.retainDays,
	}
	if running && q != nil {
		st.QueueLen = len(q)
		st.QueueCap = cap(q)
	}
	return st
}

// pricingFor 读取 (provider, model) 当前单价（缓存读穿透，含负查缓存）。
func (s *Service) pricingFor(provider, model string) *store.MLPricing {
	key := provider + "\x00" + model
	s.pricingMu.RLock()
	p, hit := s.pricingCache[key]
	s.pricingMu.RUnlock()
	if hit {
		return p
	}
	p, err := s.st.GetMLPricing(provider, model)
	if err != nil {
		s.logger.Printf("load pricing %s/%s: %v", provider, model, err)
		p = nil
	}
	s.pricingMu.Lock()
	s.pricingCache[key] = p
	s.pricingMu.Unlock()
	return p
}

// invalidatePricing 写路径更新缓存。
func (s *Service) invalidatePricing(provider, model string, p *store.MLPricing) {
	key := provider + "\x00" + model
	s.pricingMu.Lock()
	s.pricingCache[key] = p
	s.pricingMu.Unlock()
}
