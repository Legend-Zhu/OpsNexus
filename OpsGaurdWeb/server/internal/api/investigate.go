// Deep-dive investigation (P4): pick an alert → pull context (cluster
// events, service logs, audit) → assemble a prompt → call the embedded
// AiNexus gateway in-process (SSE straight through the same response), and
// optionally connect the cluster's Worker MCP so the ReAct agent can gather
// live evidence with its 16 tools.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
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
	// 计量入口：本次深度排查的全部底层 LLM 调用归属 scenario=investigate
	c.Request = c.Request.WithContext(usage.NewOperation(c.Request.Context(), usage.ScenarioInvestigate, "/api/v1/ainexus/investigate"))
	var req investigateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// 1. 告警 + 目标集群 Worker 客户端
	alert, cli, ok := h.alertAndClient(c, req.AlertID)
	if !ok {
		return
	}
	maxEvents := req.MaxEvents
	if maxEvents <= 0 {
		maxEvents = 20
	}
	logTail := req.LogTail
	if logTail <= 0 {
		logTail = 50
	}

	// 2. 拉上下文（证据：近期事件 + 审计 + 服务日志；失败不阻塞排查）
	events, audit, logs := gatherEvidence(c.Request.Context(), cli, alert.Service, maxEvents, logTail)

	// 3. 可选：连接该集群 Worker MCP，供 ReAct Agent 采证
	useMCP := req.UseMCP && connectClusterMCP(srv, h.clusters, alert.Cluster)

	// 4. 组装注入上下文后的请求体，进程内 SSE 直通
	model := srv.ResolveModel(req.Model)
	messages := h.investigateMessages(alert, events, audit, logs, useMCP)
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

// alertAndClient 取告警及其集群 Worker 客户端（investigate/chat 共用）。
func (h *Handlers) alertAndClient(c *gin.Context, alertID string) (*store.Alert, *workerproxy.Client, bool) {
	alert, err := h.clusters.Alert(alertID)
	if err != nil {
		fail(c, http.StatusInternalServerError, "get alert: "+err.Error())
		return nil, nil, false
	}
	if alert == nil {
		fail(c, http.StatusNotFound, "alert not found")
		return nil, nil, false
	}
	cli, err := h.clusters.WorkerClient(alert.Cluster)
	if err != nil {
		proxyErr(c, "worker client", err)
		return nil, nil, false
	}
	return alert, cli, true
}

// gatherEvidence 拉取告警上下文证据（近期事件 + 审计 + 服务日志）。
// 每个证据源都有字节上限，防海量证据一次性打爆模型上下文；失败不阻塞。
func gatherEvidence(ctx context.Context, cli *workerproxy.Client, service string, maxEvents, logTail int) (json.RawMessage, json.RawMessage, []workerproxy.LogLine) {
	events, _ := cli.Events(ctx, service, "", 0, maxEvents)
	audit, _ := cli.Audit(ctx, "", 10)
	events = capEvidence(events, evidenceBytes)
	audit = capEvidence(audit, evidenceBytes)
	var logs []workerproxy.LogLine
	logBytes := 0
	_ = cli.StreamLogs(ctx, service, false, logTail, "", func(ll workerproxy.LogLine) bool {
		logBytes += len(ll.Line) + 32
		if logBytes > evidenceBytes {
			logs = append(logs, workerproxy.LogLine{Line: "…[logs truncated]"})
			return false
		}
		logs = append(logs, ll)
		return true
	})
	return events, audit, logs
}

// connectClusterMCP 按需连接集群 Worker MCP（失败降级为 false，不阻断排查）。
func connectClusterMCP(srv *ainexusserver.Server, clusters *cluster.Service, clusterName string) bool {
	url, tok, err := clusters.MCPEndpoint(clusterName)
	if err != nil || url == "" {
		return false
	}
	return srv.AddMCPCluster(clusterName, url, tok) == nil
}

// investigateMessages 组装深度排查消息：MLOps 启用且任一 investigate 场景
// 已自定义时走模板渲染（另一侧用内置 v1 模板，与代码默认字节等价）；
// 未自定义/渲染失败回退代码内置组装（失败在 mlops 侧留痕，不阻断排查）。
func (h *Handlers) investigateMessages(alert *store.Alert, events, audit json.RawMessage, logs []workerproxy.LogLine, useMCP bool) []map[string]any {
	if h.mlopsSvc != nil {
		if msgs, ok := h.mlopsSvc.InvestigateMessages(investigatePromptData(alert, events, audit, logs, useMCP)); ok {
			return msgs
		}
	}
	return BuildInvestigateMessages(alert, events, audit, logs, useMCP)
}

// investigatePromptData 把排查输入组装为模板渲染数据（格式化逻辑与
// BuildInvestigateMessages 一致：prettyJSON、[ts/stream] 行、空段落省略）。
func investigatePromptData(alert *store.Alert, events, audit json.RawMessage, logs []workerproxy.LogLine, useMCP bool) mlops.InvestigateData {
	d := mlops.InvestigateData{
		UseMCP: useMCP,
		Alert: mlops.InvestigateAlert{
			Cluster: alert.Cluster,
			Service: alert.Service,
			Type:    string(alert.Type),
			Level:   string(alert.Level),
			Title:   alert.Title,
			Count:   alert.Count,
			FirstTS: formatRFC3339(alert.FirstTS),
			LastTS:  formatRFC3339(alert.LastTS),
		},
	}
	if len(events) > 0 && string(events) != "null" {
		d.Events = prettyJSON(events)
	}
	if len(audit) > 0 && string(audit) != "null" {
		d.Audit = prettyJSON(audit)
	}
	if len(logs) > 0 {
		var b strings.Builder
		for _, l := range logs {
			fmt.Fprintf(&b, "[%s/%s] %s\n", l.TS, l.Stream, l.Line)
		}
		d.Logs = b.String()
	}
	return d
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
