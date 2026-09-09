package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	ainexusrt "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexusrt"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/agent"
	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
)

// --- AiNexus 整合（内嵌网关，非独立服务） ---
//
// AiNexus 的 Go 代码 vendor 进 server/internal/ainexus，与管理端同进程运行：
// 无独立端口、无进程间 HTTP。前端聊天页 → /api/v1/ainexus/chat → 进程内
// 直调内嵌网关的 OpenAI handler（SSE 写回同一响应流）；深度排查由内嵌
// ReAct Agent 经 MCP 连接集群 Worker 的 /mcp 采集证据。管理端可在上下文
// 中注入集群诊断信息（事件、日志、审计）供 Agent 排查。

// AINexusChat godoc: POST /api/v1/ainexus/chat
// 进程内调用内嵌 AiNexus 网关的 OpenAI 格式对话接口（SSE 流式）。
// OpsGaurd 扩展字段（可选）：alert_id —— 关联告警时服务端拉取证据
// （事件/审计/日志）组装会话前缀注入，支持多轮追问（每轮前置，证据上下文
// 不丢失）；use_mcp —— 动态连接集群 Worker MCP 供 Agent 采证；cluster ——
// 自由提问显式指定目标集群（前端下拉）。未显式指定时从用户消息按集群
// Name/Desc 解析，并注入平台拓扑 system 前缀（集群目录/目标集群声明/工具
// 守则）；MCP 连接失败显性提示，不再静默降级。
// model 可空（走网关 default_model/模型池兜底）。
func (h *Handlers) AINexusChat(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	// 计量入口：本次请求的全部底层 LLM 调用归属 scenario=chat
	c.Request = c.Request.WithContext(usage.NewOperation(c.Request.Context(), usage.ScenarioChat, "/api/v1/ainexus/chat"))
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		fail(c, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}

	// OpsGaurd 扩展字段（剥离后再透传给网关）
	alertID, _ := req["alert_id"].(string)
	delete(req, "alert_id")
	useMCP, _ := req["use_mcp"].(bool)
	delete(req, "use_mcp")
	clusterArg, _ := req["cluster"].(string)
	delete(req, "cluster")
	stream, _ := req["stream"].(bool)

	// 模型兜底（显式指定优先，否则场景绑定（mlops）→ default_model →
	// 模型池首个启用模型）。显式指定被禁用的模型 → 明确 400。
	model, _ := req["model"].(string)
	if model != "" && srv.IsModelDisabled(model) {
		fail(c, http.StatusBadRequest, "model "+model+" is disabled by mlops operations")
		return
	}
	req["model"] = h.AINexusRT.EffectiveModel(usage.ScenarioChat, model)

	if alertID != "" {
		if h.clusters == nil {
			fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
			return
		}
		alert, cli, ok := h.alertAndClient(c, alertID)
		if !ok {
			return
		}
		events, audit, logs, decl := h.gatherAlertEvidence(c.Request.Context(), cli, alert, 20, 50)
		var mcpErr error
		if useMCP {
			mcpErr = connectClusterMCP(srv, h.clusters, alert.Cluster)
		}
		mcpOK := useMCP && mcpErr == nil
		if useMCP && mcpErr == nil {
			// 会话级集群 scoping：模型只见该集群的 MCP 工具，杜绝跨集群误调用
			c.Request = c.Request.WithContext(agent.WithToolScope(c.Request.Context(), "cluster:"+alert.Cluster))
		}
		seed := h.investigateMessages(alert, events, audit, logs, mcpOK, decl)
		// 证据前缀 + 客户端消息（首轮客户端消息可空 → 仅种子即完整提问）
		var msgs []any
		for _, m := range seed {
			msgs = append(msgs, m)
		}
		if arr, ok := req["messages"].([]any); ok {
			msgs = append(msgs, arr...)
		}
		req["messages"] = msgs
		// 采证通道连接失败 → 流开始前显性提示（提示词模板随之省略"已连接"表述）
		if useMCP && mcpErr != nil && stream {
			sseStreamNotice(c, fmt.Sprintf("【提示】未能连接集群「%s」的采证通道（%v），本次无法实时采证。\n\n", alert.Cluster, mcpErr))
		}
	} else if h.clusters != nil {
		// 自由提问：解析目标集群 → 注入平台上下文 system 前缀 → 定向连接采证
		if clusters, err := h.clusters.ListStatic(); err == nil && len(clusters) > 0 {
			text := chatUserText(req["messages"])
			target, candidates := resolveTargetCluster(clusters, clusterArg, text)
			var mcpErr error
			if target != nil && useMCP {
				mcpErr = connectClusterMCP(srv, h.clusters, target.Name)
			}
			prefix := h.chatSystemPrefix(buildChatData(clusters, target, candidates, errString(mcpErr)))
			msgs := []any{map[string]any{"role": "system", "content": prefix}}
			if arr, ok := req["messages"].([]any); ok {
				msgs = append(msgs, arr...)
			}
			req["messages"] = msgs
			if target != nil && useMCP && mcpErr == nil {
				c.Request = c.Request.WithContext(agent.WithToolScope(c.Request.Context(), "cluster:"+target.Name))
			}
			if target != nil && useMCP && mcpErr != nil && stream {
				sseStreamNotice(c, fmt.Sprintf("【提示】未能连接集群「%s」的采证通道（%v），本次无法实时采证。\n\n", target.Name, mcpErr))
			}
		}
	}

	newBody, err := json.Marshal(req)
	if err != nil {
		fail(c, http.StatusInternalServerError, "marshal request: "+err.Error())
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(newBody))
	c.Request.ContentLength = int64(len(newBody))
	srv.OpenAIHandler().ChatCompletions(c)
}

