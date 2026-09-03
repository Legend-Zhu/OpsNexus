// 自由提问链路的集群路由（AiNexus 优化方案 A3/A4/A5）：
// 从请求显式字段 / 用户消息文本中解析目标集群，注入平台拓扑 system 前缀
// （集群目录 + 目标集群声明 + 工具守则），并定向连接该集群 Worker MCP；
// 连接失败显性提示，不再静默降级。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// resolveTargetCluster 解析自由提问的目标集群。优先级：显式指定（前端下拉 /
// 请求字段）> 消息文本按 Name 匹配 > 按 Desc 匹配。Name 忽略大小写包含匹配；
// Desc 先整串包含、再 4 字滑窗（覆盖「用户说『自然灾害集群』而描述是
// 『自然灾害应急处置集群』」这类口语简称，两种现网形态均适用）。
//
// 返回 target（唯一命中，nil = 未解析到）与 candidates（命中多个时的候选，
// 供模型向用户澄清；唯一命中时为 nil）。
func resolveTargetCluster(clusters []*store.Cluster, explicit, text string) (target *store.Cluster, candidates []*store.Cluster) {
	if explicit != "" {
		for _, c := range clusters {
			if strings.EqualFold(c.Name, explicit) {
				return c, nil
			}
		}
	}

	byName := matchClusters(clusters, func(c *store.Cluster) bool {
		return c.Name != "" && strings.Contains(strings.ToLower(text), strings.ToLower(c.Name))
	})
	if len(byName) > 0 {
		if len(byName) == 1 {
			return byName[0], nil
		}
		return nil, byName
	}

	byDesc := matchClusters(clusters, func(c *store.Cluster) bool { return descMentioned(c.Desc, text) })
	switch len(byDesc) {
	case 1:
		return byDesc[0], nil
	case 0:
		return nil, nil
	default:
		return nil, byDesc
	}
}

// descMentioned 报告用户文本是否提及了集群描述：整串包含，或描述的任一
// 4 字滑窗片段出现在文本中（描述长度不足 4 字时退化为整串包含）。
// 中文无分词器，滑窗是保守近似：宁可多候选交给模型澄清，不误指集群。
func descMentioned(desc, text string) bool {
	if desc == "" {
		return false
	}
	if strings.Contains(text, desc) {
		return true
	}
	runes := []rune(desc)
	if len(runes) < 4 {
		return false
	}
	for i := 0; i+4 <= len(runes); i++ {
		if strings.Contains(text, string(runes[i:i+4])) {
			return true
		}
	}
	return false
}

// matchClusters 返回满足 cond 的集群（保持注册表顺序）。
func matchClusters(clusters []*store.Cluster, cond func(*store.Cluster) bool) []*store.Cluster {
	var out []*store.Cluster
	for _, c := range clusters {
		if cond(c) {
			out = append(out, c)
		}
	}
	return out
}

