// Per-node local stats: container resource usage + host-level CPU/memory.
package nodeagent

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// ContainerStat is a single container's resource usage snapshot. Exported so
// the gRPC management service can return the same shape without duplicating
// the collection logic.
type ContainerStat struct {
	ContainerID string  `json:"containerId"`
	Service     string  `json:"service,omitempty"`
	TaskID      string  `json:"taskId,omitempty"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemPercent  float64 `json:"memPercent"`
	MemUsage    uint64  `json:"memUsageBytes"`
	MemLimit    uint64  `json:"memLimitBytes"`
}

// StatsResp is the local-node container-stats response, shared by the HTTP
// /api/v1/local/stats handler and the gRPC NodeStats RPC. Host* fields carry
// the HOST-level (宿主机) resource usage read from /proc/stat + /proc/meminfo
// (the worker container shares the host procfs), used by the node card and the
// monitor tab.
type StatsResp struct {
	Node           string          `json:"node"`
	Containers     []ContainerStat `json:"containers"`
	HostCPUPercent float64         `json:"hostCpuPercent,omitempty"`
	HostMemPercent float64         `json:"hostMemPercent,omitempty"`
	HostMemTotal   uint64          `json:"hostMemTotalBytes,omitempty"`
	HostMemUsed    uint64          `json:"hostMemUsedBytes,omitempty"`
	HostCPUCores   int             `json:"hostCpuCores,omitempty"`
	// ContainerCount is the TOTAL number of containers on the node (swarm
	// tasks + standalone docker run containers), from docker ps -a.
	ContainerCount int `json:"containerCount,omitempty"`
}

// LocalStats collects this node's running swarm-service container stats plus
// the host's CPU/memory usage. Shared by the HTTP handler and the gRPC server
// so the collection logic lives once. CPU sampling (host + containers) needs
// two snapshots ~1s apart; the sleep is shared across all samples.
//
// Results are cached for statsCacheTTL: every node card / monitor refresh
// would otherwise pay the full ~1s sampling again. The returned value is a
// defensive copy (callers may hold it while a refresh repopulates the cache).
func (a *API) LocalStats(ctx context.Context) (StatsResp, error) {
	if resp, ok := a.cachedStats(); ok {
		return resp, nil
	}

	host, _ := hostname()
	cs, err := a.cli.ListContainers(ctx, docker.Filter{"label": {"com.docker.swarm.service.id"}})
	if err != nil {
		return StatsResp{}, err
	}

	hostCPU1, _ := readHostCPUStat()

	// First snapshot — fan out concurrently. On container-heavy (k8s) nodes the
	// sequential loop was the ~10-20s bottleneck; each index is written by at
	// most one goroutine so no locking is needed.
	first := make([]docker.Stats, len(cs))
	okFirst := make([]bool, len(cs))
	var wgFirst sync.WaitGroup
	for i, c := range cs {
		if c.State != "running" {
			continue
		}
		wgFirst.Add(1)
		go func(i int, id string) {
			defer wgFirst.Done()
			if s, err := a.cli.ContainerStats(ctx, id); err == nil {
				first[i] = s
				okFirst[i] = true
			}
		}(i, c.ID)
	}
	wgFirst.Wait()

	select {
	case <-ctx.Done():
		return StatsResp{}, ctx.Err()
	case <-time.After(time.Second):
	}

	hostCPU2, _ := readHostCPUStat()
	memTotal, memUsed := hostMem()

	// Second snapshot — fan out concurrently and collect via a buffered channel
	// (append isn't concurrency-safe). A closer goroutine waits for all samplers
	// then closes the channel so the range terminates.
	resp := StatsResp{Node: host, Containers: make([]ContainerStat, 0, len(cs))}
	results := make(chan ContainerStat, len(cs))
	var wgSecond sync.WaitGroup
	for i, c := range cs {
		if !okFirst[i] {
			continue
		}
		wgSecond.Add(1)
		go func(i int, id, svc, task string) {
			defer wgSecond.Done()
			second, err := a.cli.ContainerStats(ctx, id)
			if err != nil {
				return
			}
			results <- ContainerStat{
				ContainerID: id,
				Service:     svc,
				TaskID:      task,
				CPUPercent:  round2(cpuDeltaPercent(first[i], second)),
				MemPercent:  round2(memPercentOf(second)),
				MemUsage:    second.MemoryStats.Usage,
				MemLimit:    second.MemoryStats.Limit,
			}
		}(i, c.ID, c.Labels["com.docker.swarm.service.name"], c.Labels["com.docker.swarm.task.id"])
	}
	go func() { wgSecond.Wait(); close(results) }()
	for st := range results {
		resp.Containers = append(resp.Containers, st)
	}

	// Host-level usage (the node card / monitor tab show these).
	resp.HostCPUPercent = round2(hostCPUPercent(hostCPU1, hostCPU2))
	resp.HostMemTotal = memTotal
	resp.HostMemUsed = memUsed
	if memTotal > 0 {
		resp.HostMemPercent = round2(float64(memUsed) / float64(memTotal) * 100)
	}
	if cores, err := hostCPUCores(); err == nil {
		resp.HostCPUCores = cores
	}
	// Total container count (incl. standalone docker run containers).
	if all, err := a.cli.ListAllContainers(ctx); err == nil {
		resp.ContainerCount = len(all)
	}

	a.putStatsCache(&resp)
	return resp, nil
}

// cachedStats 返回 TTL 内的缓存副本；未命中返回 ok=false。
func (a *API) cachedStats() (StatsResp, bool) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.statsCache == nil || time.Since(a.statsAt) > statsCacheTTL {
		return StatsResp{}, false
	}
	out := *a.statsCache
	out.Containers = append([]ContainerStat(nil), a.statsCache.Containers...)
	return out, true
}

// putStatsCache 写入采样结果（带时间戳）。
func (a *API) putStatsCache(resp *StatsResp) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	a.statsCache = resp
	a.statsAt = time.Now()
}

// hostCPUCores returns the number of host CPU cores (cpu0..cpuN lines in
// /proc/stat, minus the aggregate "cpu " line).
func hostCPUCores() (int, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cores := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu") && !strings.HasPrefix(line, "cpu ") {
			cores++
		}
	}
	return cores, sc.Err()
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	resp, err := a.LocalStats(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// HostStatsResp is the lightweight host-only resource snapshot (no per-
// container breakdown). The streaming node-card path (WatchNodeStats → SSE)
// only needs host CPU/mem + the total container count; the full LocalStats
// per-container double-snapshot is O(N) ContainerStats calls that blocks
// 10-20s on container-heavy (k8s) nodes, so the stream uses this path.
type HostStatsResp struct {
	Node           string  `json:"node"`
	HostCPUPercent float64 `json:"hostCpuPercent,omitempty"`
	HostMemPercent float64 `json:"hostMemPercent,omitempty"`
	HostMemTotal   uint64  `json:"hostMemTotalBytes,omitempty"`
	HostMemUsed    uint64  `json:"hostMemUsedBytes,omitempty"`
	HostCPUCores   int     `json:"hostCpuCores,omitempty"`
	ContainerCount int     `json:"containerCount,omitempty"`
}

// HostStats collects only the host-level CPU/memory + total container count.
// Unlike LocalStats it skips the per-container ContainerStats double-snapshot
// (the slow O(N) part), reading just /proc + a single container-count call, so
// it returns in ~1s regardless of how many containers the node runs. Backs
// GET /api/v1/local/host-stats. Results are cached for statsCacheTTL.
func (a *API) HostStats(ctx context.Context) (HostStatsResp, error) {
	if resp, ok := a.cachedHostStats(); ok {
		return resp, nil
	}
	host, _ := hostname()
	cpu1, _ := readHostCPUStat()
	select {
	case <-ctx.Done():
		return HostStatsResp{}, ctx.Err()
	case <-time.After(time.Second):
	}
	cpu2, _ := readHostCPUStat()
	memTotal, memUsed := hostMem()
	resp := HostStatsResp{Node: host}
	resp.HostCPUPercent = round2(hostCPUPercent(cpu1, cpu2))
	resp.HostMemTotal = memTotal
	resp.HostMemUsed = memUsed
	if memTotal > 0 {
		resp.HostMemPercent = round2(float64(memUsed) / float64(memTotal) * 100)
	}
	if cores, err := hostCPUCores(); err == nil {
		resp.HostCPUCores = cores
	}
	if all, err := a.cli.ListAllContainers(ctx); err == nil {
		resp.ContainerCount = len(all)
	}
	a.putHostStatsCache(&resp)
	return resp, nil
}

func (a *API) cachedHostStats() (HostStatsResp, bool) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if a.hostStatsCache == nil || time.Since(a.hostStatsAt) > statsCacheTTL {
		return HostStatsResp{}, false
	}
	return *a.hostStatsCache, true
}

func (a *API) putHostStatsCache(resp *HostStatsResp) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	a.hostStatsCache = resp
	a.hostStatsAt = time.Now()
}

func (a *API) hostStats(w http.ResponseWriter, r *http.Request) {
	resp, err := a.HostStats(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
