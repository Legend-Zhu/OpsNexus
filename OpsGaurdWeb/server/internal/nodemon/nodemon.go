// Package nodemon evaluates cluster-level host (宿主机) resource thresholds
// (store.Cluster.NodeMonitoring). The stats themselves are already collected
// by every node's Worker nodeagent and fanned out by the manager's
// WatchNodeStats stream (host CPU/mem from /proc, 10s cadence) — this loop
// opens that stream once per cycle per configured cluster, evaluates the
// thresholds against the freshest per-node sample, and feeds state
// transitions into the ingest pipeline as events. Alerts therefore aggregate,
// recover and notify (渠道推送) through the exact same path as Worker monitor
// events; no Worker-side config is pushed (unlike AlertRule).
//
// Alert identity: service = "node/<hostname>", type = resource_over — cpu 和
// 内存超限聚合同一条告警（与 Worker rescheck 的 (cluster, service, type)
// 聚合语义一致），恢复事件按 service 整体关闭。
package nodemon

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

const (
	// defaultCycle 调度循环周期。Worker 侧节点采样节奏是 10s，30s 足够追上
	// 翻转，同时把每集群一次的流式采样的开销压到可忽略。
	defaultCycle = 30 * time.Second
	// sampleTimeout 单集群流式采样的读截止。Worker 对每个节点的采样有 8s
	// 硬超时（reachable=false 也照常产出更新），15s 覆盖 init + 全节点样本。
	sampleTimeout = 15 * time.Second
)

// Service 宿主机资源阈值监控调度器。
type Service struct {
	st       *store.Store
	clusters *cluster.Service
	ingest   *ingest.Service
	log      *slog.Logger

	cycle time.Duration

	mu      sync.Mutex
	states  map[string]*nodeState // key: cluster/nodeID/metric (cpu|memory)
	seq     uint64                // 事件 ID 序号（低时钟分辨率平台防同 tick 碰撞）
	stopCh  chan struct{}
	wg      sync.WaitGroup
	running bool
}

// nodeState 单节点单指标的运行状态（翻转检测）。hostname 记录最近一次采样
// 时的节点名，孤儿清理补发恢复事件时构造 service 名用。
type nodeState struct {
	hostname string
	over     bool
}

// nodeSample 一次流式采样里某节点的宿主机快照。
type nodeSample struct {
	hostname   string
	reachable  bool
	cpuPercent float64
	memPercent float64
}

// New 创建调度器；log 为 nil 时用 slog.Default()。
func New(st *store.Store, clusters *cluster.Service, ingestSvc *ingest.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		st: st, clusters: clusters, ingest: ingestSvc, log: log,
		cycle: defaultCycle, states: make(map[string]*nodeState), stopCh: make(chan struct{}),
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

// runCycle 执行一轮：扫全部集群，对 nodeMonitoring.enabled 的集群开
// WatchNodeStats 流采样并评估；最后清理配置已禁用/节点已摘除的遗留状态
// （清除时若处于超限态，补发恢复事件关闭遗留告警，不留幽灵告警）。
func (s *Service) runCycle(ctx context.Context) {
	clusters, err := s.st.ListClusters()
	if err != nil {
		s.log.Error("nodemon: list clusters failed", "err", err)
		return
	}
	var wg sync.WaitGroup
	activeClusters := map[string]bool{}
	for _, c := range clusters {
		if c.NodeMonitoring == nil || !c.NodeMonitoring.Enabled {
			continue
		}
		activeClusters[c.Name] = true
		nm := *c.NodeMonitoring
		name := c.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.sampleCluster(ctx, name, &nm)
		}()
	}
	wg.Wait()

	// 配置已禁用/集群已删除的节点状态：补发恢复并清除。集群级清理无条件
	// 执行；节点级清理仅在该集群本轮成功读到节点清单时执行（流失败不清，
	// 否则 Worker 短暂不可达会把超限告警错误恢复）。
	var orphans [][2]string // (cluster, service)
	s.mu.Lock()
	for k, st := range s.states {
		parts := strings.SplitN(k, "/", 3)
		if len(parts) != 3 {
			delete(s.states, k)
			continue
		}
		if !activeClusters[parts[0]] {
			if st.over {
				orphans = append(orphans, [2]string{parts[0], "node/" + st.hostname})
			}
			delete(s.states, k)
		}
	}
	s.mu.Unlock()
	for _, o := range orphans {
		s.emit(o[0], o[1], store.EventResourceRecover, store.LevelInfo,
			"宿主机阈值配置已移除，自动关闭遗留告警", "")
	}
}

