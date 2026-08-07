// Node aggregation for the management plane. The node list combines the swarm
// node table (docker node ls) with per-node container stats — the local node's
// stats come from this daemon directly, remote nodes via their node-role
// worker's local API. These functions are the shared logic behind the gRPC
// ListNodes / NodeProcesses / Check* RPCs (the former HTTP handlers were
// removed when the management API moved to gRPC).
package orchestrator

import (
	"context"
	"sync"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// NodeView 一个集群节点的管理面视图（供 HTTP/gRPC 适配层共享）。
type NodeView struct {
	ID             string  `json:"id"`
	Hostname       string  `json:"hostname"`
	Role           string  `json:"role"`         // manager | worker
	State          string  `json:"state"`        // ready | down | ...
	Availability   string  `json:"availability"` // active | pause | drain
	Addr           string  `json:"addr"`
	Leader         bool    `json:"leader"`
	ManagerReach   string  `json:"managerReachability,omitempty"` // manager-only
	Reachable      bool    `json:"reachable"`                     // node worker 可达（stats 可读）
	CPUCores       float64 `json:"cpuCores"`
	MemBytes       uint64  `json:"memBytes"`
	CPUPercent     float64 `json:"cpuPercent"` // swarm 容器聚合占用（相对节点总核）
	MemPercent     float64 `json:"memPercent"`
	ContainerCount int     `json:"containerCount"`
}

// ListNodesView aggregates the swarm node table with per-node HOST resource
// usage (宿主机 CPU/内存，读节点 /proc/stat + /proc/meminfo，而非 swarm 容器聚合).
// The local node's stats come from this daemon directly; remote nodes via their
// node-role worker's /api/v1/local/stats. Failures of individual remote stats
// are non-fatal (the node is marked unreachable).
func (o *Orchestrator) ListNodesView(ctx context.Context) ([]NodeView, error) {
	nodes, err := o.cli.ListNodes(ctx, nil)
	if err != nil {
		return nil, err
	}
	addrs, err := o.NodeAddrs(ctx)
	if err != nil {
		addrs = map[string]string{}
	}
	selfID, _ := o.SelfNodeID(ctx)

	// Per-node stats are fetched concurrently — each node's aggregation already
	// takes ~1s (CPU sampling), so serializing N nodes would make this O(N
	// seconds). The slice is preallocated; each goroutine writes its own slot.
	views := make([]NodeView, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		v := NodeView{
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
		views[i] = v

		wg.Add(1)
		go func(i int, n docker.Node) {
			defer wg.Done()
			// 本机也走统一的 local stats 端点（含宿主采样），保持两路逻辑一致。
			addr, ok := addrs[n.ID]
			if n.ID == selfID {
				addr = n.Status.Addr
				ok = addr != ""
			}
			if !ok {
				return
			}
			stats, err := o.NodeClientByAddr(addr).Stats(ctx)
			if err != nil {
				return
			}
			views[i].Reachable = true
			views[i].CPUPercent = stats.HostCPUPercent
			views[i].MemPercent = stats.HostMemPercent
			views[i].ContainerCount = stats.ContainerCount
		}(i, n)
	}
	wg.Wait()
	return views, nil
}

// ResolveNodeAddr 把 id（node ID 或 hostname）解析为 ready 节点的 worker 地址。
// 空 id 表示"本机"（manager 自身）——host-service 探活等未指定节点时从
// manager 发起。节点不存在/未 ready 返回 errNotFound；docker 失败返回原始错误。
func (o *Orchestrator) ResolveNodeAddr(ctx context.Context, id string) (string, error) {
	nodes, err := o.cli.ListNodes(ctx, nil)
	if err != nil {
		return "", err
	}
	if id == "" {
		selfID, err := o.SelfNodeID(ctx)
		if err != nil {
			return "", err
		}
		for _, n := range nodes {
			if n.ID != selfID {
				continue
			}
			if n.Status.State == "ready" && n.Status.Addr != "" {
				return n.Status.Addr, nil
			}
			break
		}
		return "", errNotFound("node", "(self)")
	}
	for _, n := range nodes {
		if n.ID != id && n.Description.Hostname != id {
			continue
		}
		if n.Status.State == "ready" && n.Status.Addr != "" {
			return n.Status.Addr, nil
		}
		break
	}
	return "", errNotFound("node", id)
}

// NodeNotFound reports whether err is a node-not-found error from ResolveNodeAddr.
func NodeNotFound(err error) bool {
	_, ok := err.(simpleErr)
	return ok
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


// simpleErr is a lightweight sentinel error used by ResolveNodeAddr for
// not-found cases (NodeNotFound distinguishes them from docker failures).
type simpleErr struct{ msg string }

func (e simpleErr) Error() string { return e.msg }

// errNotFound builds a not-found sentinel (what + id).
func errNotFound(what, id string) error { return simpleErr{what + " " + id + " not found"} }
