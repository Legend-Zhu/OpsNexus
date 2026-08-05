// Deep-dive investigation (P4): pick an alert → pull context (cluster
// events, service logs, audit) → assemble a prompt → call the embedded
// AiNexus gateway in-process (SSE straight through the same response), and
// optionally connect the cluster's Worker MCP so the ReAct agent can gather
// live evidence with its 16 tools.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// investigateRequest 深度排查请求。
type investigateRequest struct {
	AlertID   string `json:"alert_id" binding:"required"`
	Model     string `json:"model"`      // 强模型名；空则用网关第一个可用模型
	UseMCP    bool   `json:"use_mcp"`    // 是否动态连接集群 Worker MCP 采集证据
	MaxEvents int    `json:"max_events"` // 注入的近期事件数（默认 20）
	LogTail   int    `json:"log_tail"`   // 注入的服务日志行数（默认 50）
}

// AINexusInvestigate godoc: POST /api/v1/ainexus/investigate
// 深度排查：告警 → 拉上下文（事件/日志/审计）→ 进程内调内嵌 AiNexus
// （SSE 流式）。可选 use_mcp 连接该集群 Worker /mcp，让 Agent 用工具采集证据。
func (h *Handlers) AINexusInvestigate(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var req investigateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// 1. 告警
	alert, err := h.clusters.Alert(req.AlertID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "get alert: "+err.Error())
		return
	}
	if alert == nil {
		fail(c, http.StatusNotFound, "alert not found")
		return
	}

	// 2. 目标集群 Worker 客户端
	cli, err := h.clusters.WorkerClient(alert.Cluster)
	if err != nil {
		proxyErr(c, "worker client", err)
		return
	}
	ctx := c.Request.Context()
	maxEvents := req.MaxEvents
	if maxEvents <= 0 {
		maxEvents = 20
	}
	logTail := req.LogTail
	if logTail <= 0 {
		logTail = 50
	}

	// 3. 拉上下文（证据：近期事件 + 审计 + 服务日志；失败不阻塞排查）
	//    每个证据源都有字节上限，防海量证据一次性打爆模型上下文。
	events, _ := cli.Events(ctx, alert.Service, "", maxEvents)
	audit, _ := cli.Audit(ctx, "", 10)
	events = capEvidence(events, evidenceBytes)
	audit = capEvidence(audit, evidenceBytes)
	var logs []workerproxy.LogLine
	logBytes := 0
	_ = cli.StreamLogs(ctx, alert.Service, false, logTail, "", func(ll workerproxy.LogLine) bool {
		logBytes += len(ll.Line) + 32
		if logBytes > evidenceBytes {
			logs = append(logs, workerproxy.LogLine{Line: "…[logs truncated]"})
			return false
		}
		logs = append(logs, ll)
		return true
	})

	// 4. 可选：连接该集群 Worker MCP，供 ReAct Agent 采证
	useMCP := req.UseMCP
	if useMCP {
		if url, tok, merr := h.clusters.MCPEndpoint(alert.Cluster); merr == nil && url != "" {
			if aerr := srv.AddMCPCluster(alert.Cluster, url, tok); aerr != nil {
				// 连接失败不阻断排查，仅降级为纯上下文分析
				useMCP = false
			}
		} else {
			useMCP = false
		}
	}

	// 5. 组装注入上下文后的请求体，进程内 SSE 直通
	model := srv.ResolveModel(req.Model)
	messages := BuildInvestigateMessages(alert, events, audit, logs, useMCP)
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true,
	})
	if err != nil {
		fail(c, http.StatusInternalServerError, "marshal prompt: "+err.Error())
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	srv.OpenAIHandler().ChatCompletions(c)
}

// BuildInvestigateMessages 组装深度排查 prompt（纯函数，可测）。
// 系统消息给出角色与方法论；用户消息携带告警详情与证据链（事件/审计/日志），
// 并在启用 MCP 时提示 Agent 可调用集群 Worker 工具采集更多证据。
func BuildInvestigateMessages(
	alert *store.Alert,
	events json.RawMessage,
	audit json.RawMessage,
	logs []workerproxy.LogLine,
	useMCP bool,
) []map[string]any {
	system := "你是资深运维工程师，擅长 Docker Swarm 集群故障排查。请基于提供的告警与证据链，" +
		"分析根因并给出可执行的处置建议（检查项、命令、预期结果），结论要具体、可操作。"
	if useMCP {
		system += " 你已连接集群 Worker 的 MCP 工具（可查询服务状态、事件、审计、容器资源，执行受控命令采集证据）；" +
			"如证据不足，请调用工具补充，并引用工具返回的结果支撑结论。"
	}

	user := fmt.Sprintf("【告警】\n集群: %s\n服务: %s\n类型: %s\n级别: %s\n标题: %s\n次数: %d\n首次: %s\n最近: %s\n\n",
		alert.Cluster, alert.Service, alert.Type, alert.Level, alert.Title, alert.Count,
		formatRFC3339(alert.FirstTS), formatRFC3339(alert.LastTS))

	if len(events) > 0 && string(events) != "null" {
		user += fmt.Sprintf("【该服务近期监控事件】\n%s\n\n", prettyJSON(events))
	}
	if len(audit) > 0 && string(audit) != "null" {
		user += fmt.Sprintf("【近期操作审计】\n%s\n\n", prettyJSON(audit))
	}
	if len(logs) > 0 {
		user += "【服务日志（最近）】\n"
		for _, l := range logs {
			user += fmt.Sprintf("[%s/%s] %s\n", l.TS, l.Stream, l.Line)
		}
	}
	user += "\n请给出根因分析与处置建议。"

	return []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
}

// evidenceBytes 单个证据源（事件/审计/日志）的字节预算。
// 1M 上下文下证据可完整注入（256KB），仅防多源海量证据挤占分析空间。
const evidenceBytes = 256 << 10 // 256KB

// capEvidence 将证据 JSON 截断到字节上限（超出替换为截断标记）。
func capEvidence(raw json.RawMessage, limit int) json.RawMessage {
	if len(raw) <= limit {
		return raw
	}
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	trimmed := raw[:limit]
	// 尽量在安全边界截断（避免 JSON 解析失败；prompt 里以文本呈现，不依赖解析）
	return append(trimmed, []byte("\n…[evidence truncated]")...)
}

func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
