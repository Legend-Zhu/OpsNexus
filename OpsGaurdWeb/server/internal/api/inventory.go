package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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
// 跳过。
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

	// 2. inventory items → 按 type 分发查询
	if clusterRec.Inventory != nil && len(clusterRec.Inventory.Items) > 0 {
		// hostname → node ID 映射（用于 NodeContainers / CheckPort 的 nodeID 参数）
		nodes, _ := cli.ListNodes(ctx)
		nodeMap := make(map[string]string, len(nodes))
		for _, n := range nodes {
			nodeMap[n.Hostname] = n.ID
		}

		for _, item := range clusterRec.Inventory.Items {
			views = append(views, probeInventoryItem(ctx, cli, item, nodeMap))
		}
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
//   - standalone-container: 在 item.Node 上查 NodeContainers，按 ref 匹配容器名
//   - host-service: 解析 ref 为 host:port，从 item.Node（或 manager）探活
func probeInventoryItem(
	ctx context.Context,
	cli *workerproxy.Client,
	item store.InventoryItem,
	nodeMap map[string]string,
) InventoryView {
	v := InventoryView{
		Name:     item.Name,
		Type:     item.Type,
		Category: item.Category,
		Source:   "inventory",
		Node:     item.Node,
		Desc:     item.Desc,
		Status:   "unknown",
	}

	switch item.Type {
	case store.InvStandaloneContainer:
		nodeID, ok := nodeMap[item.Node]
		if !ok {
			v.Status = "node-not-found"
			break
		}
		containers, err := cli.NodeContainers(ctx, nodeID)
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
				v.Ports = ct.Ports
				found = true
				break
			}
		}
		if !found {
			v.Status = "not-found"
		}

	case store.InvHostService:
		host, portStr, pErr := parseHostPort(item.Ref)
		if pErr != nil {
			v.Status = "invalid-ref"
			break
		}
		port, _ := strconv.Atoi(portStr)
		// 探活发起节点：优先 item.Node，未指定时用 manager（空 nodeID）
		nodeID := ""
		if item.Node != "" {
			nodeID = nodeMap[item.Node] // may be "" if node not found
		}
		result, pErr := cli.CheckPort(ctx, nodeID, host, port, "5s")
		if pErr != nil {
			v.Status = "unreachable"
			break
		}
		if result.OK {
			v.Status = "ok"
		} else {
			v.Status = "down"
		}
	}

	return v
}

// parseHostPort 解析 "host:port" 或 "ip:port" 为 host 和 port。
func parseHostPort(ref string) (host, port string, err error) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid host:port %q", ref)
	}
	return ref[:idx], ref[idx+1:], nil
}
