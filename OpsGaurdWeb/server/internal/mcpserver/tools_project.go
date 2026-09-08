// 项目工具（project_*）：管理层级第一层（项目 → 集群）的 CRUD。
package mcpserver

import (
	"context"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

type projectCreateIn struct {
	Name string `json:"name" description:"Project name (unique)"`
	Desc string `json:"desc,omitempty" description:"Purpose / notes"`
}

type projectUpdateIn struct {
	ID   string `json:"id" description:"Project id (p-xxx, from project_list)"`
	Name string `json:"name,omitempty" description:"New name (empty = keep)"`
	Desc string `json:"desc,omitempty" description:"New description (empty = clear)"`
}

type projectIDIn struct {
	ID      string `json:"id" description:"Project id (p-xxx, from project_list)"`
	Confirm bool   `json:"confirm,omitempty" description:"Must be true for delete (clusters are unlinked, not deleted)"`
}

type projectOut struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Desc         string `json:"desc,omitempty"`
	ClusterCount int    `json:"cluster_count,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

type projectListOut struct {
	Items []projectOut `json:"items"`
}

func toProjectOut(p *store.Project) projectOut {
	return projectOut{
		ID: p.ID, Name: p.Name, Desc: p.Desc,
		ClusterCount: p.ClusterCount,
		CreatedAt:    formatTime(p.CreatedAt),
	}
}

func (h *Handler) registerProjectTools(s *mcp.Server) {
	// project_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "project_list",
		Description: "List projects (top-level grouping above clusters), with member cluster counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, projectListOut, error) {
		items, err := h.deps.Clusters.ListProjects()
		if err != nil {
			return nil, projectListOut{}, err
		}
		out := make([]projectOut, 0, len(items))
		for _, p := range items {
			out = append(out, toProjectOut(p))
		}
		return nil, projectListOut{Items: out}, nil
	})

	// project_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "project_get",
		Description: "Get one project by id.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIDIn) (*mcp.CallToolResult, projectOut, error) {
		p, err := h.deps.Clusters.GetProject(in.ID)
		if err != nil {
			return nil, projectOut{}, err
		}
		return nil, toProjectOut(p), nil
	})

	// project_create
	mcp.AddTool(s, &mcp.Tool{
		Name:        "project_create",
		Description: "Create a project (unique name). Projects group clusters.",
	}, audited(h, "project_create",
		func(in projectCreateIn) map[string]string { return map[string]string{"name": in.Name, "desc": in.Desc} },
		func(ctx context.Context, in projectCreateIn) (projectOut, error) {
			p, err := h.deps.Clusters.CreateProject(in.Name, in.Desc)
			if err != nil {
				return projectOut{}, err
			}
			return toProjectOut(p), nil
		}))

	// project_update
	mcp.AddTool(s, &mcp.Tool{
		Name:        "project_update",
		Description: "Update a project's name/description.",
	}, audited(h, "project_update",
		func(in projectUpdateIn) map[string]string {
			return map[string]string{"id": in.ID, "name": in.Name, "desc": in.Desc}
		},
		func(ctx context.Context, in projectUpdateIn) (projectOut, error) {
			p, err := h.deps.Clusters.UpdateProject(in.ID, in.Name, in.Desc)
			if err != nil {
				return projectOut{}, err
			}
			return toProjectOut(p), nil
		}))

	// project_delete
	mcp.AddTool(s, &mcp.Tool{
		Name:        "project_delete",
		Description: "Delete a project. Member clusters are unlinked (kept, not deleted). Destructive-ish: requires confirm=true.",
	}, audited(h, "project_delete",
		func(in projectIDIn) map[string]string { return map[string]string{"id": in.ID} },
		func(ctx context.Context, in projectIDIn) (projectOut, error) {
			p, err := h.deps.Clusters.GetProject(in.ID)
			if err != nil {
				return projectOut{}, err
			}
			if !in.Confirm {
				return projectOut{}, fmt.Errorf(
					"deleting project %q unlinks its %d member cluster(s) from it (clusters are kept); "+
						"restate this impact to the user, and set confirm=true once they agree", p.Name, p.ClusterCount)
			}
			if err := h.deps.Clusters.DeleteProject(in.ID); err != nil {
				return projectOut{}, err
			}
			return toProjectOut(p), nil
		}))
}
