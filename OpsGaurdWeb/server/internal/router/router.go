// Package router wires the management-plane HTTP routes.
//
// Route groups (skeletons — handlers return placeholders until business
// logic lands):
//
//	/api/v1/clusters        多集群管理（类 Rancher）
//	/api/v1/clusters/:name/workloads   工作负载（经 Worker 编排）
//	/api/v1/clusters/:name/events      监控事件
//	/api/v1/clusters/:name/audit       审计
//	/api/v1/ainexus/*       AiNexus 异常排查（内嵌网关，进程内直调）
//	/ainexus/*              内嵌 AiNexus 网关原生端点（兼容其 URL 契约）
package router

import (
	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
)

// New builds the gin engine with all routes registered.
func New(h *api.Handlers) *gin.Engine {
	r := gin.Default()

	// 健康检查
	r.GET("/healthz", h.Health)

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
		// 登录（放行）
		v1.POST("/auth/login", h.Login)

		if authMW != nil {
			v1.Use(authMW)
		}

		// 集群管理（多集群，类 Rancher）
		clusters := v1.Group("/clusters")
		{
			clusters.GET("", h.ListClusters)
			clusters.POST("", h.AddCluster)
			clusters.GET("/:name", h.GetCluster)
			clusters.DELETE("/:name", h.RemoveCluster)
		}

		// 工作负载（经 Worker）
		clusters.GET("/:name/workloads", h.ListWorkloads)
		clusters.POST("/:name/workloads", h.DeployWorkload)
		clusters.GET("/:name/workloads/:service", h.GetWorkload)
		clusters.DELETE("/:name/workloads/:service", h.RemoveWorkload)
		clusters.POST("/:name/workloads/:service/scale", h.ScaleWorkload)
		clusters.POST("/:name/workloads/:service/restart", h.RestartWorkload)
		clusters.GET("/:name/workloads/:service/logs", h.StreamWorkloadLogs)
		clusters.GET("/:name/workloads/ops/:id", h.GetWorkloadOperation)

		// 监控事件 / 审计
		clusters.GET("/:name/events", h.ListEvents)
		clusters.GET("/:name/audit", h.ListAudit)
		clusters.GET("/:name/metrics", h.ClusterMetrics)

		// 告警 ingest（Worker webhook 入口）
		v1.POST("/ingest/events", h.IngestEvent)

		// 告警中心
		alerts := v1.Group("/alerts")
		{
			alerts.GET("", h.ListAlerts)
			alerts.POST("/:id/ack", h.AckAlert)
			alerts.POST("/:id/recover", h.RecoverAlert)
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

		// AiNexus 异常排查（内嵌网关，进程内直调）
		ainexus := v1.Group("/ainexus")
		{
			ainexus.GET("/health", h.AINexusHealth)
			ainexus.GET("/models", h.ListAINexusModels)
			ainexus.POST("/chat", h.AINexusChat)
			ainexus.POST("/investigate", h.AINexusInvestigate)
		}
	}

	return r
}