// errString error 转字符串（nil → ""）。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// AINexusHealth godoc: GET /api/v1/ainexus/health
// 内嵌网关健康（providers/models/tools/mcp_servers）。
func (h *Handlers) AINexusHealth(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	srv.HealthHandler(c)
}

// ListAINexusModels godoc: GET /api/v1/ainexus/models
// 模型列表（名称/provider/类型），供前端模型选择器使用。
func (h *Handlers) ListAINexusModels(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	srv.ModelsHandler(c)
}

// --- 内嵌网关原生端点（挂载在 /ainexus，兼容 AiNexus 自身 URL 契约） ---

// EmbedOpenAIHandler godoc: POST /ainexus/v1/chat/completions
func (h *Handlers) EmbedOpenAIHandler(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	c.Request = c.Request.WithContext(usage.NewOperation(c.Request.Context(), usage.ScenarioNativeChat, "/ainexus/v1/chat/completions"))
	applyNativeModelBinding(c, srv, h.AINexusRT)
	srv.OpenAIHandler().ChatCompletions(c)
}

// EmbedAnthropicHandler godoc: POST /ainexus/v1/messages
func (h *Handlers) EmbedAnthropicHandler(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	c.Request = c.Request.WithContext(usage.NewOperation(c.Request.Context(), usage.ScenarioNativeChat, "/ainexus/v1/messages"))
	applyNativeModelBinding(c, srv, h.AINexusRT)
	srv.AnthropicHandler().Messages(c)
}

// applyNativeModelBinding 原生兼容端点的模型解析：请求未带 model 时按
// native_chat 场景绑定（mlops）注入；显式指定被禁用的模型返回 400。
// 请求体原样透传（仅改写 model 字段），解析失败时不改写（交给网关报错）。
func applyNativeModelBinding(c *gin.Context, srv *ainexusserver.Server, rt *ainexusrt.Service) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(body)) // 原样恢复
		c.Request.ContentLength = int64(len(body))
		return
	}
	model, _ := req["model"].(string)
	if model != "" {
		if srv.IsModelDisabled(model) {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			fail(c, http.StatusBadRequest, "model "+model+" is disabled by mlops operations")
			return
		}
		return // 显式指定，透传
	}
	req["model"] = rt.EffectiveModel(usage.ScenarioNativeChat, "")
	newBody, err := json.Marshal(req)
	if err != nil {
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(newBody))
	c.Request.ContentLength = int64(len(newBody))
}

// EmbedModelsHandler godoc: GET /ainexus/v1/models
func (h *Handlers) EmbedModelsHandler(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	srv.OpenAIHandler().Models(c)
}

// EmbedToolsHandler godoc: GET /ainexus/api/tools
func (h *Handlers) EmbedToolsHandler(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	srv.ToolsHandler(c)
}

// EmbedMCPHandler godoc: GET /ainexus/api/mcp
func (h *Handlers) EmbedMCPHandler(c *gin.Context) {
	srv := h.gateway()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	srv.MCPHandler(c)
}

// gateway 返回当前内嵌网关（热重载后指针会换新，须在单次请求内取用）。
// nil = 网关运行时未初始化或未启用。
func (h *Handlers) gateway() *ainexusserver.Server {
	if h.AINexusRT == nil {
		return nil
	}
	return h.AINexusRT.Server()
}
