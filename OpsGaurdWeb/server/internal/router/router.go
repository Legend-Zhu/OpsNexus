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

		// 内嵌镜像仓库（管理 API：构建提交/轮询、镜像列表、删除 tag）
		registry := v1.Group("/registry")
		{
			registry.GET("/info", h.RegistryInfo)
			registry.POST("/builds", h.SubmitRegistryBuild)
			registry.GET("/builds", h.ListRegistryBuilds)
			registry.GET("/builds/:id", h.GetRegistryBuild)
			registry.GET("/images", h.ListRegistryImages)
			registry.DELETE("/images/*ref", h.DeleteRegistryTag)
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

		// 认证 / 用户（P6）
		v1.GET("/auth/me", h.Me)
		v1.GET("/users", h.ListUsers)
		v1.POST("/users", h.CreateUser)

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

		// AiNexus 异常排查（内嵌网关，进程内直调；config 为页面管理的网关配置）
		ainexus := v1.Group("/ainexus")
		{
			ainexus.GET("/health", h.AINexusHealth)
			ainexus.GET("/models", h.ListAINexusModels)
			ainexus.POST("/chat", h.AINexusChat)
			ainexus.POST("/investigate", h.AINexusInvestigate)
			ainexus.GET("/config", h.GetAINexusConfig)
			ainexus.PUT("/config", h.UpdateAINexusConfig)
			ainexus.POST("/config/test", h.TestAINexusProvider)
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
