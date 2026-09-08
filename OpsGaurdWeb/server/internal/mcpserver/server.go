// MCP Server 主体：构建官方 go-sdk server（与 Worker/internal/mcp 同构：
// stateless Streamable HTTP，typed struct 自动生成 schema），注册六组工具，
// 并持有各后端依赖。工具层零业务逻辑——全部薄封装既有 service 层。
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexusrt"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/patrol"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/registry"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// Version MCP Server 版本（P1 硬编码；后续随构建注入）。
const Version = "1.0.0"

// serverInstructions 给外部助手的运行手册（工具选择/破坏性操作守则/协同
// 排障用法）。常量化便于测试守护关键守则。
const serverInstructions = "OpsGaurd 智能运维平台管理 MCP。能力：项目/集群纳管/服务部署与治理/zip 构建镜像/告警排查/巡检/告警规则/通知。" +
	"所有集群类工具都要求 cluster 参数，先用 cluster_list 查可用集群名。" +
	"构建镜像流程固定为：build_upload_begin 拿票据 → curl 直传 zip（票据仅一次性）→ build_submit。" +
	"部署用 service_deploy（config 为 Worker YAML，镜像引用 registry.opsguard/<name>:<tag>），" +
	"返回异步 operation，用 service_operation 轮询到 done|failed。" +
	"破坏性操作（project_delete/cluster_remove/service_remove/service_scale 到 0/image_delete/patrol_delete/alertrule_delete）" +
	"要求 confirm=true——必须先向用户复述影响面并得到明确确认后才能传 true。" +
	"排查：快查用 service_logs/node_stats/probe_*（秒级、零成本）；深挖用 investigation_start 委托内嵌 AI 排查" +
	"（question 参数建议按「现象 + 代码侧假设 + 请确认什么」组织），investigation_get 轮询结论，" +
	"investigation_continue 多轮追问。巡检任务用 patrol_*（patrol_create 的 yaml 骨架见工具描述）。" +
	"service_exec/node_exec 是命令执行类最高危工具，仅在与用户明确确认诊断意图后使用。"

// Deps MCP 工具的后端依赖（均为既有 service，直接复用）。
type Deps struct {
	Store    *store.Store
	Clusters *cluster.Service
	// Registry 内嵌镜像仓库服务；nil = 未启用（构建/镜像工具返回引导错误）。
	Registry *registry.Service
	// AiNexus 内嵌网关运行时；nil = 未启用（委托排查/exec 透传返回引导错误）。
	AiNexus *ainexusrt.Service
	// Patrol 智能巡检服务（nil = 巡检工具返回引导错误）。
	Patrol *patrol.Service
	// AlertRules 告警规则服务（nil = 规则工具返回引导错误）。
	AlertRules *alertrule.Service
	// Notify 通知服务（nil = 通知工具返回引导错误）。
	Notify *notify.Service
	// IdP 内嵌 IdP 的 token 校验器（nil = 未启用 IdP，/mcp 仅接受静态/
	// 运行时 token；启用后静态未命中时回退校验 IdP access token）。
	IdP IdPTokenValidator
	// IdPIssuer 内嵌 IdP 的 issuer 地址（RFC 9728 元数据的
	// authorization_servers 字段；空 = 不宣告）。
	IdPIssuer string
	Config    Config
	Log       *slog.Logger
}

// Handler MCP Server 及其依赖。
type Handler struct {
	deps    Deps
	log     *slog.Logger
	auth    *authorizer
	srv     *mcp.Server
	tickets *uploadTable
	usage   *usageTable
	invMgr  *investigationManager
}

// New 构建 MCP Server 并注册全部工具。
func New(deps Deps) *Handler {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	deps.Config.applyDefaults()
	h := &Handler{
		deps:    deps,
		log:     deps.Log,
		auth:    newAuthorizer(deps.Config.Tokens),
		tickets: newUploadTable(),
		usage:   newUsageTable(),
	}
	h.invMgr = newInvestigationManager(h)
	// 运行时 token 表加载 + 静态种子镜像（失败不阻断启动，降级为纯静态表）
	if err := h.syncTokens(); err != nil {
		h.log.Error("mcp runtime token load failed (static tokens only)", "err", err)
	}
	h.usage.Start(h)

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "opsguard",
		Title:   "OpsGaurd",
		Version: Version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		Logger:       deps.Log,
	})

	h.registerProjectTools(srv)
	h.registerClusterTools(srv)
	h.registerServiceTools(srv)
	h.registerBuildTools(srv)
	h.registerAlertTools(srv)
	h.registerInvestigationTools(srv)
	h.registerDiagTools(srv)
	h.registerPatrolTools(srv)
	h.registerAlertRuleTools(srv)
	h.registerNotifyTools(srv)
	h.registerExecTools(srv)
	h.registerResources(srv)
	h.srv = srv
	return h
}

