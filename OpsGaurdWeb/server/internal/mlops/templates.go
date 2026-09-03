// Package mlops implements the MLOps operational layer around the embedded
// AiNexus gateway. P1 scope: Prompt Hub — scenario-keyed structured message
// templates with versioning, preview, activation/rollback and builtin
// defaults that are byte-identical to the previous hardcoded prompts.
package mlops

// 场景 key（提示词场景与计量 scenario 是两个命名空间：前者定位模板，
// 后者归属费用口径）。
const (
	ScenarioInvestigateSystem = "investigate_system" // 深度排查 system 消息
	ScenarioInvestigateUser   = "investigate_user"   // 深度排查 user 消息（告警+证据）
	ScenarioCompressSystem    = "compress_system"    // Agent 会话压缩 system 提示词
	ScenarioPatrolSystem      = "patrol_system"      // 巡检 AI 报告 system 提示词
	ScenarioCustom            = "custom"             // 用户自定义（不接入线上入口）
)

// InvestigateAlert investigate_user 模板的告警字段（均已格式化为字符串，
// 与 BuildInvestigateMessages 的输出一致）。
type InvestigateAlert struct {
	Cluster string `json:"cluster"`
	Service string `json:"service"`
	Type    string `json:"type"`
	Level   string `json:"level"`
	Title   string `json:"title"`
	Count   int    `json:"count"`
	FirstTS string `json:"first_ts"`
	LastTS  string `json:"last_ts"`
}

// InvestigateData investigate 场景渲染数据。Events/Audit/Logs 为预格式化
// 文本，空 = 对应段落省略（与内置行为逐字节一致）。
type InvestigateData struct {
	Alert  InvestigateAlert `json:"alert"`
	Events string           `json:"events"`
	Audit  string           `json:"audit"`
	Logs   string           `json:"logs"`
	UseMCP bool             `json:"use_mcp"`
}

// CompressData compress_system 场景渲染数据。
type CompressData struct {
	MaxWords int    `json:"max_words"`
	Content  string `json:"content"`
}

// 内置 v1 模板。逐字节对应当前代码里的硬编码提示词（api/investigate.go、
// agent/llm_compressor.go、ainexus/server/server.go），等价性由回归测试
// （TestInvestigateTemplateV1MatchesLegacy 等）守护——修改任何一侧都必须同步。

const builtinInvestigateSystemTpl = `你是资深运维工程师，擅长 Docker Swarm 集群故障排查。请基于提供的告警与证据链，分析根因并给出可执行的处置建议（检查项、命令、预期结果），结论要具体、可操作。{{if .UseMCP}} 你已连接集群 Worker 的 MCP 工具（可查询服务状态、事件、审计、容器资源，执行受控命令采集证据）；如证据不足，请调用工具补充，并引用工具返回的结果支撑结论。{{end}}`

const builtinInvestigateUserTpl = `【告警】
集群: {{.Alert.Cluster}}
服务: {{.Alert.Service}}
类型: {{.Alert.Type}}
级别: {{.Alert.Level}}
标题: {{.Alert.Title}}
次数: {{.Alert.Count}}
首次: {{.Alert.FirstTS}}
最近: {{.Alert.LastTS}}

{{if .Events}}【该服务近期监控事件】
{{.Events}}

{{end}}{{if .Audit}}【近期操作审计】
{{.Audit}}

{{end}}{{if .Logs}}【服务日志（最近）】
{{.Logs}}{{end}}
请给出根因分析与处置建议。`

// builtinCompressSystemTpl 与 agent.summaryPromptTemplate 保持一致
// （等价性由 agent 包回归测试守护）。
const builtinCompressSystemTpl = `你是会话压缩器。把下面的对话轮次压缩成一段不超过 {{.MaxWords}} 字的纪要,
只保留对后续排查有用的信息,按类别组织:
- 根因/结论:已确认或高度怀疑的根因
- 关键操作:执行过的命令/工具调用(名称+简要结果)
- 关键观测:数值(CPU/内存/端口等)与状态变化
- 待办/下一步:未完成的分析或建议
丢弃寒暄、重复与无关内容。直接输出纪要,不要解释。

对话轮次:
{{.Content}}
`

