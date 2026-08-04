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

	// API v1
	v1 := r.Group("/api/v1")
	{
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

		// AiNexus 异常排查（内嵌网关，进程内直调）
		ainexus := v1.Group("/ainexus")
		{
			ainexus.GET("/health", h.AINexusHealth)
			ainexus.GET("/models", h.ListAINexusModels)
			ainexus.POST("/chat", h.AINexusChat)
		}
	}

	return r
}
