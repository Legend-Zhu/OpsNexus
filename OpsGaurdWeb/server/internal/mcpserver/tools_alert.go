// 告警工具（alert_*）：活跃告警是排查入口；证据查询在 service_logs/
// node_stats/probe_*，处置最小集（ack/recover）在此。
package mcpserver

import (
	"context"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

type alertListIn struct {
	Status  string `json:"status,omitempty" description:"active | acked | recovered (default active)"`
	Cluster string `json:"cluster,omitempty" description:"Filter by cluster"`
	Level   string `json:"level,omitempty" description:"Filter by level (info|warn|critical)"`
	Limit   int    `json:"limit,omitempty" description:"Max alerts (default 20)"`
}

type alertOut struct {
	ID             string `json:"id"`
	Cluster        string `json:"cluster"`
	Service        string `json:"service"`
	Type           string `json:"type"`
	Level          string `json:"level"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Count          int    `json:"count"`
	FirstTS        string `json:"first_ts,omitempty"`
	LastTS         string `json:"last_ts,omitempty"`
	AckedBy        string `json:"acked_by,omitempty"`
	Acked          bool   `json:"acked,omitempty"`
	Investigations int    `json:"investigations,omitempty"`
}

type alertListOut struct {
	Items []alertOut `json:"items"`
	Hint  string     `json:"hint,omitempty"`
}

type alertIDIn struct {
	ID string `json:"id" description:"Alert id (from alert_list)"`
}

type alertGetOut struct {
	alertOut
	Events  []store.IngestEvent `json:"events,omitempty"`
	HistInv []investigationRef  `json:"investigations,omitempty"`
	Hint    string              `json:"hint,omitempty"`
}

type investigationRef struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Source  string `json:"source,omitempty"`
	Created string `json:"created_at,omitempty"`
}

func toAlertOut(a *store.Alert) alertOut {
	return alertOut{
		ID: a.ID, Cluster: a.Cluster, Service: a.Service,
		Type: string(a.Type), Level: string(a.Level),
		Title: a.Title, Status: string(a.Status),
		Count: a.Count, FirstTS: formatTime(a.FirstTS), LastTS: formatTime(a.LastTS),
		AckedBy: a.AckedBy, Acked: a.AckedAt != nil,
		Investigations: a.Investigations,
	}
}

func (h *Handler) registerAlertTools(s *mcp.Server) {
	// alert_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alert_list",
		Description: "List alerts (default: active). The entry point of troubleshooting: pick one, then service_logs / node_stats / probe_* for evidence, investigation_start for AI deep-dive.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in alertListIn) (*mcp.CallToolResult, alertListOut, error) {
		status := store.AlertActive
		if in.Status != "" {
			status = store.AlertStatus(in.Status)
		}
		alerts, err := h.deps.Clusters.Alerts(status, in.Cluster)
		if err != nil {
			return nil, alertListOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		items := make([]alertOut, 0, len(alerts))
		for _, a := range alerts {
			if in.Level != "" && string(a.Level) != in.Level {
				continue
			}
			if len(items) >= limit {
				break
			}
			items = append(items, toAlertOut(a))
		}
		return nil, alertListOut{Items: items,
			Hint: "alert_get for the event timeline; investigation_start{alert_id} to delegate deep-dive to the embedded AI"}, nil
	})

	// alert_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alert_get",
		Description: "Get one alert with its recent event timeline and past investigation sessions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in alertIDIn) (*mcp.CallToolResult, alertGetOut, error) {
		a, err := h.deps.Clusters.Alert(in.ID)
		if err != nil {
			return nil, alertGetOut{}, err
		}
		if a == nil {
			return nil, alertGetOut{}, fmt.Errorf("alert %q not found", in.ID)
		}
		out := alertGetOut{alertOut: toAlertOut(a)}
		if cli, err := h.workerClient(a.Cluster); err == nil {
			if raw, err := cli.Events(ctx, a.Service, "", 0, 20); err == nil && len(raw) > 0 && string(raw) != "null" {
				_ = jsonUnmarshalInto(raw, &out.Events)
			}
		}
		if invs, err := h.deps.Clusters.Investigations(a.ID, 5); err == nil {
			for _, inv := range invs {
				out.HistInv = append(out.HistInv, investigationRef{
					ID: inv.ID, Title: inv.Title, Created: formatTime(inv.CreatedAt),
				})
			}
		}
		out.Hint = "evidence: service_logs / node_stats / probe_port / probe_http on cluster " + a.Cluster
		return nil, out, nil
	})

	// alert_ack
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alert_ack",
		Description: "Acknowledge an alert (active → acked; you take ownership). Audit actor is the MCP token name.",
	}, audited(h, "alert_ack",
		func(in alertIDIn) map[string]string { return map[string]string{"id": in.ID} },
		func(ctx context.Context, in alertIDIn) (alertOut, error) {
			id := identityFrom(ctx)
			a, err := h.deps.Clusters.AckAlert(in.ID, id.Name)
			if err != nil {
				return alertOut{}, err
			}
			return toAlertOut(a), nil
		}))

	// alert_recover
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alert_recover",
		Description: "Manually close an alert (active/acked → recovered). Use when the underlying issue is fixed or the alert is stale.",
	}, audited(h, "alert_recover",
		func(in alertIDIn) map[string]string { return map[string]string{"id": in.ID} },
		func(ctx context.Context, in alertIDIn) (alertOut, error) {
			a, err := h.deps.Clusters.RecoverAlert(in.ID)
			if err != nil {
				return alertOut{}, err
			}
			return toAlertOut(a), nil
		}))
}
