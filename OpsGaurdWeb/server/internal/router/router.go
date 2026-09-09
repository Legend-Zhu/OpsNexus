// Package router wires the management-plane HTTP routes.
//
// Route groups:
//
//	/api/v1/projects        项目（管理层级第一层：项目 → 集群）
//	/api/v1/clusters        多集群管理
//	/api/v1/clusters/:name/workloads   工作负载（经 Worker 编排）
//	/api/v1/clusters/:name/events      监控事件
//	/api/v1/clusters/:name/audit       审计
//	/api/v1/ainexus/*       AiNexus 异常排查（内嵌网关，进程内直调）
//	/ainexus/*              内嵌 AiNexus 网关原生端点（兼容其 URL 契约）
//	/                       前端控制台（dist 静态托管 + SPA 回退）
package router

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
)

// New builds the gin engine with all routes registered.
func New(h *api.Handlers) *gin.Engine {
	r := gin.Default()

	// 健康检查
	r.GET("/healthz", h.Health)

	// 内嵌镜像仓库(OCI /v2 协议端点:docker CLI 直连,独立 basic auth,
	// 不走管理端会话认证)
	if reg := h.Registry(); reg != nil {
		v2 := r.Group("/v2", reg.AuthMiddleware())
		v2.Any("/*rest", reg.V2)
	}

	// 内嵌 AiNexus 网关原生端点（兼容 AiNexus 自身 URL 契约，非独立服务）
	ainx := r.Group("/ainexus")
	{
		ainx.GET("/health", h.AINexusHealth)
		ainx.GET("/v1/models", h.EmbedModelsHandler)
		ainx.GET("/api/models", h.ListAINexusModels)
		ainx.GET("/api/tools", h.EmbedToolsHandler)
		ainx.GET("/api/mcp", h.EmbedMCPHandler)
		ainx.POST("/v1/chat/completions", h.EmbedOpenAIHandler)
		ainx.POST("/v1/messages", h.EmbedAnthropicHandler)
	}

	// 管理端 MCP Server（外部 AI 助手经 /mcp 用自然语言操作平台）：
	// 独立命名 token 鉴权（不走平台会话认证），与 /v2 同为自治端点；
	// build-upload 为构建包一次性票据直传端点（zip 不经 MCP 协议）；
	// well-known 为 RFC 9728 OAuth 保护资源元数据（公开，供助手发现）。
	if mc := h.MCPServer(); mc != nil {
		r.Any("/mcp", mc.Middleware(), gin.WrapH(mc.HTTPHandler()))
		r.POST("/api/v1/mcp/build-upload", mc.UploadBuildPackage)
		r.GET("/.well-known/oauth-protected-resource", mc.Metadata)
	}

	// 认证中间件（P6）：Bearer token 校验；放行健康检查、webhook ingest、
	// 登录与 AiNexus 原生端点（内嵌网关自身无鉴权，统一由管理端覆盖）。
	var authMW gin.HandlerFunc
	if h.AuthMiddleware != nil {
		authMW = h.AuthMiddleware
	}

	// API v1
	v1 := r.Group("/api/v1")
	{
		// 登录 / SSO（放行，注册在认证中间件之前）
		v1.POST("/auth/login", h.Login)
		v1.GET("/auth/sso/login", h.LoginSSO)
		v1.GET("/auth/sso/status", h.SSOStatus)
		v1.GET("/auth/callback", h.SSOCallback)

		// IdP（OpsGaurd 作为 OIDC 身份提供者）公开端点。
		// authorize/token 靠 cookie 会话 + client 凭证/PKCE 自认证，不走 Bearer；
		// jwks / discovery 供 RP 发现公钥与端点。注册在 AuthMiddleware 之前。
		if idpSvc := h.IdP(); idpSvc != nil {
			v1.GET("/idp/authorize", idpSvc.Authorize)
			v1.POST("/idp/token", idpSvc.Token)
			v1.GET("/idp/jwks", idpSvc.JWKS)
			v1.GET("/idp/userinfo", idpSvc.UserInfo) // 自身用 Bearer access token
			v1.POST("/idp/introspect", idpSvc.Introspect)
			v1.GET("/idp/logout", idpSvc.Logout)
			r.GET("/.well-known/openid-configuration", idpSvc.Discovery)
		}

		if authMW != nil {
			v1.Use(authMW)
		}

		// 项目（管理层级第一层：项目 → 集群）
		projects := v1.Group("/projects")
		{
			projects.GET("", h.ListProjects)
			projects.POST("", h.CreateProject)
			projects.GET("/:id", h.GetProject)
			projects.PUT("/:id", h.UpdateProject)
			projects.DELETE("/:id", h.DeleteProject)
		}

		// 集群管理（多集群）
		clusters := v1.Group("/clusters")
		{
			clusters.GET("", h.ListClusters)
			clusters.POST("", h.AddCluster)
			clusters.GET("/:name", h.GetCluster)
			clusters.PUT("/:name", h.UpdateCluster)
			clusters.DELETE("/:name", h.RemoveCluster)
		}

		// 工作负载（经 Worker）
		clusters.GET("/:name/workloads", h.ListWorkloads)
		clusters.POST("/:name/workloads", h.DeployWorkload)
		clusters.GET("/:name/workloads/:service", h.GetWorkload)
		clusters.PUT("/:name/workloads/:service", h.UpdateWorkload)
		clusters.DELETE("/:name/workloads/:service", h.RemoveWorkload)
		clusters.POST("/:name/workloads/:service/scale", h.ScaleWorkload)
		clusters.POST("/:name/workloads/:service/restart", h.RestartWorkload)
		clusters.GET("/:name/workloads/:service/logs", h.StreamWorkloadLogs)
		clusters.GET("/:name/workloads/ops/:id", h.GetWorkloadOperation)

		// 监控事件 / 审计 / 节点（管理层级：集群 → 节点 → 容器/进程）
		clusters.GET("/:name/events", h.ListEvents)
		clusters.GET("/:name/audit", h.ListAudit)
		clusters.GET("/:name/metrics", h.ClusterMetrics)
			clusters.GET("/:name/nodes", h.ListNodes)
			clusters.GET("/:name/nodes/stream", h.StreamNodes)
			clusters.GET("/:name/nodes/:id/processes", h.NodeProcesses)
		clusters.GET("/:name/nodes/:id/containers", h.NodeContainers)
		clusters.POST("/:name/nodes/:id/containers/restart", h.NodeContainerRestart)

		// 纳管清单（集群纳管的外部对象：standalone 容器 / 宿主机服务）
		clusters.GET("/:name/inventory", h.GetInventory)
		clusters.PUT("/:name/inventory", h.UpsertInventory)

		// 告警中心
		alerts := v1.Group("/alerts")
		{
			alerts.GET("", h.ListAlerts)
			alerts.POST("/:id/ack", h.AckAlert)
			alerts.POST("/:id/recover", h.RecoverAlert)
			alerts.GET("/:id/investigations", h.ListAlertInvestigations)
		}

		// 排查会话（对话式 troubleshoot 落库）
		invs := v1.Group("/investigations")
		{
			invs.GET("", h.ListInvestigations)
			invs.POST("", h.SaveInvestigation)
			invs.PUT("/:id", h.UpdateInvestigation)
			invs.GET("/:id", h.GetInvestigation)
		}

		// 全局设置（巡检报告投递策略等）
		v1.GET("/settings/patrol-report", h.GetPatrolReportSetting)
		v1.PUT("/settings/patrol-report", h.PutPatrolReportSetting)

		// 密钥（巡检 flow 拨测账号；列表不返回值）
		v1.GET("/secrets", h.ListSecrets)
		v1.PUT("/secrets/:name", h.PutSecret)
		v1.DELETE("/secrets/:name", h.DeleteSecret)

		// 内嵌镜像仓库（管理 API：构建提交/轮询、镜像列表、删除 tag/仓库）
		registry := v1.Group("/registry")
		{
			registry.GET("/info", h.RegistryInfo)
			registry.POST("/builds", h.SubmitRegistryBuild)
			registry.GET("/builds", h.ListRegistryBuilds)
			registry.GET("/builds/:id", h.GetRegistryBuild)
			registry.GET("/images", h.ListRegistryImages)
			registry.DELETE("/images/*ref", h.DeleteRegistryImage)
		}

		// 智能巡检（YAML 流程 + 内置调度 + AI 报告）
		patrols := v1.Group("/patrols")
		{
			patrols.GET("", h.ListPatrols)
			patrols.POST("", h.CreatePatrol)
			patrols.GET("/:id", h.GetPatrol)
			patrols.PUT("/:id", h.UpdatePatrol)
			patrols.DELETE("/:id", h.DeletePatrol)
			patrols.POST("/:id/run", h.RunPatrol)
			patrols.GET("/:id/runs", h.ListPatrolRuns)
			patrols.GET("/:id/runs/:runId", h.GetPatrolRun)
			patrols.GET("/:id/reports", h.ListPatrolReports)
		}

		// 通知中心（P6：渠道/策略/记录）
		notify := v1.Group("/notify")
		{
			notify.GET("/channels", h.ListChannels)
			notify.POST("/channels", h.CreateChannel)
			notify.PUT("/channels/:id", h.UpdateChannel)
			notify.DELETE("/channels/:id", h.DeleteChannel)
			notify.GET("/policies", h.ListPolicies)
			notify.PUT("/policies/:level", h.UpsertPolicy)
			notify.GET("/records", h.ListNotifyRecords)
		}

		// 告警规则（P6：管理 Worker monitoring config，决策⑦）
		rules := v1.Group("/alertrules")
		{
			rules.GET("", h.ListAlertRules)
			rules.PUT("", h.UpsertAlertRule)
			rules.POST("/apply", h.ApplyAlertRule)
			rules.DELETE("/:cluster/:service", h.DeleteAlertRule)
		}

		// 认证 / 用户（P6）：修改密码自助；用户管理（列表/新增/重置密码）admin only
		v1.GET("/auth/me", h.Me)
		v1.PUT("/auth/password", h.ChangePassword)
		usersAdmin := v1.Group("/users")
		if adminMW := h.AdminMiddleware(); adminMW != nil {
			usersAdmin.Use(adminMW)
		}
		usersAdmin.GET("", h.ListUsers)
		usersAdmin.POST("", h.CreateUser)
		usersAdmin.PUT("/:username/password", h.ResetPassword)

		// IdP client 管理（admin only）：注册 / 编辑 / 删除 / 轮换密钥。
		// IdP 未启用时不挂载（h.IdP()==nil）。
		if h.IdP() != nil {
			idpAdmin := v1.Group("/idp/clients")
			if adminMW := h.AdminMiddleware(); adminMW != nil {
				idpAdmin.Use(adminMW)
			}
			idpAdmin.GET("", h.ListClients)
			idpAdmin.POST("", h.CreateClient)
			idpAdmin.GET("/:id", h.GetClient)
			idpAdmin.PUT("/:id", h.UpdateClient)
			idpAdmin.DELETE("/:id", h.DeleteClient)
			idpAdmin.POST("/:id/rotate-secret", h.RotateClientSecret)
		}

		// 管理端 MCP Server 管理 API（admin only）：token 运行时管理、
		// 审计/用量查询、接入信息。MCP 未启用时 mcpSvc 为 nil，各接口 503。
		mcpAdmin := v1.Group("/mcp")
		if adminMW := h.AdminMiddleware(); adminMW != nil {
			mcpAdmin.Use(adminMW)
		}
		mcpAdmin.GET("/info", h.MCPInfo)
		mcpAdmin.GET("/tokens", h.ListMCPTokens)
		mcpAdmin.POST("/tokens", h.CreateMCPToken)
		mcpAdmin.DELETE("/tokens/:name", h.DeleteMCPToken)
		mcpAdmin.PUT("/tokens/:name/enabled", h.SetMCPTokenEnabled)
		mcpAdmin.GET("/audit", h.ListMCPAudit)
		mcpAdmin.GET("/usage", h.MCPUsage)

		// AiNexus 异常排查（内嵌网关，进程内直调；config 为页面管理的网关配置）。
		// config 读返回脱敏视图（api_key_set），普通认证可读；写/测试收敛 admin。
		ainexus := v1.Group("/ainexus")
		{
			ainexus.GET("/health", h.AINexusHealth)
			ainexus.GET("/models", h.ListAINexusModels)
			ainexus.POST("/chat", h.AINexusChat)
			ainexus.POST("/investigate", h.AINexusInvestigate)
			ainexus.GET("/config", h.GetAINexusConfig)
			ainexusAdmin := ainexus.Group("")
			if adminMW := h.AdminMiddleware(); adminMW != nil {
				ainexusAdmin.Use(adminMW)
			}
			ainexusAdmin.PUT("/config", h.UpdateAINexusConfig)
			ainexusAdmin.POST("/config/test", h.TestAINexusProvider)
		}

		// MLOps 运营层（P1 Prompt Hub；P2 用量费用；mlops.enabled=false
		// 时不挂载）。读操作普通认证；写操作与审计查询挂 admin 守卫。
		if h.MLOps() != nil {
			ml := v1.Group("/mlops")
			{
				ml.GET("/prompts", h.ListMLOpsPrompts)
				ml.GET("/prompts/:id", h.GetMLOpsPrompt)
				ml.POST("/prompts/:id/render", h.RenderMLOpsPrompt)

				// 费用报表（读）：overview/trend/detail/operation 下钻/价格列表
				ml.GET("/costs/overview", h.MLOpsCostsOverview)
				ml.GET("/costs/trend", h.MLOpsCostsTrend)
				ml.GET("/costs/detail", h.MLOpsCostsDetail)
				ml.GET("/costs/operations/:id", h.MLOpsCostsOperation)
				ml.GET("/costs/pricing", h.ListMLOpsPricing)

				// 模型运营（读）：视图/健康缓存/绑定/预算
				ml.GET("/models", h.ListMLOpsModels)
				ml.GET("/models/health", h.ListMLOpsModelHealth)
				ml.GET("/bindings", h.ListMLOpsBindings)
				ml.GET("/budgets", h.ListMLOpsBudgets)

				admin := ml.Group("")
				if adminMW := h.AdminMiddleware(); adminMW != nil {
					admin.Use(adminMW)
				}
				admin.GET("/audit", h.ListMLOpsAudits)
				admin.POST("/prompts", h.CreateMLOpsPrompt)
				admin.PUT("/prompts/:id", h.SaveMLOpsPromptVersion)
				admin.POST("/prompts/:id/activate", h.ActivateMLOpsPrompt)
				admin.POST("/prompts/:id/reset", h.ResetMLOpsPrompt)
				admin.DELETE("/prompts/:id", h.DeleteMLOpsPrompt)
				admin.PUT("/costs/pricing", h.SaveMLOpsPricing)
				admin.DELETE("/costs/pricing", h.DeleteMLOpsPricing)
				admin.POST("/models/enable", h.EnableMLOpsModel)
				admin.POST("/models/disable", h.DisableMLOpsModel)
				admin.POST("/models/health", h.TestMLOpsModelHealth)
				admin.PUT("/bindings/:scenario", h.SaveMLOpsBinding)
				admin.DELETE("/bindings/:scenario", h.DeleteMLOpsBinding)
				admin.POST("/budgets", h.SaveMLOpsBudget)
				admin.DELETE("/budgets/:month", h.DeleteMLOpsBudget)
			}
		}
	}

	// 前端控制台静态托管（SPA）：dist/ 目录（存在时挂载），
	// 未匹配的 GET 回退 index.html 支持前端路由（/dashboard /clusters …）。
	dist := "./web"
	if info, err := os.Stat(dist); err == nil && info.IsDir() {
		r.Static("/assets", filepath.Join(dist, "assets"))
		r.NoRoute(func(c *gin.Context) {
			if c.Request.Method != http.MethodGet {
				c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "not found"})
				return
			}
			c.File(filepath.Join(dist, "index.html"))
		})
	}

	return r
}