// sampleCluster 打开一轮回调集群的节点统计流，收集每节点最新宿主机样本并
// 评估阈值翻转。worker 不可达只记日志跳过本轮（状态保持，不误报恢复）。
func (s *Service) sampleCluster(ctx context.Context, clusterName string, nm *store.NodeMonitoring) {
	cli, err := s.clusters.WorkerClient(clusterName)
	if err != nil {
		s.log.Warn("nodemon: worker client failed, skip cluster", "cluster", clusterName, "err", err)
		return
	}
	defer cli.Close()

	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	stream, err := cli.WatchNodeStats(sctx)
	if err != nil {
		s.log.Warn("nodemon: watch node stats failed", "cluster", clusterName, "err", err)
		return
	}

	hostnames := map[string]string{} // nodeID -> hostname（init 全量清单）
	samples := map[string]nodeSample{}
	initSeen := false
	for {
		upd, err := stream.Recv()
		if err != nil {
			break // 含 sampleTimeout 到期；样本不足的节点本轮跳过评估
		}
		switch upd.GetKind() {
		case "init":
			initSeen = true
			hostnames = map[string]string{}
			for _, n := range upd.GetNodes() {
				hostnames[n.GetId()] = n.GetHostname()
			}
		case "node":
			id := upd.GetNodeId()
			host := hostnames[id]
			if host == "" {
				host = id
			}
			samples[id] = nodeSample{
				hostname:   host,
				reachable:  upd.GetReachable(),
				cpuPercent: upd.GetCpuPercent(),
				memPercent: upd.GetMemPercent(),
			}
		}
		// init 清单里每个节点都有了样本（含 reachable=false 的占位样本）
		// 即可提前收尾，不必等 sampleTimeout。
		if initSeen && len(hostnames) > 0 && len(samples) == len(hostnames) {
			break
		}
	}
	if !initSeen {
		s.log.Warn("nodemon: no node list received, skip cluster this cycle", "cluster", clusterName)
		return
	}

	s.evaluate(clusterName, nm, hostnames, samples)
}

// evaluate 对每个节点的宿主机指标做阈值判断，仅在翻转时发事件；并清理已
// 从 swarm 摘除的节点状态（本轮 init 清单里没有的 nodeID，若处于超限态补发
// 恢复事件）。判定用 >=，与 Worker rescheck 一致。
func (s *Service) evaluate(clusterName string, nm *store.NodeMonitoring, hostnames map[string]string, samples map[string]nodeSample) {
	for id := range hostnames {
		smp, ok := samples[id]
		if !ok || !smp.reachable {
			// 无样本/节点不可达：保持现状不翻转（RPC 失败不算确定性结果）。
			continue
		}
		for _, m := range []struct {
			metric    string
			threshold int
			value     float64
		}{
			{"cpu", nm.CPUThreshold, smp.cpuPercent},
			{"memory", nm.MemThreshold, smp.memPercent},
		} {
			if m.threshold <= 0 {
				continue
			}
			key := clusterName + "/" + id + "/" + m.metric
			over := m.value >= float64(m.threshold)
			s.mu.Lock()
			st := s.states[key]
			if st == nil {
				st = &nodeState{}
				s.states[key] = st
			}
			st.hostname = smp.hostname
			transition := over != st.over
			st.over = over
			s.mu.Unlock()
			if !transition {
				continue
			}
			service := "node/" + smp.hostname
			detail := fmt.Sprintf("node=%s host cpu=%.1f%% mem=%.1f%%", id, smp.cpuPercent, smp.memPercent)
			if over {
				s.emit(clusterName, service, store.EventResourceOver, store.LevelWarn,
					fmt.Sprintf("宿主机 %s 占用 %.1f%% ≥ 阈值 %d%%", metricLabel(m.metric), m.value, m.threshold), detail)
			} else {
				s.emit(clusterName, service, store.EventResourceRecover, store.LevelInfo,
					fmt.Sprintf("宿主机 %s 占用回落至 %.1f%%（阈值 %d%%）", metricLabel(m.metric), m.value, m.threshold), detail)
			}
		}
	}

	// 节点摘除清理：本集群状态下 nodeID 不在本轮清单里的，视为已移除。
	var removed [][2]string // (service, key)
	prefix := clusterName + "/"
	s.mu.Lock()
	for k, st := range s.states {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		parts := strings.SplitN(k, "/", 3)
		if len(parts) == 3 {
			if _, ok := hostnames[parts[1]]; ok {
				continue
			}
			if st.over {
				removed = append(removed, [2]string{"node/" + st.hostname, k})
			}
		}
		delete(s.states, k)
	}
	s.mu.Unlock()
	for _, r := range removed {
		s.emit(clusterName, r[0], store.EventResourceRecover, store.LevelInfo,
			"节点已从集群移除，自动关闭遗留告警", "")
	}
}

func metricLabel(metric string) string {
	if metric == "memory" {
		return "内存"
	}
	return "CPU"
}

// emit 向 ingest 管线发一条事件。ID 显式携带 key + 自增序号：ingest 按事件
// ID 去重（10 分钟窗），空 ID 走 ev-<纳秒> 在低时钟分辨率平台会同 tick 碰撞。
func (s *Service) emit(clusterName, service string, typ store.EventType, level store.Level, msg, detail string) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	if err := s.ingest.HandleEvent(clusterName, &store.IngestEvent{
		ID:      fmt.Sprintf("nodemon/%s/%s/%d-%d", clusterName, service, time.Now().UnixNano(), seq),
		Service: service,
		Type:    typ,
		Level:   level,
		Msg:     msg,
		Detail:  detail,
	}); err != nil {
		s.log.Error("nodemon: handle event failed", "cluster", clusterName, "service", service, "err", err)
	}
}
