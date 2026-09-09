// 排查会话工具（investigation_*）：双 agent 协同排障的通道。本机 agent
// （有代码上下文）把「现象 + 代码侧假设 + 请确认什么」经 investigation_start
// 委托给内嵌 AiNexus（有集群证据与领域技能），investigation_get 轮询结论，
// investigation_continue 携带新线索多轮追问。
//
// 执行模型：后台 goroutine 跑网关 ReAct（分钟级），信号量限并发；会话
// 落 store.Investigation（source=mcp 语义由调用记录承载），与页面发起的
// 排查共用同一存储与告警回写闭环。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// questionMax 提问上下文上限（设计方案 §7.3：question ≤ 8KB）。
const questionMax = 8 << 10

// transcriptMax 会话内保留的工具调用轨迹上限。
const transcriptMax = 64 << 10

// messagesMax Messages 字段落库总上限（与页面排查会话同量级约束）。
const messagesMax = 512 << 10

type investigationListIn struct {
	AlertID string `json:"alert_id,omitempty" description:"Filter by alert id (empty = all)"`
	Limit   int    `json:"limit,omitempty" description:"Max sessions (default 20)"`
}

type investigationStartIn struct {
	// AlertID 与 Question 二选一；都给时 alert 模式 + 补充提问。
	AlertID  string `json:"alert_id,omitempty" description:"Investigate this alert (from alert_list); evidence (events/logs) is gathered automatically"`
	Question string `json:"question,omitempty" description:"Free-form question for collaborative debugging. Recommended shape: 现象 + 你的代码侧假设(附关键代码摘要) + 请在集群侧确认什么. Max 8KB."`
	Cluster  string `json:"cluster,omitempty" description:"Cluster to investigate (required for question mode; derived from the alert otherwise)"`
}

type investigationGetIn struct {
	ID string `json:"id" description:"Investigation id (inv-xxx from investigation_start)"`
}

type investigationContinueIn struct {
	ID      string `json:"id" description:"Investigation id (inv-xxx)"`
	Message string `json:"message" description:"Follow-up: new evidence from your code reading, fix description, or another question. Max 8KB."`
}