// builtinPatrolSystemTpl 与 server.Summarize 内置 system 消息保持一致。
// 报告经飞书 post 富文本逐行展示：post 无表格/样式能力，提示词引导模型
// 用短段落与列表输出，少产 Markdown 装饰（转换器仍会兜底降级）。
const builtinPatrolSystemTpl = `你是智能运维巡检报告助手。基于巡检检查结果，给出简明、结构化的报告：异常概况、逐项说明、处置建议。不要编造数据。报告将在飞书通知中逐行展示：用短段落和以 - 开头的列表组织内容，不要输出 Markdown 表格，不要使用 # 标题、** 加粗、> 引用等标记。`

// scenarioDef 内置场景定义。
type scenarioDef struct {
	Key  string
	Name string
	// Prototype 零值渲染校验/预览解码用的数据原型（nil = custom 类场景）。
	Prototype func() any
}

var scenarioDefs = []scenarioDef{
	{Key: ScenarioInvestigateSystem, Name: "深度排查 · 系统提示词", Prototype: func() any { return &InvestigateData{} }},
	{Key: ScenarioInvestigateUser, Name: "深度排查 · 证据消息模板", Prototype: func() any { return &InvestigateData{} }},
	{Key: ScenarioCompressSystem, Name: "会话压缩 · 系统提示词", Prototype: func() any { return &CompressData{MaxWords: 80, Content: "…"} }},
	{Key: ScenarioPatrolSystem, Name: "巡检报告 · 系统提示词", Prototype: func() any { return &struct{}{} }},
}

func findScenarioDef(key string) *scenarioDef {
	for i := range scenarioDefs {
		if scenarioDefs[i].Key == key {
			return &scenarioDefs[i]
		}
	}
	return nil
}

// builtinMessages 场景内置 v1 的消息列表。
func builtinMessages(key string) []builtinMsg {
	switch key {
	case ScenarioInvestigateSystem:
		return []builtinMsg{{Role: "system", Template: builtinInvestigateSystemTpl}}
	case ScenarioInvestigateUser:
		return []builtinMsg{{Role: "user", Template: builtinInvestigateUserTpl}}
	case ScenarioCompressSystem:
		return []builtinMsg{{Role: "system", Template: builtinCompressSystemTpl}}
	case ScenarioPatrolSystem:
		return []builtinMsg{{Role: "system", Template: builtinPatrolSystemTpl}}
	}
	return nil
}

type builtinMsg struct {
	Role     string
	Template string
}

// builtinVariables 场景变量说明（展示 + 预览提示用）。
func builtinVariables(key string) []promptVariableView {
	switch key {
	case ScenarioInvestigateSystem:
		return []promptVariableView{{Name: "UseMCP", Note: "是否已连接集群 Worker MCP 工具"}}
	case ScenarioInvestigateUser:
		return []promptVariableView{
			{Name: "Alert.Cluster", Required: true, Note: "集群名"},
			{Name: "Alert.Service", Required: true, Note: "服务名"},
			{Name: "Alert.Type", Required: true, Note: "告警类型"},
			{Name: "Alert.Level", Required: true, Note: "告警级别"},
			{Name: "Alert.Title", Required: true, Note: "告警标题"},
			{Name: "Alert.Count", Required: true, Note: "触发次数"},
			{Name: "Alert.FirstTS", Required: true, Note: "首次触发时间"},
			{Name: "Alert.LastTS", Required: true, Note: "最近触发时间"},
			{Name: "Events", Note: "近期监控事件（空 = 段落省略）"},
			{Name: "Audit", Note: "近期操作审计（空 = 段落省略）"},
			{Name: "Logs", Note: "服务日志（空 = 段落省略）"},
		}
	case ScenarioCompressSystem:
		return []promptVariableView{
			{Name: "MaxWords", Required: true, Note: "纪要字数上限（当前固定 80）"},
			{Name: "Content", Required: true, Note: "待压缩的对话轮次"},
		}
	}
	return nil
}

type promptVariableView struct {
	Name     string
	Required bool
	Note     string
}
