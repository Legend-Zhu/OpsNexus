package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// resCheck samples the CPU/memory usage of a service's task containers (via
// one-shot stats) and emits EventResourceOver when a configured threshold is
// crossed, and EventResourceRecover when it drops back under.
//
// CPU usage is derived from cumulative counters, so a single snapshot only
// yields the container's lifetime average (≈0 for fresh containers). We keep
// a per-container previous sample and compute the instantaneous rate as a
// delta between consecutive snapshots.
type resCheck struct {
	service string
	cli     docker.Client
	store   *EventStore
	log     *slog.Logger

	thresholds []config.ResourceThreshold
	interval   time.Duration

	// metric -> currently-over flag (for recover events)
	over map[string]bool

	// containerID -> previous CPU sample
	prev map[string]cpuSample
}

// cpuSample is a point-in-time CPU counter snapshot.
type cpuSample struct {
	total  uint64
	system uint64
	at     time.Time
}

func newResCheck(service string, rts []config.ResourceThreshold, cli docker.Client, store *EventStore, log *slog.Logger) (*resCheck, error) {
	interval := 15 * time.Second
	return &resCheck{
		service:    service,
		cli:        cli,
		store:      store,
		log:        log,
		thresholds: rts,
		interval:   interval,
		over:       map[string]bool{},
		prev:       map[string]cpuSample{},
	}, nil
}

func (r *resCheck) run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sample(ctx)
		}
	}
}

func (r *resCheck) sample(ctx context.Context) {
	svc, err := r.cli.GetService(ctx, r.service)
	if err != nil {
		return
	}
	tasks, err := r.cli.ServiceTasks(ctx, svc.ID)
	if err != nil {
		return
	}
	// Aggregate CPU% and mem% across running task containers (mean).
	var cpuSum, memSum float64
	var n int
	for _, t := range tasks {
		if t.Status.State != "running" {
			continue
		}
		cid := t.Status.ContainerStatus.ContainerID
		if cid == "" {
			continue
		}
		st, err := r.cli.ContainerStats(ctx, cid)
		if err != nil {
			r.log.Debug("resource stats failed", "service", r.service, "container", cid, "err", err)
			continue
		}
		cpuSum += r.cpuDeltaPercent(cid, st)
		memSum += memPercent(st)
		n++
	}
	if n == 0 {
		return
	}
	cpu := cpuSum / float64(n)
	mem := memSum / float64(n)
	r.log.Debug("resource sample", "service", r.service, "cpu%", cpu, "mem%", mem, "replicas", n)

	for _, th := range r.thresholds {
		var val float64
		switch th.Metric {
		case "cpu":
			val = cpu
		case "memory":
			val = mem
		default:
			continue
		}
		key := th.Metric
		over := val >= float64(th.Threshold)
		if over && !r.over[key] {
			ev := Event{
				Service: r.service,
				Type:    EventResourceOver,
				Level:   LevelWarn,
				Msg:     fmt.Sprintf("%s usage %.1f%% >= %d%%", th.Metric, val, th.Threshold),
				Detail:  fmt.Sprintf("aggregate across %d replicas", n),
			}
			r.store.Add(ev)
			r.log.Warn("resource threshold crossed", "service", r.service, "metric", th.Metric, "value", val)
			r.over[key] = true
		} else if !over && r.over[key] {
			ev := Event{
				Service: r.service,
				Type:    EventResourceRecover,
				Level:   LevelInfo,
				Msg:     fmt.Sprintf("%s usage back to %.1f%%", th.Metric, val),
			}
			r.store.Add(ev)
			r.over[key] = false
		}
	}
}

// cpuDeltaPercent computes the instantaneous CPU usage percentage between the
// previous and current snapshot for a container. The first observation only
// seeds the baseline and returns 0.
func (r *resCheck) cpuDeltaPercent(cid string, st docker.Stats) float64 {
	now := time.Now()
	cur := cpuSample{total: st.CPUStats.CPUUsage.TotalUsage, system: st.CPUStats.SystemCPUUsage, at: now}
	if prev, ok := r.prev[cid]; ok {
		dt := cur.system - prev.system
		if dt > 0 {
			dtCPU := cur.total - prev.total
			pct := float64(dtCPU) / float64(dt) * 100
			cores := float64(st.CPUStats.OnlineCPUs)
			if cores > 0 {
				pct *= cores
			}
			r.prev[cid] = cur
			return pct
		}
	}
	r.prev[cid] = cur
	return 0
}

// memPercent computes memory usage percentage, excluding page-cache
// (inactive_file) to match `docker stats` semantics on cgroup v2.
func memPercent(st docker.Stats) float64 {
	if st.MemoryStats.Limit == 0 {
		return 0
	}
	usage := st.MemoryStats.Usage
	if v, ok := st.MemoryStats.Stats["inactive_file"]; ok {
		if usage > v {
			usage -= v
		}
	}
	return float64(usage) / float64(st.MemoryStats.Limit) * 100
}
