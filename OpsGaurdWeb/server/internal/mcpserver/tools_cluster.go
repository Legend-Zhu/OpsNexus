// 集群工具（cluster_*）：纳管（add 探活）、注册表 CRUD、聚合事件查询。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

type clusterListIn struct {
	Project string `json:"project,omitempty" description:"Filter by project name (resolves to id)"`
}

type clusterOut struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // online | offline
	Project  string `json:"project,omitempty"`
	Desc     string `json:"desc,omitempty"`
	LastSeen string `json:"last_seen,omitempty"`
	Err      string `json:"err,omitempty"`
}

type clusterListOut struct {
	Items []clusterOut `json:"items"`
	Hint  string       `json:"hint,omitempty"`
}

type clusterNameIn struct {
	Name string `json:"name" description:"Cluster name (from cluster_list)"`
}

type clusterAddIn struct {
	Name string `json:"name" description:"Cluster name (letters/digits/. _ -, 1-63 chars)"`
	// WorkerURL Worker gRPC 管理端点（默认 :9080）。
	WorkerURL string `json:"worker_url" description:"Worker gRPC endpoint, e.g. 10.0.1.5:9080"`
	// WorkerHTTPURL Worker HTTP 端点（默认 :8080），/mcp 与 /healthz 同端口。
	WorkerHTTPURL string `json:"worker_http_url,omitempty" description:"Worker HTTP endpoint, e.g. http://10.0.1.5:8080 — a separate port from gRPC; serves /mcp and /healthz. MCP endpoint defaults to {worker_http_url}/mcp"`
	Token         string `json:"token,omitempty" description:"Worker bearer token if auth enabled on the worker"`
	Project       string `json:"project,omitempty" description:"Project name or id to attach the cluster to"`
	Desc          string `json:"desc,omitempty" description:"Human-readable description"`
}

type clusterUpdateIn struct {
	Name string `json:"name" description:"Cluster name to update"`
	// WorkerURL Worker gRPC 管理端点（默认 :9080）。
	WorkerURL string `json:"worker_url,omitempty" description:"New Worker gRPC endpoint (empty = keep)"`
	// WorkerHTTPURL Worker HTTP 端点（默认 :8080）；变化时自动推导的 mcp_url 跟随更新。
	WorkerHTTPURL string `json:"worker_http_url,omitempty" description:"New Worker HTTP endpoint, e.g. http://10.0.1.5:8080 (empty = keep)"`
	Token         string `json:"token,omitempty" description:"New bearer token (empty = keep existing)"`
	Project       string `json:"project,omitempty" description:"New project name/id (empty string clears membership; use - to keep)"`
	Desc          string `json:"desc,omitempty" description:"New description (empty = keep)"`
}

type clusterRemoveIn struct {
	Name    string `json:"name" description:"Cluster name to remove from management"`
	Confirm bool   `json:"confirm,omitempty" description:"Must be true (stops event subscription, alert rules, MCP link for this cluster)"`
}

type clusterEventsIn struct {
	Name    string `json:"name" description:"Cluster name"`
	Service string `json:"service,omitempty" description:"Filter by service name"`
	Type    string `json:"type,omitempty" description:"Filter by event type (port_down|http_unhealthy|log_match|resource_over|container_down|recovered|patrol_failed)"`
	Limit   int    `json:"limit,omitempty" description:"Max events (default 20, max 100)"`
}

type clusterEventsOut struct {
	Items []store.IngestEvent `json:"items"`
}

