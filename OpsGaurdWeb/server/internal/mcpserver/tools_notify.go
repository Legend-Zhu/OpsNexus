// 通知工具（notify_*）：渠道查看/维护、策略查看、发送记录。渠道凭据
// （webhook url）与页面同口径回显——config 由渠道类型决定，feishu 需要
// webhook_url。
package mcpserver

import (
	"context"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

type notifyChannelsOut struct {
	Items []*store.NotifyChannel `json:"items"`
}

type notifyChannelSaveIn struct {
	// ID 非空 = 更新；空 = 新建
	ID      string         `json:"id,omitempty" description:"Channel id to update (empty = create)"`
	Type    string         `json:"type,omitempty" description:"feishu | webhook | sms (required for create)"`
	Name    string         `json:"name" description:"Channel display name"`
	Config  map[string]any `json:"config,omitempty" description:"Type-specific: feishu {webhook_url}; webhook {url, headers?}"`
	Enabled *bool          `json:"enabled,omitempty" description:"Enable now (create default true)"`
}

type notifyChannelSaveOut struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Hint    string `json:"hint,omitempty"`
}

type notifyPoliciesOut struct {
	Items []*store.NotifyPolicy `json:"items"`
}

type notifyRecordsIn struct {
	Limit int `json:"limit,omitempty" description:"Max records (default 20)"`
}

type notifyRecordsOut struct {
	Items []*store.NotifyRecord `json:"items"`
}

// requireNotify 前置。
func (h *Handler) requireNotify() error {
	if h.deps.Notify == nil {
		return fmt.Errorf("notify service not initialized")
	}
	return nil
}

func (h *Handler) registerNotifyTools(s *mcp.Server) {
	// notify_channels
	mcp.AddTool(s, &mcp.Tool{
		Name:        "notify_channels",
		Description: "List notification channels (feishu/webhook/sms) with type-specific config.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, notifyChannelsOut, error) {
		if err := h.requireNotify(); err != nil {
			return nil, notifyChannelsOut{}, err
		}
		chs, err := h.deps.Notify.ListChannels()
		if err != nil {
			return nil, notifyChannelsOut{}, err
		}
		if chs == nil {
			chs = []*store.NotifyChannel{}
		}
		return nil, notifyChannelsOut{Items: chs}, nil
	})

	// notify_channel_save
	mcp.AddTool(s, &mcp.Tool{
		Name:        "notify_channel_save",
		Description: "Create (id empty) or update a notification channel. feishu config: {webhook_url}; webhook config: {url, headers?}. Updating with config omitted keeps the existing config.",
	}, audited(h, "notify_channel_save",
		func(in notifyChannelSaveIn) map[string]string {
			return map[string]string{"id": in.ID, "type": in.Type, "name": in.Name}
		},
		func(ctx context.Context, in notifyChannelSaveIn) (notifyChannelSaveOut, error) {
			if err := h.requireNotify(); err != nil {
				return notifyChannelSaveOut{}, err
			}
			if in.Name == "" {
				return notifyChannelSaveOut{}, fmt.Errorf("name is required")
			}
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			var (
				ch  *store.NotifyChannel
				err error
			)
			if in.ID != "" {
				// 更新：config 空则沿用现有（避免半截 config 抹掉凭据）
				cur, gerr := h.deps.Notify.GetChannel(in.ID)
				if gerr != nil {
					return notifyChannelSaveOut{}, gerr
				}
				if cur == nil {
					return notifyChannelSaveOut{}, fmt.Errorf("channel %q not found", in.ID)
				}
				cfg := in.Config
				if len(cfg) == 0 {
					cfg = cur.Config
				}
				ch, err = h.deps.Notify.UpdateChannel(in.ID, in.Name, cfg, cur.ViaProxy, cur.ProxyURL, enabled)
			} else {
				if in.Type == "" {
					return notifyChannelSaveOut{}, fmt.Errorf("type is required for create (feishu | webhook | sms)")
				}
				ch, err = h.deps.Notify.CreateChannel(store.ChannelType(in.Type), in.Name, in.Config, false, "", enabled)
			}
			if err != nil {
				return notifyChannelSaveOut{}, err
			}
			return notifyChannelSaveOut{ID: ch.ID, Type: string(ch.Type), Name: ch.Name, Enabled: ch.Enabled,
				Hint: "alert policies (notify_policies) decide which levels route here"}, nil
		}))

	// notify_policies
	mcp.AddTool(s, &mcp.Tool{
		Name:        "notify_policies",
		Description: "Alert-level routing policies (which channels get info/warn/critical).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, notifyPoliciesOut, error) {
		if err := h.requireNotify(); err != nil {
			return nil, notifyPoliciesOut{}, err
		}
		ps, err := h.deps.Notify.ListPolicies()
		if err != nil {
			return nil, notifyPoliciesOut{}, err
		}
		if ps == nil {
			ps = []*store.NotifyPolicy{}
		}
		return nil, notifyPoliciesOut{Items: ps}, nil
	})

	// notify_records
	mcp.AddTool(s, &mcp.Tool{
		Name:        "notify_records",
		Description: "Recent notification send records (success/failure per channel).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in notifyRecordsIn) (*mcp.CallToolResult, notifyRecordsOut, error) {
		if err := h.requireNotify(); err != nil {
			return nil, notifyRecordsOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		recs, err := h.deps.Notify.Records(limit)
		if err != nil {
			return nil, notifyRecordsOut{}, err
		}
		if recs == nil {
			recs = []*store.NotifyRecord{}
		}
		return nil, notifyRecordsOut{Items: recs}, nil
	})
}
