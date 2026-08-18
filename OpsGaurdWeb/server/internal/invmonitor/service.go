// Package invmonitor executes the monitoring blocks declared on inventory
// items (standalone-container / host-service) from the server side. Swarm
// services are monitored by the Worker's monitor.Manager; inventory items
// never reach the Worker, so this loop probes them via the workerproxy
// one-shot checks (CheckPort / CheckHTTP / NodeContainers) and feeds state
// transitions into the ingest pipeline as events — the same path Worker
// monitor events take, so alert aggregation/recovery is reused as-is.
//
// Resource thresholds and log checks on inventory items are stored and shown
// in the UI but not executed here yet (documented in docs/集群纳管清单方案.md).
package invmonitor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// 默认探测节奏（条目未显式声明 interval 时）。
const (
	defaultCycle         = 30 * time.Second // 调度循环周期
	defaultPortInterval  = 30 * time.Second
	defaultHTTPInterval  = 60 * time.Second
	defaultStateInterval = 30 * time.Second
)

// Service 纳管对象监控调度器。
type Service struct {
	st       *store.Store
	clusters *cluster.Service
	ingest   *ingest.Service
	log      *slog.Logger

	cycle time.Duration

	mu      sync.Mutex
	states  map[string]*checkState // key: cluster/item/checkID
	seq     uint64                 // 事件 ID 序号（低时钟分辨率平台防同 tick 碰撞）
	stopCh  chan struct{}
	wg      sync.WaitGroup
	running bool
}

// checkState 单个检查的运行状态（翻转检测 + interval 节流）。
type checkState struct {
	lastRun time.Time
	failing bool
}

// New 创建调度器；log 为 nil 时用 slog.Default()。
func New(st *store.Store, clusters *cluster.Service, ingestSvc *ingest.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		st: st, clusters: clusters, ingest: ingestSvc, log: log,
		cycle: defaultCycle, states: make(map[string]*checkState), stopCh: make(chan struct{}),
	}
}

// Start 启动调度循环（幂等）。
func (s *Service) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	s.wg.Add(1)
	go s.loop()
}

// Stop 停止调度循环并等待退出。
func (s *Service) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cycle)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.cycle)
			s.runCycle(ctx)
			cancel()
		}
	}
}

// runCycle 执行一轮：扫全部集群的纳管清单，对 monitoring.enabled 的条目
// 执行到期检查，最后清理已从清单移除的检查状态。
func (s *Service) runCycle(ctx context.Context) {
	clusters, err := s.st.ListClusters()
	if err != nil {
		s.log.Error("invmonitor: list clusters failed", "err", err)
		return
	}
	// active 集合按配置生成（与探测是否执行无关）——worker 不可达的集群本轮
	// 跳过探测，但其检查状态必须保留，避免被误判为"已移除"而错误恢复。
	active := map[string]bool{}
	var wg sync.WaitGroup
	for _, c := range clusters {
		if c.Inventory == nil {
			continue
		}
		var items []store.InventoryItem
		for i := range c.Inventory.Items {
			item := c.Inventory.Items[i]
			if item.Monitoring != nil && item.Monitoring.Enabled {
				items = append(items, item)
				for _, id := range checkIDs(&item) {
					active[c.Name+"/"+item.Name+"/"+id] = true
				}
			}
		}
		if len(items) == 0 {
			continue
		}
		cli, err := s.clusters.WorkerClient(c.Name)
		if err != nil {
			s.log.Warn("invmonitor: worker client failed, skip cluster", "cluster", c.Name, "err", err)
			continue
		}
		for i := range items {
			item := items[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.runItem(ctx, c.Name, cli, &item)
			}()
		}
	}
	wg.Wait()

	// 清单中已删除/禁用监控的检查，状态一并清除；清除时若处于失败态，补发
	// 恢复事件关闭其遗留告警（配置变更不应留下永不恢复的幽灵告警）。
	var orphans [][2]string // (cluster, service)
	s.mu.Lock()
	for k, st := range s.states {
		if !active[k] {
			if st.failing {
				parts := strings.SplitN(k, "/", 3)
				if len(parts) == 3 {
					orphans = append(orphans, [2]string{parts[0], parts[1]})
				}
			}
			delete(s.states, k)
		}
	}
	s.mu.Unlock()
	for _, o := range orphans {
		s.mu.Lock()
		s.seq++
		seq := s.seq
		s.mu.Unlock()
		if err := s.ingest.HandleEvent(o[0], &store.IngestEvent{
			ID:      fmt.Sprintf("invmon-orphan/%s/%s/%d-%d", o[0], o[1], time.Now().UnixNano(), seq),
			Service: o[1],
			Type:    store.EventRecovered,
			Level:   store.LevelInfo,
			Msg:     "监控配置变更/检查已移除，自动关闭遗留告警",
		}); err != nil {
			s.log.Error("invmonitor: orphan recover failed", "cluster", o[0], "service", o[1], "err", err)
		}
	}
}

// recordResult 记录一次确定性探测结果，仅在状态翻转时向 ingest 发事件：
// 恢复失败→成功发 store.EventRecovered（关闭该对象活动告警），失败事件
// 类型由检查种类决定（port_down / http_unhealthy / container_down）。
// 返回检查 key（供 active 集合标记）。RPC 执行失败不算确定性结果，不翻转。
func (s *Service) recordResult(clusterName, service string, spec checkSpec, ok bool, msg string, rpcErr error) string {
	key := clusterName + "/" + service + "/" + spec.id
	if rpcErr != nil {
		s.log.Warn("invmonitor: probe rpc failed", "key", key, "err", rpcErr)
		s.touch(key)
		return key
	}

	s.mu.Lock()
	st := s.states[key]
	if st == nil {
		st = &checkState{}
		s.states[key] = st
	}
	st.lastRun = time.Now()
	transition := st.failing == ok // ok 时此前 failing、fail 时此前正常 → 翻转
	st.failing = !ok
	s.mu.Unlock()

	if !transition {
		return key
	}
	level := store.LevelError
	evType := spec.evType
	if ok {
		level = store.LevelInfo
		evType = store.EventRecovered
		msg = "检查恢复正常：" + spec.id
	}
	// ID 显式携带检查标识 + 自增序号：ingest 按事件 ID 去重（10 分钟窗），
	// 空 ID 走 ev-<纳秒> 生成在低时钟分辨率平台会同 tick 碰撞，恢复事件可能
	// 被误去重（Windows 实测 ~0.5ms 分辨率下同 key 两次翻转同 tick）。
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	if err := s.ingest.HandleEvent(clusterName, &store.IngestEvent{
		ID:      fmt.Sprintf("invmon/%s/%d-%d", key, time.Now().UnixNano(), seq),
		Service: service,
		Type:    evType,
		Level:   level,
		Msg:     msg,
	}); err != nil {
		s.log.Error("invmonitor: handle event failed", "key", key, "err", err)
	}
	return key
}

// touch 仅刷新 lastRun（RPC 失败时节流，不改变失败态）。
func (s *Service) touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[key]
	if st == nil {
		st = &checkState{}
		s.states[key] = st
	}
	st.lastRun = time.Now()
}

// due 判断检查是否到执行时间。
func (s *Service) due(key string, interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[key]
	if st == nil {
		return true
	}
	return time.Since(st.lastRun) >= interval
}

// parseInterval 解析检查声明的 interval，非法/为空时回退默认值。
func parseInterval(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
