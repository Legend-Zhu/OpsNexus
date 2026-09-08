// 巡检工具（patrol_*）：自然语言创建/管理巡检任务、触发执行、查看异常
// 结论与 AI 报告。YAML 流程定义与页面同格式——工具描述给出骨架，助手可
// 依据 cluster/service 信息代写。
package mcpserver

import (
	"context"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// patrolYAMLSkeleton 工具描述里的流程骨架（与页面 YAML 同 schema）。
const patrolYAMLSkeleton = `steps are free-form; YAML shape:
vars: {svc: gateway}
checks:
  - name: cpu
    type: resource            # resource|health|port|http|process|flow
    cluster: prod
    service: ${svc}
    metric: cpu
    threshold: 85
  - name: portal
    type: http
    cluster: prod
    url: http://gateway:8080/health
    expect_status: [200]`

type patrolIDIn struct {
	ID string `json:"id" description:"Patrol id (from patrol_list)"`
}

type patrolCreateIn struct {
	Name        string `json:"name" description:"Patrol name (unique)"`
	Description string `json:"desc,omitempty" description:"Purpose"`
	Cron        string `json:"cron,omitempty" description:"5-field cron in Asia/Shanghai (empty = manual run only)"`
	YAML        string `json:"yaml" description:"Flow definition YAML (see the tool description for the skeleton)"`
	Enabled     bool   `json:"enabled,omitempty" description:"Enable the schedule now (default false)"`
}

type patrolUpdateIn struct {
	ID          string `json:"id" description:"Patrol id"`
	Name        string `json:"name,omitempty" description:"New name (empty = keep)"`
	Description string `json:"desc,omitempty" description:"New description (empty = keep)"`
	Cron        string `json:"cron,omitempty" description:"New cron (empty = keep)"`
	YAML        string `json:"yaml,omitempty" description:"New flow YAML (empty = keep)"`
	Enabled     *bool  `json:"enabled,omitempty" description:"Set schedule on/off (omit = keep)"`
}

type patrolDeleteIn struct {
	ID      string `json:"id" description:"Patrol id"`
	Confirm bool   `json:"confirm,omitempty" description:"Must be true (stops its schedule; past runs/reports are kept)"`
}

type patrolOut struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"desc,omitempty"`
	Cron        string `json:"cron,omitempty"`
	Enabled     bool   `json:"enabled"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Hint        string `json:"hint,omitempty"`
}

type patrolListOut struct {
	Items []patrolOut `json:"items"`
}

type patrolRunIn struct {
	ID string `json:"id" description:"Patrol id to run now"`
}

type patrolRunOut struct {
	RunID     string          `json:"run_id"`
	Status    string          `json:"status"`
	Anomalies int             `json:"anomaly_count"`
	Failed    int             `json:"failed_count"`
	Items     []store.Anomaly `json:"items,omitempty"`
	ReportID  string          `json:"report_id,omitempty"`
	Error     string          `json:"error,omitempty"`
	Hint      string          `json:"hint,omitempty"`
}

type patrolRunsIn struct {
	ID    string `json:"id" description:"Patrol id"`
	Limit int    `json:"limit,omitempty" description:"Max runs (default 10)"`
}

type patrolRunsOut struct {
	Items []patrolRunBrief `json:"items"`
}

