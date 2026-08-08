package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// InventoryView 纳管对象统一视图。swarm service 和 inventory item 合并到同
// 一个列表，前端按 category 分组、按 source 区分来源。
type InventoryView struct {
	Name     string `json:"name"`
	Type     string `json:"type"`               // swarm-service | standalone-container | host-service
	Category string `json:"category,omitempty"` // middleware | business | infra | ...
	Source   string `json:"source"`             // swarm | inventory
	Node     string `json:"node,omitempty"`
	Status   string `json:"status"` // running / ok / down / not-found / unreachable / ...
	Image    string `json:"image,omitempty"`
	Ports    string `json:"ports,omitempty"`
	Desc     string `json:"desc,omitempty"`
	// Ref 仅 inventory 条目携带：standalone-container → 容器名；host-service → host:port。
	// 前端"重启容器"等操作需要它（Name 只是展示名）。
	Ref string `json:"ref,omitempty"`
}

// GetInventory GET /api/v1/clusters/:name/inventory
//
// 合并 swarm services（source=swarm）+ inventory items（source=inventory），
// 每个 item 携带实时状态：
//   - swarm-service: running/desired
//   - standalone-container: 容器 state（running/exited/...）
//   - host-service: 端口探活 ok/down
//
// 集群不可达时仍返回 inventory 配置声明（status=unreachable），swarm 部分
// 跳过。items 探测并发执行（每个 item 一次 RPC，串行会随条目数线性变慢）。
func (h *Handlers) GetInventory(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	ctx := c.Request.Context()
	name := c.Param("name")

	// 读集群记录（含 inventory 配置，不探测——避免重复开销）
	clusterRec, err := h.clusters.GetStatic(name)
	if err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "get cluster: "+err.Error())
		return
	}

	cli, err := h.clusters.WorkerClient(name)
	if err != nil {
		proxyErr(c, "worker client", err)
		return
	}

	var views []InventoryView

	// 1. swarm services → type=swarm-service, source=swarm
	workloads, wErr := cli.ListWorkloads(ctx, "")
	if wErr == nil {
		for _, w := range workloads {
			status := w.Replica
			if status == "" {
				status = fmt.Sprintf("%d/%d", w.Running, w.Desired)
			}
			views = append(views, InventoryView{
				Name:     w.Name,
				Type:     "swarm-service",
				Category: w.Labels["category"],
				Source:   "swarm",
				Status:   status,
				Image:    w.Image,
			})
		}
	}

	// 2. inventory items → 按 type 分发查询。节点直接传 hostname（worker 的
	// ResolveNodeAddr 支持按 hostname 解析，无需先拉节点表建映射）。
	if clusterRec.Inventory != nil && len(clusterRec.Inventory.Items) > 0 {
		items := clusterRec.Inventory.Items
		itemViews := make([]InventoryView, len(items))
		var wg sync.WaitGroup
		for i := range items {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				itemViews[i] = probeInventoryItem(ctx, cli, items[i])
			}(i)
		}
		wg.Wait()
		views = append(views, itemViews...)
	}

	ok(c, http.StatusOK, gin.H{"items": views})
}

// UpsertInventory PUT /api/v1/clusters/:name/inventory
//
// 整体替换集群纳管清单（外部对象声明）。校验通过后落库，返回更新后的
// inventory 配置。
func (h *Handlers) UpsertInventory(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var inv store.InventoryConfig
	if err := c.ShouldBindJSON(&inv); err != nil {
		fail(c, http.StatusBadRequest, "invalid inventory: "+err.Error())
		return
	}
	clusterRec, err := h.clusters.UpdateInventory(c.Request.Context(), c.Param("name"), &inv)
	if err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, clusterRec.Public().Inventory)
}

// probeInventoryItem 按 item.Type 分发查询，返回带实时状态的 InventoryView。
//   - standalone-container: 在 item.Node（hostname）上查 NodeContainers，按 ref 匹配容器名；
//     声明了 Ports 时用声明端口展示，未声明取容器实况端口
//   - host-service: ref 为 host（可带默认端口），探活端口 = ref 端口 + 声明 Ports 去重合并，
//     逐个并发探活后聚合状态（全通 ok / 部分通 "n/N" / 全不通 down）；
//     发起节点为 item.Node（hostname）或空串 = manager 本机
func probeInventoryItem(
	ctx context.Context,
	cli *workerproxy.Client,
	item store.InventoryItem,
) InventoryView {
	v := InventoryView{
		Name:     item.Name,
		Type:     item.Type,
		Category: item.Category,
		Source:   "inventory",
		Node:     item.Node,
		Ref:      item.Ref,
		Desc:     item.Desc,
		Status:   "unknown",
	}

	switch item.Type {
	case store.InvStandaloneContainer:
		containers, err := cli.NodeContainers(ctx, item.Node)
		if err != nil {
			v.Status = "unreachable"
			break
		}
		found := false
		for _, ct := range containers {
			// docker ps Names 带 leading "/"，去掉再匹配
			ctName := strings.TrimPrefix(ct.Name, "/")
			if ctName == item.Ref || ct.Name == item.Ref {
				v.Status = ct.State
				v.Image = ct.Image
				if len(item.Ports) > 0 {
					v.Ports = strings.Join(item.Ports, ", ")
				} else {
					v.Ports = ct.Ports
				}
				found = true
				break
			}
		}
		if !found {
			v.Status = "not-found"
		}

	case store.InvHostService:
		host, refPort := parseHostPort(item.Ref)
		if host == "" {
			v.Status = "invalid-ref"
			break
		}
		// 探活端口列表：ref 端口 + 声明 Ports（去重保序）
		var ports []string
		seen := map[string]bool{}
		if refPort != "" && !seen[refPort] {
			ports = append(ports, refPort)
			seen[refPort] = true
		}
		for _, p := range item.Ports {
			if !seen[p] {
				ports = append(ports, p)
				seen[p] = true
			}
		}
		if len(ports) == 0 {
			v.Status = "invalid-ref"
			break
		}
		// 并发探活（每个 2s 超时；串行会 N×2s 线性变慢）
		results := make([]bool, len(ports))
		var wg sync.WaitGroup
		for i, p := range ports {
			wg.Add(1)
			go func(i int, p string) {
				defer wg.Done()
				port, _ := strconv.Atoi(p)
				r, pErr := cli.CheckPort(ctx, item.Node, host, port, "2s")
				results[i] = pErr == nil && r.OK
			}(i, p)
		}
		wg.Wait()
		okCount := 0
		for _, ok := range results {
			if ok {
				okCount++
			}
		}
		v.Ports = strings.Join(ports, ", ")
		switch {
		case okCount == len(ports):
			v.Status = "ok"
		case okCount > 0:
			v.Status = fmt.Sprintf("%d/%d", okCount, len(ports))
		default:
			v.Status = "down"
		}
	}

	return v
}

// parseHostPort 解析 "host" 或 "host:port"（IPv4 域）；无端口时 port 返回 ""。
func parseHostPort(ref string) (host, port string) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 {
		return ref, ""
	}
	// 形如 "host:" 或 ":" 结尾视为纯 host（冒号是分隔符但无端口值）
	if idx == len(ref)-1 {
		return ref[:idx], ""
	}
	return ref[:idx], ref[idx+1:]
}