// HTTPHandler stateless Streamable HTTP（2026-07-28 规范：无会话，每请求
// 独立处理）。与 Worker MCP 完全同构。
func (h *Handler) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return h.srv
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// Ready 启动自检：enabled 但没有任何可用 token 时报错（/mcp 将对一切
// 请求 401，等价于不可用——fail fast 让配置错误启动即暴露）。
func (h *Handler) Ready() error {
	if !h.auth.enabled() {
		return fmt.Errorf("mcp.enabled=true but no usable mcp.tokens (name+secret+scope) configured")
	}
	return nil
}

// TokenNames 已配置 token 名（健康检查展示）。
func (h *Handler) TokenNames() []string { return h.auth.tokenNames() }

// Stop 停止后台任务并做最后一次用量 flush（进程优雅退出时调用）。
func (h *Handler) Stop() { h.usage.Stop(h) }

// AuditQuery 审计查询（管理 API 用；actor/tool 为空 = 全部）。
func (h *Handler) AuditQuery(actor, tool string, limit int) ([]*store.MCPAudit, error) {
	if h.deps.Store == nil {
		return nil, fmt.Errorf("store not initialized")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	recs, err := h.deps.Store.ListMCPAudit(limit)
	if err != nil {
		return nil, err
	}
	out := recs[:0]
	for _, r := range recs {
		if actor != "" && r.Actor != actor {
			continue
		}
		if tool != "" && r.Tool != tool {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// gateway 当前内嵌 AiNexus 网关（nil = 未启用；指针热重载后失效，调用方
// 须单次请求内用完——investigation 后台体每次调用都重新取）。
func (h *Handler) gateway() *ainexusserver.Server {
	if h.deps.AiNexus == nil {
		return nil
	}
	return h.deps.AiNexus.Server()
}

// modelName 委托排查用的模型：investigate 场景绑定 → 网关默认可用。
func (h *Handler) modelName() string {
	if h.deps.AiNexus == nil {
		return ""
	}
	return h.deps.AiNexus.EffectiveModel("investigate", "")
}

// --- 结果体积防线（设计方案 §八：128KB 头尾保留截断） ---

// maxResultBytes 单个字符串字段的结果上限（与 ainexus MCP 客户端侧
// MaxToolResultBytes 一致；本端先截，避免超长日志/事件打爆助手上下文）。
const maxResultBytes = 128 << 10

// truncStr 头尾保留、中部省略（与 ainexus/mcp_tool.go 截断策略一致）。
func truncStr(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	runes := []rune(s)
	if len(runes)*4 <= maxResultBytes { // 罕见：多字节为主
		return s
	}
	// 以 rune 为单位各取一半预算
	half := maxResultBytes / 2
	head := truncateRunes(runes, half)
	tailRunes := []rune(s)
	tail := tailRunes
	if len(tail) > half/2 {
		tail = tail[len(tail)-half/2:]
	}
	omitted := len(runes) - len([]rune(head)) - len(tail)
	return head + fmt.Sprintf("\n…[中间省略 %d 字符]\n", omitted) + string(tail)
}

// truncateRunes 截取前 n 字节内的完整 rune 序列。
func truncateRunes(rs []rune, n int) string {
	size := 0
	i := 0
	for ; i < len(rs); i++ {
		size += len(string(rs[i]))
		if size > n {
			break
		}
	}
	return string(rs[:i])
}

// --- audited 通用包装：write scope 守卫 + 审计，消除每个写工具的样板 ---

// audited 包装写工具 handler：先查 write scope，再执行，最后审计。
// 读工具不套（不审计、只要求已通过端点鉴权）。
func audited[In any, Out any](h *Handler, tool string, argsOf func(In) map[string]string,
	fn func(ctx context.Context, in In) (Out, error),
) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		if err := h.writeGuard(ctx, tool); err != nil {
			return nil, zero, err
		}
		start := time.Now()
		out, err := fn(ctx, in)
		h.auditWrite(ctx, tool, summarizeArgs(argsOf(in)), err, start)
		if err != nil {
			return nil, zero, err
		}
		return nil, out, nil
	}
}

// joinLines 拼接日志行（truncStr 前的组装）。
func joinLines(lines []string) string { return strings.Join(lines, "\n") }