func (h *Handler) registerClusterTools(s *mcp.Server) {
	// cluster_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_list",
		Description: "List managed clusters with online/offline status. Every cluster-scoped tool needs a cluster name from here.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterListIn) (*mcp.CallToolResult, clusterListOut, error) {
		items, err := h.deps.Clusters.List(ctx)
		if err != nil {
			return nil, clusterListOut{}, err
		}
		projectID := ""
		if in.Project != "" {
			projectID, err = h.resolveProjectID(in.Project)
			if err != nil {
				return nil, clusterListOut{}, err
			}
		}
		out := make([]clusterOut, 0, len(items))
		for _, c := range items {
			if projectID != "" && c.ProjectID != projectID {
				continue
			}
			out = append(out, clusterOut{
				Name:     c.Name,
				Status:   string(c.Status),
				Project:  c.ProjectID,
				Desc:     c.Desc,
				LastSeen: formatTime(c.LastSeen),
				Err:      c.Err,
			})
		}
		return nil, clusterListOut{Items: out,
				Hint: "use these names as the cluster parameter of service_*/node_*/probe_* tools"},
			nil
	})

	// cluster_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_get",
		Description: "Get one cluster with live health probe result (may be slow: probes the worker).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterNameIn) (*mcp.CallToolResult, clusterOut, error) {
		c, err := h.deps.Clusters.Get(ctx, in.Name)
		if err != nil {
			return nil, clusterOut{}, err
		}
		return nil, clusterOut{
			Name:     c.Name,
			Status:   string(c.Status),
			Project:  c.ProjectID,
			Desc:     c.Desc,
			LastSeen: formatTime(c.LastSeen),
			Err:      c.Err,
		}, nil
	})

	// cluster_add（纳管）
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_add",
		Description: "Onboard a cluster: probes the Worker gRPC endpoint (must be reachable and a swarm manager) and, when worker_http_url is given, its HTTP /healthz (the /mcp port) before saving. The probe error is returned verbatim on failure.",
	}, audited(h, "cluster_add",
		func(in clusterAddIn) map[string]string {
			return map[string]string{"name": in.Name, "worker_url": in.WorkerURL, "worker_http_url": in.WorkerHTTPURL, "project": in.Project}
		},
		func(ctx context.Context, in clusterAddIn) (clusterOut, error) {
			if err := h.checkClusterURLAllowed(in.WorkerURL); err != nil {
				return clusterOut{}, err
			}
			if in.WorkerHTTPURL != "" {
				if err := h.checkClusterURLAllowed(in.WorkerHTTPURL); err != nil {
					return clusterOut{}, err
				}
			}
			projectID := ""
			if in.Project != "" {
				var err error
				if projectID, err = h.resolveProjectID(in.Project); err != nil {
					return clusterOut{}, err
				}
			}
			c, err := h.deps.Clusters.Add(ctx, &store.Cluster{
				Name:          in.Name,
				WorkerURL:     in.WorkerURL,
				WorkerHTTPURL: in.WorkerHTTPURL,
				Token:         in.Token,
				ProjectID:     projectID,
				Desc:          in.Desc,
			})
			if err != nil {
				return clusterOut{}, err
			}
			return clusterOut{Name: c.Name, Status: string(c.Status), Project: c.ProjectID,
				Desc: c.Desc, LastSeen: formatTime(c.LastSeen)}, nil
		}))

	// cluster_update
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_update",
		Description: "Update cluster endpoint/description. Empty fields keep existing values (token included). Pass project=\"\" to detach from its project.",
	}, audited(h, "cluster_update",
		func(in clusterUpdateIn) map[string]string {
			return map[string]string{"name": in.Name, "worker_url": in.WorkerURL, "worker_http_url": in.WorkerHTTPURL}
		},
		func(ctx context.Context, in clusterUpdateIn) (clusterOut, error) {
			if in.WorkerURL != "" {
				if err := h.checkClusterURLAllowed(in.WorkerURL); err != nil {
					return clusterOut{}, err
				}
			}
			if in.WorkerHTTPURL != "" {
				if err := h.checkClusterURLAllowed(in.WorkerHTTPURL); err != nil {
					return clusterOut{}, err
				}
			}
			cur, err := h.deps.Clusters.GetStatic(in.Name)
			if err != nil {
				return clusterOut{}, err
			}
			projectID := cur.ProjectID
			if in.Project != "-" && in.Project != cur.ProjectID {
				// "-" 保留原归属；其余按名字解析（空串=解除归属）
				if in.Project != "" {
					if projectID, err = h.resolveProjectID(in.Project); err != nil {
						return clusterOut{}, err
					}
				} else {
					projectID = ""
				}
			}
			c, err := h.deps.Clusters.Update(ctx, &store.Cluster{
				Name:          in.Name,
				WorkerURL:     in.WorkerURL,
				WorkerHTTPURL: in.WorkerHTTPURL,
				Token:         in.Token,
				ProjectID:     projectID,
				Desc:          in.Desc,
			})
			if err != nil {
				return clusterOut{}, err
			}
			return clusterOut{Name: c.Name, Status: string(c.Status), Project: c.ProjectID,
				Desc: c.Desc, LastSeen: formatTime(c.LastSeen)}, nil
		}))

	// cluster_remove
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_remove",
		Description: "Remove a cluster from management: stops its event subscription, cleans its alert rules, disconnects its MCP link. The cluster itself is untouched. Destructive: requires confirm=true.",
	}, audited(h, "cluster_remove",
		func(in clusterRemoveIn) map[string]string { return map[string]string{"name": in.Name} },
		func(ctx context.Context, in clusterRemoveIn) (clusterOut, error) {
			if !in.Confirm {
				svcCount := -1
				if cli, err := h.deps.Clusters.WorkerClient(in.Name); err == nil {
					if items, err := cli.ListWorkloads(ctx, ""); err == nil {
						svcCount = len(items)
					}
				}
				msg := fmt.Sprintf("removing cluster %q stops its monitoring/alerting and detaches it from the platform", in.Name)
				if svcCount >= 0 {
					msg += fmt.Sprintf(" (it currently runs %d service(s); the services themselves are NOT deleted)", svcCount)
				}
				return clusterOut{}, fmt.Errorf("%s; restate this impact to the user, and set confirm=true once they agree", msg)
			}
			c, err := h.deps.Clusters.GetStatic(in.Name)
			if err != nil {
				return clusterOut{}, err
			}
			if err := h.deps.Clusters.Remove(ctx, in.Name); err != nil {
				return clusterOut{}, err
			}
			return clusterOut{Name: c.Name, Status: "removed"}, nil
		}))

	// cluster_events
	mcp.AddTool(s, &mcp.Tool{
		Name:        "cluster_events",
		Description: "Query aggregated monitor events ingested from the cluster (server-side store, includes recovery events). For the live worker-side raw queue use it via investigation; this is the alerting-grade view.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterEventsIn) (*mcp.CallToolResult, clusterEventsOut, error) {
		cli, err := h.workerClient(in.Name)
		if err != nil {
			return nil, clusterEventsOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		if limit > 100 {
			limit = 100
		}
		raw, err := cli.Events(ctx, in.Service, in.Type, 0, limit)
		if err != nil {
			return nil, clusterEventsOut{}, err
		}
		var out clusterEventsOut
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &out.Items); err != nil {
				return nil, clusterEventsOut{}, fmt.Errorf("decode events: %w", err)
			}
		}
		if out.Items == nil {
			out.Items = []store.IngestEvent{}
		}
		return nil, out, nil
	})
}

// resolveProjectID 项目名或 id → id。名字大小写敏感精确匹配，其次按 id。
func (h *Handler) resolveProjectID(nameOrID string) (string, error) {
	projects, err := h.deps.Clusters.ListProjects()
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.Name == nameOrID || p.ID == nameOrID {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("project %q not found (use project_list)", nameOrID)
}
