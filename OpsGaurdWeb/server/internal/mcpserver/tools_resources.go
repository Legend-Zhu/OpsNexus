// MCP resources（P3）：只读资源视图，供支持 resources 的助手做上下文
// 预读（tools 仍是主交互面，resources 是点缀——与设计方案 §二非目标一致，
// 只做低成本高价值的四类清单 + 一个集群模板）。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/registry"
)

// textResult 把可 JSON 序列化的值包成 ReadResourceResult（与 Worker MCP
// 同构）。
func textResult(req *mcp.ReadResourceRequest, v any) *mcp.ReadResourceResult {
	data, err := json.Marshal(v)
	if err != nil {
		data = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      req.Params.URI,
		MIMEType: "application/json",
		Text:     string(data),
	}}}
}

func (h *Handler) registerResources(s *mcp.Server) {
	// opsguard://clusters — 集群清单
	s.AddResource(&mcp.Resource{
		URI:         "opsguard://clusters",
		Name:        "Clusters",
		Description: "Managed clusters with online/offline status, project and description",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		items, err := h.deps.Clusters.List(ctx)
		if err != nil {
			return nil, err
		}
		type clusterRow struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Project string `json:"project,omitempty"`
			Desc    string `json:"desc,omitempty"`
		}
		rows := make([]clusterRow, 0, len(items))
		for _, c := range items {
			rows = append(rows, clusterRow{Name: c.Name, Status: string(c.Status),
				Project: c.ProjectID, Desc: c.Desc})
		}
		return textResult(req, rows), nil
	})

	// opsguard://clusters/{name}/services — 集群服务清单（模板）
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "opsguard://clusters/{name}/services",
		Name:        "Cluster services",
		Description: "Services of one cluster with image and replica status",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		const prefix = "opsguard://clusters/"
		const suffix = "/services"
		uri := req.Params.URI
		if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
			return nil, fmt.Errorf("unexpected template URI %q", uri)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(uri, prefix), suffix)
		if name == "" {
			return nil, fmt.Errorf("missing cluster name in URI %q", uri)
		}
		cli, err := h.workerClient(name)
		if err != nil {
			return nil, err
		}
		items, err := cli.ListWorkloads(ctx, "")
		if err != nil {
			return nil, err
		}
		return textResult(req, items), nil
	})

	// opsguard://alerts/active — 活跃告警
	s.AddResource(&mcp.Resource{
		URI:         "opsguard://alerts/active",
		Name:        "Active alerts",
		Description: "Currently active alerts, newest first (entry point of troubleshooting)",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		alerts, err := h.deps.Clusters.Alerts("active", "")
		if err != nil {
			return nil, err
		}
		type alertRow struct {
			ID      string `json:"id"`
			Cluster string `json:"cluster"`
			Service string `json:"service"`
			Type    string `json:"type"`
			Level   string `json:"level"`
			Title   string `json:"title"`
			Count   int    `json:"count"`
			LastTS  string `json:"last_ts,omitempty"`
		}
		rows := make([]alertRow, 0, len(alerts))
		for _, a := range alerts {
			rows = append(rows, alertRow{ID: a.ID, Cluster: a.Cluster, Service: a.Service,
				Type: string(a.Type), Level: string(a.Level), Title: a.Title,
				Count: a.Count, LastTS: formatTime(a.LastTS)})
		}
		return textResult(req, rows), nil
	})

	// opsguard://patrols — 巡检任务
	s.AddResource(&mcp.Resource{
		URI:         "opsguard://patrols",
		Name:        "Patrol tasks",
		Description: "Inspection tasks with cron and enabled state",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if err := h.requirePatrol(); err != nil {
			return nil, err
		}
		items, err := h.deps.Patrol.List()
		if err != nil {
			return nil, err
		}
		type patrolRow struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Cron    string `json:"cron,omitempty"`
			Enabled bool   `json:"enabled"`
		}
		rows := make([]patrolRow, 0, len(items))
		for _, p := range items {
			rows = append(rows, patrolRow{ID: p.ID, Name: p.Name, Cron: p.Cron, Enabled: p.Enabled})
		}
		return textResult(req, rows), nil
	})

	// opsguard://images — 镜像仓库
	s.AddResource(&mcp.Resource{
		URI:         "opsguard://images",
		Name:        "Image registry",
		Description: "Image repos and tags in the embedded registry (deploys reference registry.opsguard/<name>:<tag>)",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		reg, err := h.requireRegistry()
		if err != nil {
			return nil, err
		}
		st := reg.Store()
		type repoRow struct {
			Name string             `json:"name"`
			Tags []registry.TagInfo `json:"tags"`
		}
		rows := make([]repoRow, 0)
		for _, repo := range st.Catalog() {
			tags, err := st.Tags(repo)
			if err != nil {
				continue
			}
			rows = append(rows, repoRow{Name: repo, Tags: tags})
		}
		return textResult(req, rows), nil
	})
}