type patrolRunBrief struct {
	RunID     string `json:"run_id"`
	Status    string `json:"status"`
	StartedAt string `json:"started_at,omitempty"`
	Anomalies int    `json:"anomaly_count,omitempty"`
	ReportID  string `json:"report_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

type patrolReportIn struct {
	RunID string `json:"run_id" description:"Run id (from patrol_run / patrol_runs)"`
}

type patrolReportOut struct {
	ReportID string `json:"report_id,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Model    string `json:"model,omitempty"`
	Created  string `json:"created_at,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

func toPatrolOut(p *store.Patrol) patrolOut {
	return patrolOut{
		ID: p.ID, Name: p.Name, Description: p.Description,
		Cron: p.Cron, Enabled: p.Enabled, UpdatedAt: formatTime(p.UpdatedAt),
	}
}

// requirePatrol 巡检工具前置。
func (h *Handler) requirePatrol() error {
	if h.deps.Patrol == nil {
		return fmt.Errorf("patrol service not initialized")
	}
	return nil
}

func (h *Handler) registerPatrolTools(s *mcp.Server) {
	// patrol_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_list",
		Description: "List inspection tasks (name/cron/enabled). Use patrol_create to add one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, patrolListOut, error) {
		if err := h.requirePatrol(); err != nil {
			return nil, patrolListOut{}, err
		}
		items, err := h.deps.Patrol.List()
		if err != nil {
			return nil, patrolListOut{}, err
		}
		out := make([]patrolOut, 0, len(items))
		for _, p := range items {
			out = append(out, toPatrolOut(p))
		}
		return nil, patrolListOut{Items: out}, nil
	})

	// patrol_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_get",
		Description: "Get one patrol task with its full flow YAML (edit that and call patrol_update).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in patrolIDIn) (*mcp.CallToolResult, patrolOut, error) {
		if err := h.requirePatrol(); err != nil {
			return nil, patrolOut{}, err
		}
		p, err := h.deps.Patrol.Get(in.ID)
		if err != nil {
			return nil, patrolOut{}, err
		}
		out := toPatrolOut(p)
		out.Hint = truncStr(p.YAML)
		return nil, out, nil
	})

	// patrol_create
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_create",
		Description: "Create an inspection task from a flow YAML (" + patrolYAMLSkeleton + "). Validation errors quote the offending field.",
	}, audited(h, "patrol_create",
		func(in patrolCreateIn) map[string]string {
			return map[string]string{"name": in.Name, "cron": in.Cron, "yaml": in.YAML}
		},
		func(ctx context.Context, in patrolCreateIn) (patrolOut, error) {
			if err := h.requirePatrol(); err != nil {
				return patrolOut{}, err
			}
			p, err := h.deps.Patrol.Create(in.Name, in.Description, in.Cron, in.YAML, in.Enabled)
			if err != nil {
				return patrolOut{}, err
			}
			out := toPatrolOut(p)
			out.Hint = "patrol_run{id} to execute once now; runs are visible on the patrol page"
			return out, nil
		}))

	// patrol_update
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_update",
		Description: "Update a patrol task's fields (empty = keep; enabled is a tri-state omit/true/false).",
	}, audited(h, "patrol_update",
		func(in patrolUpdateIn) map[string]string {
			return map[string]string{"id": in.ID, "cron": in.Cron, "yaml": in.YAML}
		},
		func(ctx context.Context, in patrolUpdateIn) (patrolOut, error) {
			if err := h.requirePatrol(); err != nil {
				return patrolOut{}, err
			}
			cur, err := h.deps.Patrol.Get(in.ID)
			if err != nil {
				return patrolOut{}, err
			}
			name, desc, cron, yamlText := in.Name, in.Description, in.Cron, in.YAML
			if name == "" {
				name = cur.Name
			}
			if desc == "" {
				desc = cur.Description
			}
			if cron == "" {
				cron = cur.Cron
			}
			if yamlText == "" {
				yamlText = cur.YAML
			}
			enabled := cur.Enabled
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			p, err := h.deps.Patrol.Update(in.ID, name, desc, cron, yamlText, enabled)
			if err != nil {
				return patrolOut{}, err
			}
			return toPatrolOut(p), nil
		}))

	// patrol_delete
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_delete",
		Description: "Delete a patrol task (stops its schedule; history kept). Destructive: requires confirm=true.",
	}, audited(h, "patrol_delete",
		func(in patrolDeleteIn) map[string]string { return map[string]string{"id": in.ID} },
		func(ctx context.Context, in patrolDeleteIn) (patrolOut, error) {
			if err := h.requirePatrol(); err != nil {
				return patrolOut{}, err
			}
			p, err := h.deps.Patrol.Get(in.ID)
			if err != nil {
				return patrolOut{}, err
			}
			if !in.Confirm {
				return patrolOut{}, fmt.Errorf("deleting patrol %q stops its schedule permanently; "+
					"restate this to the user and set confirm=true to proceed", p.Name)
			}
			if err := h.deps.Patrol.Delete(in.ID); err != nil {
				return patrolOut{}, err
			}
			return toPatrolOut(p), nil
		}))

	// patrol_run
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_run",
		Description: "Trigger a patrol run now (checks execute synchronously; the AI report is generated afterwards). Returns anomalies immediately.",
	}, audited(h, "patrol_run",
		func(in patrolRunIn) map[string]string { return map[string]string{"id": in.ID} },
		func(ctx context.Context, in patrolRunIn) (patrolRunOut, error) {
			if err := h.requirePatrol(); err != nil {
				return patrolRunOut{}, err
			}
			run, err := h.deps.Patrol.Run(ctx, in.ID)
			if err != nil {
				return patrolRunOut{}, err
			}
			return toPatrolRunOut(run), nil
		}))

	// patrol_runs
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_runs",
		Description: "Recent runs of a patrol task (status/anomaly counts, newest first).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in patrolRunsIn) (*mcp.CallToolResult, patrolRunsOut, error) {
		if err := h.requirePatrol(); err != nil {
			return nil, patrolRunsOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 10
		}
		runs, err := h.deps.Patrol.Runs(in.ID, limit)
		if err != nil {
			return nil, patrolRunsOut{}, err
		}
		out := make([]patrolRunBrief, 0, len(runs))
		for _, r := range runs {
			out = append(out, patrolRunBrief{
				RunID: r.ID, Status: string(r.Status), StartedAt: formatTime(r.StartedAt),
				Anomalies: countFailed(r.Anomalies), ReportID: r.ReportID, Error: r.Error,
			})
		}
		return nil, patrolRunsOut{Items: out}, nil
	})

	// patrol_report
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_report",
		Description: "Fetch the AI report of a run (run patrol_report right after patrol_run; it may still be generating — retry shortly).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in patrolReportIn) (*mcp.CallToolResult, patrolReportOut, error) {
		if err := h.requirePatrol(); err != nil {
			return nil, patrolReportOut{}, err
		}
		run, err := h.deps.Patrol.RunByID(in.RunID)
		if err != nil {
			return nil, patrolReportOut{}, err
		}
		if run.ReportID == "" {
			return nil, patrolReportOut{Hint: "report not generated yet — retry in a moment"}, nil
		}
		reports, err := h.deps.Patrol.Reports(run.PatrolID, 50)
		if err != nil {
			return nil, patrolReportOut{}, err
		}
		for _, r := range reports {
			if r.ID == run.ReportID {
				return nil, patrolReportOut{ReportID: r.ID, Summary: truncStr(r.Summary),
					Model: r.Model, Created: formatTime(r.CreatedAt)}, nil
			}
		}
		return nil, patrolReportOut{}, fmt.Errorf("report %q not found", run.ReportID)
	})
}

func toPatrolRunOut(run *store.PatrolRun) patrolRunOut {
	out := patrolRunOut{
		RunID: run.ID, Status: string(run.Status),
		Items: run.Anomalies, ReportID: run.ReportID, Error: run.Error,
	}
	for _, a := range run.Anomalies {
		out.Anomalies++
		if !a.OK {
			out.Failed++
		}
	}
	switch run.Status {
	case "running":
		out.Hint = "checks still running — poll patrol_runs"
	case "success":
		out.Hint = "patrol_report{run_id} for the AI summary"
	case "failed":
		out.Hint = "see error; check cluster connectivity and YAML"
	}
	return out
}

// countFailed 异常计数（OK=false 的检查项）。
func countFailed(anomalies []store.Anomaly) int {
	n := 0
	for _, a := range anomalies {
		if !a.OK {
			n++
		}
	}
	return n
}
