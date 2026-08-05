// Node listing for the management plane: GET /api/v1/nodes and
// GET /api/v1/nodes/{id}/processes. The node list combines the swarm node
// table (docker node ls) with per-node container stats — the local node's
// stats come from this daemon directly, remote nodes via their node-role
// worker's /api/v1/local/stats. Process listing is proxied per node.
package orchestrator

import (
	"context"
	"net/http"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// nodeView 一个集群节点的管理面视图。
type nodeView struct {
	ID           string  `json:"id"`
	Hostname     string  `json:"hostname"`
	Role         string  `json:"role"`            // manager | worker
	State        string  `json:"state"`           // ready | down | ...
	Availability string  `json:"availability"`    // active | pause | drain
	Addr         string  `json:"addr"`
	Leader       bool    `json:"leader"`
	ManagerReach string  `json:"managerReachability,omitempty"` // manager-only
	Reachable    bool    `json:"reachable"`       // node worker 可达（stats 可读）
	CPUCores     float64 `json:"cpuCores"`
	MemBytes     uint64  `json:"memBytes"`
	CPUPercent   float64 `json:"cpuPercent"` // swarm 容器聚合占用（相对节点总核）
	MemPercent   float64 `json:"memPercent"`
	ContainerCount int   `json:"containerCount"`
}

// nodes handles GET /api/v1/nodes.
func (a *API) nodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes, err := a.orch.cli.ListNodes(ctx, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	addrs, err := a.orch.NodeAddrs(ctx)
	if err != nil {
		addrs = map[string]string{}
	}
	selfID, _ := a.orch.SelfNodeID(ctx)

	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		v := nodeView{
			ID:           n.ID,
			Hostname:     n.Description.Hostname,
			Role:         n.Spec.Role,
			State:        n.Status.State,
			Availability: n.Spec.Availability,
			Addr:         n.Status.Addr,
			CPUCores:     coresOf(n),
			MemBytes:     memBytesOf(n),
		}
		if n.ManagerStatus != nil {
			v.Leader = n.ManagerStatus.Leader
			v.ManagerReach = n.ManagerStatus.Reachability
		}
		if n.ID == selfID {
			v.Reachable = true // 本 daemon 直读
			v.CPUPercent, v.MemPercent, v.ContainerCount = a.orch.localNodeAggregate(ctx, v.CPUCores, v.MemBytes)
		} else if addr, ok := addrs[n.ID]; ok {
			stats, err := a.orch.NodeClientByAddr(addr).Stats(ctx)
			if err == nil {
				v.Reachable = true
				v.CPUPercent, v.MemPercent, v.ContainerCount = aggregateStats(stats.Containers, v.CPUCores, v.MemBytes)
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// nodeProcesses handles GET /api/v1/nodes/{id}/processes — proxies to the
// node's local worker (the node-role worker must run /api/v1/local/processes).
func (a *API) nodeProcesses(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	nodes, err := a.orch.cli.ListNodes(ctx, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	for _, n := range nodes {
		if n.ID != id {
			continue
		}
		if n.Status.State != "ready" || n.Status.Addr == "" {
			writeErr(w, http.StatusNotFound, errNotFound("node", id))
			return
		}
		procs, err := a.orch.NodeClientByAddr(n.Status.Addr).Processes(ctx, r.URL.Query().Get("top"), r.URL.Query().Get("limit"))
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, procs)
		return
	}
	writeErr(w, http.StatusNotFound, errNotFound("node", id))
}

// localNodeAggregate reads this daemon's running swarm-service containers and
// aggregates CPU/memory percent against the node's total resources.
func (o *Orchestrator) localNodeAggregate(ctx context.Context, cores float64, memBytes uint64) (cpuPct, memPct float64, count int) {
	cs, err := o.cli.ListContainers(ctx, docker.Filter{"label": {"com.docker.swarm.service.id"}})
	if err != nil {
		return 0, 0, 0
	}
	var sumCPU float64
	var sumMem uint64
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		first, err := o.cli.ContainerStats(ctx, c.ID)
		if err != nil {
			continue
		}
		time.Sleep(time.Second)
		second, err := o.cli.ContainerStats(ctx, c.ID)
		if err != nil {
			continue
		}
		sumCPU += cpuDeltaPercent(first, second)
		sumMem += memUsageOf(second)
		count++
	}
	if cores > 0 {
		cpuPct = round2(sumCPU / cores)
	}
	if memBytes > 0 {
		memPct = round2(float64(sumMem) / float64(memBytes) * 100)
	}
	return cpuPct, memPct, count
}

// aggregateStats sums per-container stats from a node worker's local stats.
func aggregateStats(containers []NodeContainerStat, cores float64, memBytes uint64) (cpuPct, memPct float64, count int) {
	var sumCPU float64
	var sumMem uint64
	for _, c := range containers {
		sumCPU += c.CPUPercent
		// memPercent 已排除 page cache；换算回有效使用字节
		sumMem += uint64(c.MemPercent / 100 * float64(c.MemLimit))
		count++
	}
	if cores > 0 {
		cpuPct = round2(sumCPU / cores)
	}
	if memBytes > 0 {
		memPct = round2(float64(sumMem) / float64(memBytes) * 100)
	}
	return cpuPct, memPct, count
}

func coresOf(n docker.Node) float64 {
	return float64(n.Description.Resources.NanoCPUs) / 1e9
}

func memBytesOf(n docker.Node) uint64 {
	if n.Description.Resources.MemoryBytes < 0 {
		return 0
	}
	return uint64(n.Description.Resources.MemoryBytes)
}

// memUsageOf 容器内存有效使用（排除 page cache）。
func memUsageOf(st docker.Stats) uint64 {
	usage := st.MemoryStats.Usage
	if v, ok := st.MemoryStats.Stats["inactive_file"]; ok && usage > v {
		usage -= v
	}
	return usage
}

// cpuDeltaPercent computes the CPU usage percentage between two stats
// snapshots (docker stats algorithm, normalized by core count).
func cpuDeltaPercent(prev, cur docker.Stats) float64 {
	dt := cur.CPUStats.SystemCPUUsage - prev.CPUStats.SystemCPUUsage
	if dt == 0 {
		return 0
	}
	dtCPU := cur.CPUStats.CPUUsage.TotalUsage - prev.CPUStats.CPUUsage.TotalUsage
	pct := float64(dtCPU) / float64(dt) * 100
	cores := float64(cur.CPUStats.OnlineCPUs)
	if cores > 0 {
		pct *= cores
	}
	return pct
}

// round2 保留两位小数。
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