type investigationOut struct {
	ID        string `json:"id"`
	AlertID   string `json:"alert_id,omitempty"`
	Cluster   string `json:"cluster,omitempty"`
	Title     string `json:"title"`
	Status    string `json:"status"` // running | done | error
	Model     string `json:"model,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	// Conclusion 末轮 assistant 结论（done 时非空）。
	Conclusion string `json:"conclusion,omitempty"`
	// EvidenceTail 工具调用轨迹尾部（done/running 中已产出的部分）。
	EvidenceTail string `json:"evidence_tail,omitempty"`
	Hint         string `json:"hint,omitempty"`
}

type investigationListOut struct {
	Items []investigationRef `json:"items"`
}

func (h *Handler) registerInvestigationTools(s *mcp.Server) {
	// investigation_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "investigation_list",
		Description: "List troubleshooting sessions (web-console and MCP-delegated), newest first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in investigationListIn) (*mcp.CallToolResult, investigationListOut, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		invs, err := h.deps.Clusters.Investigations(in.AlertID, limit)
		if err != nil {
			return nil, investigationListOut{}, err
		}
		items := make([]investigationRef, 0, len(invs))
		for _, inv := range invs {
			items = append(items, investigationRef{
				ID: inv.ID, Title: inv.Title, Created: formatTime(inv.CreatedAt),
			})
		}
		return nil, investigationListOut{Items: items}, nil
	})

	// investigation_start
	mcp.AddTool(s, &mcp.Tool{
		Name:        "investigation_start",
		Description: "Delegate a deep-dive to the embedded AiNexus agent (it has live cluster evidence and domain skills, but no code context — bring your hypothesis in question). Runs in background (minutes): returns an id immediately; poll investigation_get. alert_id mode auto-gathers alert evidence; question mode is for code-side hypotheses.",
	}, audited(h, "investigation_start",
		func(in investigationStartIn) map[string]string {
			return map[string]string{"alert_id": in.AlertID, "cluster": in.Cluster, "question": in.Question}
		},
		func(ctx context.Context, in investigationStartIn) (investigationOut, error) {
			return h.invMgr.start(ctx, in)
		}))

	// investigation_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "investigation_get",
		Description: "Poll a delegated investigation: status running|done|error, the AI conclusion (done), and the tail of its tool-call evidence trail (cite it back into your code analysis).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in investigationGetIn) (*mcp.CallToolResult, investigationOut, error) {
		out, err := h.invMgr.get(in.ID)
		if err != nil {
			return nil, investigationOut{}, err
		}
		return nil, out, nil
	})

	// investigation_continue
	mcp.AddTool(s, &mcp.Tool{
		Name:        "investigation_continue",
		Description: "Continue a finished investigation with a follow-up (new code evidence, a fix to verify, another question). Re-runs the AI agent with the full prior conversation in background; poll investigation_get again.",
	}, audited(h, "investigation_continue",
		func(in investigationContinueIn) map[string]string {
			return map[string]string{"id": in.ID, "message": in.Message}
		},
		func(ctx context.Context, in investigationContinueIn) (investigationOut, error) {
			return h.invMgr.continueSession(ctx, in)
		}))
}

// --- 执行管理 ---

// investigationManager 委托排查的编排（信号量 + 后台执行 + 落库）。
type investigationManager struct {
	h   *Handler
	sem chan struct{}
}

func newInvestigationManager(h *Handler) *investigationManager {
	return &investigationManager{h: h, sem: make(chan struct{}, h.deps.Config.MaxInvestigations)}
}

type chatTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// start 发起委托排查：校验 → 组装 prompt → 立即落库占位（running）→
// 后台执行。返回的记录 status=running。
func (m *investigationManager) start(ctx context.Context, in investigationStartIn) (investigationOut, error) {
	h := m.h
	srv := h.gateway()
	if srv == nil {
		return investigationOut{}, fmt.Errorf("embedded AiNexus gateway is not enabled — configure it under MLOps/模型接入 to use collaborative debugging")
	}

	inv := &store.Investigation{Messages: "[]"}
	var turns []chatTurn
	invType := ""
	system := investigationSystem(invType)

	if in.AlertID != "" {
		alertTurns, cluster, title, decl, err := h.buildAlertTurns(ctx, in.AlertID, in.Question)
		if err != nil {
			return investigationOut{}, err
		}
		inv.AlertID = in.AlertID
		inv.Cluster = cluster
		inv.Title = title
		turns = alertTurns
		if decl != nil {
			invType = decl.Type
			system = investigationSystem(invType)
		}
	} else {
		if strings.TrimSpace(in.Question) == "" {
			return investigationOut{}, fmt.Errorf("either alert_id or question is required")
		}
		if len(in.Question) > questionMax {
			return investigationOut{}, fmt.Errorf("question exceeds %dKB (got %d bytes); keep it to 现象+假设+请确认什么", questionMax>>10, len(in.Question))
		}
		if in.Cluster == "" {
			return investigationOut{}, fmt.Errorf("cluster is required for question mode (which cluster should the AI collect evidence from?)")
		}
		if _, err := h.deps.Clusters.GetStatic(in.Cluster); err != nil {
			return investigationOut{}, fmt.Errorf("cluster %q: %w (use cluster_list)", in.Cluster, err)
		}
		inv.Cluster = in.Cluster
		inv.Title = clip(firstLine(in.Question), 80)
		turns = []chatTurn{{Role: "user", Content: in.Question}}
	}

	// 落库占位（分配 inv-xxx id），随后后台执行
	inv.Model = h.modelName()
	if err := h.deps.Clusters.SaveInvestigation(inv); err != nil {
		return investigationOut{}, err
	}

	if err := m.launch(inv, system, turns); err != nil {
		return investigationOut{}, err
	}
	out := investigationOut{
		ID: inv.ID, AlertID: inv.AlertID, Cluster: inv.Cluster,
		Title: inv.Title, Status: "running", Model: inv.Model,
		CreatedAt: formatTime(inv.CreatedAt), UpdatedAt: formatTime(inv.UpdatedAt),
		Hint: "poll investigation_get (runs for minutes); meanwhile continue your code-side analysis",
	}
	return out, nil
}

// continueSession 多轮追问：校验会话存在且非 running → 追加 user 消息 →
// 后台带着全量历史重跑。
func (m *investigationManager) continueSession(ctx context.Context, in investigationContinueIn) (investigationOut, error) {
	h := m.h
	if h.gateway() == nil {
		return investigationOut{}, fmt.Errorf("embedded AiNexus gateway is not enabled")
	}
	if len(in.Message) > questionMax {
		return investigationOut{}, fmt.Errorf("message exceeds %dKB", questionMax>>10)
	}
	inv, err := h.deps.Clusters.Investigation(in.ID)
	if err != nil {
		return investigationOut{}, err
	}
	if inv == nil {
		return investigationOut{}, fmt.Errorf("investigation %q not found", in.ID)
	}
	if statusOf(inv) == "running" {
		return investigationOut{}, fmt.Errorf("investigation %q is still running; poll investigation_get first", in.ID)
	}

	history, err := parseTurns(inv.Messages)
	if err != nil {
		return investigationOut{}, fmt.Errorf("investigation %q history corrupted: %w", in.ID, err)
	}
	history = append(history, chatTurn{Role: "user", Content: in.Message})

	// 重置为 running（保留历史，结论清空）
	inv.Conclusion = ""
	inv.Messages = mustTurnsJSON(history)
	if err := h.deps.Clusters.SaveInvestigation(inv); err != nil {
		return investigationOut{}, err
	}
	if err := m.launch(inv, investigationSystem(h.sessionInvType(inv.AlertID)), history); err != nil {
		return investigationOut{}, err
	}
	return investigationOut{
		ID: inv.ID, AlertID: inv.AlertID, Cluster: inv.Cluster,
		Title: inv.Title, Status: "running", Model: inv.Model,
		CreatedAt: formatTime(inv.CreatedAt), UpdatedAt: formatTime(inv.UpdatedAt),
		Hint: "poll investigation_get",
	}, nil
}

// get 读取会话与状态。
func (m *investigationManager) get(id string) (investigationOut, error) {
	h := m.h
	inv, err := h.deps.Clusters.Investigation(id)
	if err != nil {
		return investigationOut{}, err
	}
	if inv == nil {
		return investigationOut{}, fmt.Errorf("investigation %q not found", id)
	}
	status := statusOf(inv)
	out := investigationOut{
		ID: inv.ID, AlertID: inv.AlertID, Cluster: inv.Cluster,
		Title: inv.Title, Status: status, Model: inv.Model,
		CreatedAt: formatTime(inv.CreatedAt), UpdatedAt: formatTime(inv.UpdatedAt),
		Conclusion: inv.Conclusion,
	}
	if turns, err := parseTurns(inv.Messages); err == nil {
		out.EvidenceTail = clip(evidenceTail(turns), 12<<10)
	}
	switch status {
	case "running":
		out.Hint = "still running — poll again later"
	case "done":
		out.Hint = "cite the conclusion + evidence in your code analysis; investigation_continue for follow-ups"
	case "error":
		out.Hint = "the run failed (see evidence_tail); fix the gateway/model or retry with investigation_continue"
	}
	return out, nil
}

// launch 信号量 + 后台 goroutine。非阻塞：信号量满时立即报"排队"而非
// 静默堆积（设计方案 §7.3 护栏）。
func (m *investigationManager) launch(inv *store.Investigation, system string, turns []chatTurn) error {
	select {
	case m.sem <- struct{}{}:
	default:
		return fmt.Errorf("investigation queue is full (max %d concurrent); retry shortly", cap(m.sem))
	}
	go func() {
		defer func() { <-m.sem }()
		m.run(inv, system, turns)
	}()
	return nil
}

// run 后台执行体：连接集群 MCP → 跑网关 → 落库结论。
func (m *investigationManager) run(inv *store.Investigation, system string, turns []chatTurn) {
	h := m.h
	ctx := context.Background()

	cluster := inv.Cluster
	if cluster != "" {
		// 目标集群的采证通道（连接失败仅留痕，不阻断——AI 可说明证据受限）
		if url, token, err := h.deps.Clusters.MCPEndpoint(cluster); err == nil {
			if srv := h.gateway(); srv != nil {
				if err := srv.AddMCPCluster(cluster, url, token); err != nil {
					h.log.Warn("investigation: connect cluster mcp failed", "cluster", cluster, "err", err)
				}
			}
		}
	}

	srv := h.gateway()
	aiTurns := make([]ainexusserver.ChatTurn, 0, len(turns))
	for _, t := range turns {
		aiTurns = append(aiTurns, ainexusserver.ChatTurn{Role: t.Role, Content: t.Content})
	}
	answer, transcript, err := srv.RunInvestigation(ctx, inv.Model, system, aiTurns, cluster)
	now := time.Now().UTC()
	if err != nil {
		h.log.Error("investigation run failed", "id", inv.ID, "err", err)
		turns = append(turns, chatTurn{Role: "assistant", Content: "[ERROR] " + clip(err.Error(), errorMax)})
		inv.Messages = clip(mustTurnsJSON(turns), messagesMax)
		inv.UpdatedAt = now
		_ = h.deps.Clusters.SaveInvestigation(inv)
		return
	}
	turns = append(turns, chatTurn{Role: "assistant", Content: answer})
	if transcript != "" {
		turns = append(turns, chatTurn{Role: "tool_transcript", Content: clip(transcript, transcriptMax)})
	}
	inv.Conclusion = clip(answer, transcriptMax)
	inv.Messages = clip(mustTurnsJSON(turns), messagesMax)
	inv.UpdatedAt = now
	if err := h.deps.Clusters.SaveInvestigation(inv); err != nil {
		h.log.Error("investigation save failed", "id", inv.ID, "err", err)
	}
}

// --- prompt 与证据组装 ---

// investigationSystem 委托排查的 system 提示：角色 + 集群工具守则 +
// 协同上下文说明（提问方是带着代码上下文的另一个 agent）。invType 非空时
// 追加纳管对象说明（非 swarm 对象的采证路径指引）。
func investigationSystem(invType string) string {
	s := "你是 OpsGaurd 平台的内嵌 AI 排查专家，运行在管理面，可通过集群 Worker 的 MCP 工具对纳管集群做实时采证（服务状态/日志/资源/事件/拨测）。\n" +
		"本次提问来自另一个具备代码上下文的 AI 助手（或工程师）：它会给出代码侧假设，请你在集群侧独立取证核实或推翻，不要盲从假设。\n" +
		"要求：\n" +
		"1. 结论必须引用工具返回的具体证据（数值、日志行、状态）支撑；证据不足就明确说不足，不要编造；\n" +
		"2. 采证顺序建议：get_service/list_services 确认状态 → get_service_logs 看日志 → get_resource_usage 查资源 → check_http/check_port 主动复现 → get_events 看关联事件；\n" +
		"3. 你只做取证与分析，不要执行任何变更类操作（restart/scale/update/remove）；\n" +
		"4. 结论用简洁的中文，给出：结论（证实/推翻/不确定）→ 证据 → 建议的下一步（可交给提问方在代码侧执行）。\n"
	if invType != "" {
		s += mlops.InvestigateSystemNote(invType)
	}
	return s
}

// sessionInvType 由会话的告警还原纳管对象类型（continue 重跑时 system 重建，
// 声明不落库，按告警重新解析；无告警/非纳管对象返回空）。
func (h *Handler) sessionInvType(alertID string) string {
	if alertID == "" {
		return ""
	}
	a, err := h.deps.Clusters.Alert(alertID)
	if err != nil || a == nil {
		return ""
	}
	if decl := h.inventoryDeclaration(a.Cluster, a.Service); decl != nil {
		return decl.Type
	}
	return ""
}

// inventoryDeclaration 解析告警对象的纳管声明：按名称匹配集群纳管清单中的
// standalone-container / host-service 条目；swarm 服务或读取失败返回 nil。
func (h *Handler) inventoryDeclaration(clusterName, service string) *mlops.InvestigateInventory {
	if service == "" {
		return nil
	}
	c, err := h.deps.Clusters.GetStatic(clusterName)
	if err != nil || c == nil || c.Inventory == nil {
		return nil
	}
	for i := range c.Inventory.Items {
		item := &c.Inventory.Items[i]
		if item.Name != service {
			continue
		}
		inv := &mlops.InvestigateInventory{
			Name:     item.Name,
			Type:     item.Type,
			Ref:      item.Ref,
			Node:     item.Node,
			Ports:    append([]string(nil), item.Ports...),
			Category: item.Category,
			Desc:     item.Desc,
		}
		if item.Monitoring != nil {
			if mj, err := json.MarshalIndent(item.Monitoring, "", "  "); err == nil {
				inv.Monitoring = string(mj)
			}
		}
		return inv
	}
	return nil
}

// buildAlertTurns 告警模式：取告警 + 证据（事件/日志），组装首轮 user 消息。
// 告警对象命中纳管清单时，事件证据改用服务端事件库（invmonitor 直写、不过
// Worker），并注入纳管声明；返回纳管声明供 system 提示词按对象类型组装。
func (h *Handler) buildAlertTurns(ctx context.Context, alertID, extraQuestion string) ([]chatTurn, string, string, *mlops.InvestigateInventory, error) {
	alert, err := h.deps.Clusters.Alert(alertID)
	if err != nil {
		return nil, "", "", nil, err
	}
	if alert == nil {
		return nil, "", "", nil, fmt.Errorf("alert %q not found", alertID)
	}
	decl := h.inventoryDeclaration(alert.Cluster, alert.Service)

	var b strings.Builder
	fmt.Fprintf(&b, "【告警】\n集群: %s\n服务: %s\n类型: %s\n级别: %s\n标题: %s\n次数: %d\n首次: %s\n最近: %s\n\n",
		alert.Cluster, alert.Service, alert.Type, alert.Level, alert.Title, alert.Count,
		formatTime(alert.FirstTS), formatTime(alert.LastTS))

	if decl != nil {
		b.WriteString("【纳管对象声明】\n")
		b.WriteString(mlops.FormatInventoryDeclaration(decl))
		b.WriteString("\n\n")
	}

	switch {
	case decl != nil && h.deps.Store != nil:
		// 纳管对象事件只在服务端（Worker 侧事件队列没有它），改用服务端事件库
		if evs, err := h.deps.Store.ListEvents(alert.Cluster, alert.Service, "", 20); err == nil && len(evs) > 0 {
			if raw, err := json.Marshal(evs); err == nil {
				b.WriteString("【该服务近期监控事件】\n")
				b.Write(raw)
				b.WriteString("\n\n")
			}
		}
	case decl == nil:
		if cli, err := h.workerClient(alert.Cluster); err == nil {
			if raw, err := cli.Events(ctx, alert.Service, "", 0, 20); err == nil && len(raw) > 0 && string(raw) != "null" {
				b.WriteString("【该服务近期监控事件】\n")
				b.Write(raw)
				b.WriteString("\n\n")
			}
			var logs []string
			logBytes := 0
			_ = cli.StreamLogs(ctx, alert.Service, false, 50, "", func(ll workerproxy.LogLine) bool {
				logBytes += len(ll.Line) + 32
				if logBytes > 64<<10 {
					logs = append(logs, "…[logs truncated]")
					return false
				}
				logs = append(logs, fmt.Sprintf("[%s/%s] %s", ll.TS, ll.Stream, ll.Line))
				return true
			})
			if len(logs) > 0 {
				b.WriteString("【服务日志（最近）】\n")
				b.WriteString(joinLines(logs))
				b.WriteString("\n\n")
			}
		} else {
			fmt.Fprintf(&b, "【注意】集群 Worker 连接失败（%v），仅能基于告警信息分析。\n\n", err)
		}
	}

	b.WriteString("请给出根因分析与处置建议（引用证据）。")
	if extraQuestion != "" {
		b.WriteString("\n\n【提问方补充】\n")
		b.WriteString(clip(extraQuestion, questionMax))
	}
	title := alert.Title
	if extraQuestion != "" {
		title = alert.Title + " / " + clip(firstLine(extraQuestion), 40)
	}
	return []chatTurn{{Role: "user", Content: b.String()}}, alert.Cluster, clip(title, 120), decl, nil
}

// --- 辅助 ---

// statusOf 会话状态推导：Conclusion 非空 = done；Messages 占位 [] = running；
// 其余（写入过 assistant [ERROR]）= error。
func statusOf(inv *store.Investigation) string {
	if inv.Conclusion != "" {
		return "done"
	}
	if inv.Messages == "[]" || inv.Messages == "" {
		return "running"
	}
	return "error"
}

// parseTurns 解析会话消息 JSON（tool_transcript 轮保留，供证据尾部提取）。
func parseTurns(raw string) ([]chatTurn, error) {
	if raw == "" {
		return nil, nil
	}
	var turns []chatTurn
	if err := json.Unmarshal([]byte(raw), &turns); err != nil {
		return nil, err
	}
	return turns, nil
}

// mustTurnsJSON 序列化（失败回退空数组占位——绝不让落库失败中断主流程）。
func mustTurnsJSON(turns []chatTurn) string {
	data, err := json.Marshal(turns)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// evidenceTail 取最后的 tool_transcript 轮（无则取最后一条 assistant）。
func evidenceTail(turns []chatTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "tool_transcript" {
			return turns[i].Content
		}
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			return turns[i].Content
		}
	}
	return ""
}

// firstLine 取首行。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
