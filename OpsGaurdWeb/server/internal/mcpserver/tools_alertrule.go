// 告警规则工具（alertrule_*）：monitoring 探测配置的管理（server 侧为
// 源，Apply 合并进服务配置下发 Worker）。输入结构与页面 API 同形。
package mcpserver

import (
	"context"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// --- 输入结构 ---
// 规则体直接复用 store.AlertRule（与页面 API 同一 JSON 形态：
// monitoring:{enabled, portChecks[], httpChecks[], logChecks[], resourceThresholds[]}），
// schema 由 go-sdk 自动生成。

type alertRuleGetIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Service string `json:"service" description:"Service name (or inventory item name)"`
}

type alertRuleListIn struct {
	Cluster string `json:"cluster,omitempty" description:"Filter by cluster (empty = all)"`
}

type alertRuleListOut struct {
	Items []*store.AlertRule `json:"items"`
}

type alertRuleSaveIn struct {
	Rule store.AlertRule `json:"rule" description:"Full rule: {cluster, service, monitoring:{enabled, portChecks[], httpChecks[], logChecks[], resourceThresholds[]}} (same shape as alertrule_get returns)"`
	// Apply 保存后立即下发该集群（默认 true；false 只入库不下发）
	Apply *bool `json:"apply,omitempty" description:"Push to the worker right after save (default true)"`
}

type alertRuleDeleteIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Service string `json:"service" description:"Service name"`
	Confirm bool   `json:"confirm,omitempty" description:"Must be true (stops its monitors on the worker)"`
}

// requireAlertRules 前置。
func (h *Handler) requireAlertRules() error {
	if h.deps.AlertRules == nil {
		return fmt.Errorf("alert rule service not initialized")
	}
	return nil
}

func (h *Handler) registerAlertRuleTools(s *mcp.Server) {
	// alertrule_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alertrule_list",
		Description: "List alert monitoring rules (per cluster/service: port/http/log/resource checks).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in alertRuleListIn) (*mcp.CallToolResult, alertRuleListOut, error) {
		if err := h.requireAlertRules(); err != nil {
			return nil, alertRuleListOut{}, err
		}
		rules, err := h.deps.AlertRules.List(in.Cluster)
		if err != nil {
			return nil, alertRuleListOut{}, err
		}
		if rules == nil {
			rules = []*store.AlertRule{}
		}
		return nil, alertRuleListOut{Items: rules}, nil
	})

	// alertrule_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alertrule_get",
		Description: "Get one rule (edit it and alertrule_save).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in alertRuleGetIn) (*mcp.CallToolResult, store.AlertRule, error) {
		if err := h.requireAlertRules(); err != nil {
			return nil, store.AlertRule{}, err
		}
		r, err := h.deps.AlertRules.Get(in.Cluster, in.Service)
		if err != nil {
			return nil, store.AlertRule{}, err
		}
		return nil, *r, nil
	})

	// alertrule_save
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alertrule_save",
		Description: "Create or update an alert monitoring rule; by default applies it to the cluster right after save (worker-side periodic probes).",
	}, audited(h, "alertrule_save",
		func(in alertRuleSaveIn) map[string]string {
			return map[string]string{"cluster": in.Rule.Cluster, "service": in.Rule.Service}
		},
		func(ctx context.Context, in alertRuleSaveIn) (store.AlertRule, error) {
			if err := h.requireAlertRules(); err != nil {
				return store.AlertRule{}, err
			}
			rule := in.Rule
			if rule.Cluster == "" || rule.Service == "" {
				return store.AlertRule{}, fmt.Errorf("rule.cluster and rule.service are required")
			}
			saved, err := h.deps.AlertRules.Upsert(&rule)
			if err != nil {
				return store.AlertRule{}, err
			}
			if in.Apply == nil || *in.Apply {
				if err := h.deps.AlertRules.Apply(ctx, saved); err != nil {
					return store.AlertRule{}, fmt.Errorf("rule saved but apply failed (retry with alertrule_apply): %w", err)
				}
			}
			return *saved, nil
		}))

	// alertrule_apply
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alertrule_apply",
		Description: "Push a saved rule to its cluster worker (merge into the service's monitoring config). Use after a failed apply or to re-sync.",
	}, audited(h, "alertrule_apply",
		func(in alertRuleGetIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service}
		},
		func(ctx context.Context, in alertRuleGetIn) (store.AlertRule, error) {
			if err := h.requireAlertRules(); err != nil {
				return store.AlertRule{}, err
			}
			r, err := h.deps.AlertRules.Get(in.Cluster, in.Service)
			if err != nil {
				return store.AlertRule{}, err
			}
			if err := h.deps.AlertRules.Apply(ctx, r); err != nil {
				return store.AlertRule{}, err
			}
			return *r, nil
		}))

	// alertrule_delete
	mcp.AddTool(s, &mcp.Tool{
		Name:        "alertrule_delete",
		Description: "Delete a rule and stop its monitors on the worker (cascades; orphan cleanup is automatic). Destructive: requires confirm=true.",
	}, audited(h, "alertrule_delete",
		func(in alertRuleDeleteIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service}
		},
		func(ctx context.Context, in alertRuleDeleteIn) (store.AlertRule, error) {
			if err := h.requireAlertRules(); err != nil {
				return store.AlertRule{}, err
			}
			if !in.Confirm {
				return store.AlertRule{}, fmt.Errorf("deleting the rule for %s/%s stops its periodic monitors (no more alerts from them); "+
					"restate this to the user and set confirm=true to proceed", in.Cluster, in.Service)
			}
			if _, err := h.deps.AlertRules.Delete(ctx, in.Cluster, in.Service, false); err != nil {
				return store.AlertRule{}, err
			}
			return store.AlertRule{Cluster: in.Cluster, Service: in.Service}, nil
		}))
}
