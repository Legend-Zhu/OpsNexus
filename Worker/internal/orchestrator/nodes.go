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
	"time"

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

// ListNodesView returns the BASE node view (swarm node table only — id/
// hostname/role/state/addr/cpuCores/memBytes). It does NOT fan out per-node
// stats: that was the source of the /nodes endpoint blocking for 15s+ (the
// stats endpoint is O(N) sequential ContainerStats on container-heavy nodes).
// Per-node stats now stream via the WatchNodeStats RPC / SSE so a slow node
// can't block the list. The returned stats fields (CPUPercent/MemPercent/
// ContainerCount/Reachable) are left zero — the caller (UI) fills them via
// the stream.
func (o *Orchestrator) ListNodesView(ctx context.Context) ([]NodeView, error) {
	nodes, err := o.cli.ListNodes(ctx, nil)
	if err != nil {
		return nil, err
	}
	views := make([]NodeView, len(nodes))
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
	}
	return views, nil
}

// NodeViewStats is one node's per-node resource sample (the streaming
// companion to ListNodesView's base view). Returned by StreamNodeStats per
// node as each sample completes.
type NodeViewStats struct {
	NodeID         string
	Reachable      bool
	CPUPercent     float64
	MemPercent     float64
	MemUsage       uint64
	MemTotal       uint64
	ContainerCount int
}

// StreamNodeStats fetches each node's host resource stats concurrently and
// invokes emit with each result as it completes (order non-deterministic).
// A slow/unreachable node is bounded by perNodeTimeout and emits a
// reachable=false result rather than blocking the others. base provides the
// nodes to sample (from ListNodesView) — each entry's Addr identifies the
// node worker to query (the manager proxies via NodeClientByAddr, forwarding
// the configured bearer token).
func (o *Orchestrator) StreamNodeStats(ctx context.Context, base []NodeView, perNodeTimeout time.Duration, emit func(NodeViewStats)) {
	var wg sync.WaitGroup
	for _, v := range base {
		wg.Add(1)
		go func(v NodeView) {
			defer wg.Done()
			if v.Addr == "" {
				emit(NodeViewStats{NodeID: v.ID, Reachable: false})
				return
			}
			nctx, cancel := context.WithTimeout(ctx, perNodeTimeout)
			defer cancel()
			stats, err := o.NodeClientByAddr(v.Addr).Stats(nctx)
			if err != nil {
				emit(NodeViewStats{NodeID: v.ID, Reachable: false})
				return
			}
			emit(NodeViewStats{
				NodeID:         v.ID,
				Reachable:      true,
				CPUPercent:     stats.HostCPUPercent,
				MemPercent:     stats.HostMemPercent,
				MemUsage:       stats.HostMemUsed,
				MemTotal:       stats.HostMemTotal,
				ContainerCount: stats.ContainerCount,
			})
		}(v)
	}
	wg.Wait()
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