// chatUserText 拼接请求 messages 中全部 user 消息文本（集群提及可能出现在
// 任意一轮；忽略非 map/非字符串等非法形态）。
func chatUserText(messages any) string {
	arr, ok := messages.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, m := range arr {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "user" {
			if content, _ := msg["content"].(string); content != "" {
				b.WriteString(content)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// buildChatData 把解析结果组装为 chat_system 场景渲染数据（集群信息经
// Public() 脱敏，token 等敏感字段不得进入模型上下文）。
func buildChatData(clusters []*store.Cluster, target *store.Cluster, candidates []*store.Cluster, connectErr string) mlops.ChatData {
	data := mlops.ChatData{ConnectErr: connectErr}
	for _, c := range clusters {
		p := c.Public()
		data.Clusters = append(data.Clusters, mlops.ChatCluster{Name: p.Name, Desc: p.Desc, Status: string(p.Status)})
	}
	switch {
	case target != nil:
		data.HasTarget = true
		data.Target = target.Name
		data.TargetDesc = target.Desc
	case len(candidates) > 0:
		data.HasCandidates = true
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			names = append(names, c.Name)
		}
		data.Candidates = strings.Join(names, "、")
	}
	return data
}

// chatSystemPrefix 生成自由提问的 system 前缀：mlops chat_system 场景已
// 自定义时走模板渲染（渲染失败留痕回退），否则代码内置组装（二者字节
// 等价，由回归测试守护）。
func (h *Handlers) chatSystemPrefix(data mlops.ChatData) string {
	if h.mlopsSvc != nil {
		if txt, ok := h.mlopsSvc.ChatSystemMessage(data); ok {
			return txt
		}
	}
	return builtinChatSystemPrefix(data)
}

// builtinChatSystemPrefix 代码内置的 chat_system 文案（与 mlops 内置 v1
// 模板 builtinChatSystemTpl 逐字节一致——修改任一侧必须同步）。
func builtinChatSystemPrefix(data mlops.ChatData) string {
	var b strings.Builder
	b.WriteString("你是 OpsGaurd 智能运维平台的 AI 排查助手，运行在管理面，可通过各集群 Worker 的 MCP 工具对纳管集群做实时采证与受控操作。\n\n")

	if len(data.Clusters) > 0 {
		b.WriteString("【纳管集群目录】\n")
		for _, c := range data.Clusters {
			line := "- 名称: " + c.Name
			if c.Desc != "" {
				line += "  描述: " + c.Desc
			}
			line += "  状态: " + c.Status
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	switch {
	case data.HasTarget:
		b.WriteString("【本次目标集群】" + data.Target + "\n")
		if data.TargetDesc != "" {
			b.WriteString("（描述: " + data.TargetDesc + "）\n")
		}
		b.WriteString("用户问题涉及的集群已解析为上述目标集群，所有集群工具调用均只针对该集群执行。\n\n")
		if data.ConnectErr != "" {
			b.WriteString("【注意】目标集群的 MCP 采证通道连接失败（" + data.ConnectErr + "），本次无法实时采证；" +
				"请基于已有信息分析，并在结论开头明确说明此限制。\n\n")
		}
	case data.HasCandidates:
		b.WriteString("用户提到的集群名匹配到多个集群（" + data.Candidates + "），请先向用户确认具体是哪一个，确认前不要调用任何集群工具。\n\n")
	default:
		b.WriteString("未能从用户消息中确定目标集群：如果问题涉及某个集群，请结合【纳管集群目录】先向用户澄清；" +
			"确认目标集群前不要调用集群工具。\n\n")
	}

	b.WriteString(`【工具使用守则】
1. 集群工具按描述中的 [MCP:cluster:<集群名>] 前缀区分归属，只能调用目标集群的工具；
2. 排查服务异常的方法论：先 list_services/get_service 确认服务与副本状态 → get_service_logs 看日志 → HTTP 类问题用 check_http/check_flow 从集群节点网络复现（URL 按 get_service 返回的 Endpoint.Ports 拼接）→ 需要时 exec_in_container 深挖；
3. http_request/command_executor/file_read 是管理面本机工具，访问不到集群业务网络，不要用它们做集群内排查；
4. restart_service/scale_service/update_service/remove_service 是变更类操作，必须先征得用户明确确认才能执行；
5. 结论必须引用工具返回的具体证据支撑；证据不足就明确说不足，不要编造。`)
	return b.String()
}

// sseStreamNotice 在 SSE 流开始前向客户端写一条纯文本提示（OpenAI chunk
// 格式），用于采证通道连接失败等需要显性告知的场景。仅限 stream=true、
// 尚未写响应体时调用；后续网关流式输出接在同一条流上。
func sseStreamNotice(c *gin.Context, text string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	chunk := gin.H{
		"id":      fmt.Sprintf("chatcmpl-notice-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []gin.H{{
			"index": 0,
			"delta": gin.H{"role": "assistant", "content": text},
		}},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(c.Writer, "data: %s\n\n", data)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}
