// 写工具审计：落 LevelDB（mcp_audit）+ slog。读工具不落库只打日志
// （口径与 Worker 侧一致：deploy/scale 等变更才审计，check 类不审计）。
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// argSummaryMax 入参摘要上限；config/question 等大字段由 summarizeArgs
// 只记长度，不落正文（防审计库被部署 YAML 撑爆）。
const argSummaryMax = 512

// errorMax 失败原因上限。
const errorMax = 256

// auditWrite 写一条写工具审计。err 为 nil 记成功。
func (h *Handler) auditWrite(ctx context.Context, tool, args string, err error, start time.Time) {
	actor := "unknown"
	if id := identityFrom(ctx); id != nil {
		actor = id.Actor
	}
	rec := &store.MCPAudit{
		TS:     time.Now().UTC(),
		Actor:  actor,
		Tool:   tool,
		Args:   clip(args, argSummaryMax),
		OK:     err == nil,
		CostMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		rec.Error = clip(err.Error(), errorMax)
	}
	if h.deps.Store != nil {
		if serr := h.deps.Store.SaveMCPAudit(rec); serr != nil {
			h.log.Error("mcp audit persist failed", "tool", tool, "err", serr)
		}
	}
	h.log.Info("mcp tool call",
		"actor", actor, "tool", tool, "ok", rec.OK,
		"error", rec.Error, "cost_ms", rec.CostMS)
}

// summarizeArgs 组装入参摘要；大字段（值超过 maxInline）替换为长度标记。
func summarizeArgs(kv map[string]string) string {
	out := ""
	for k, v := range kv {
		if len(v) > maxInlineField {
			v = fmt.Sprintf("<%d bytes>", len(v))
		}
		if out != "" {
			out += ", "
		}
		out += k + "=" + v
	}
	return out
}

// maxInlineField 摘要中允许原样出现的字段值上限。
const maxInlineField = 128

// clip 按字节截断（审计/错误文本用；不追求 rune 安全，截尾即停）。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
