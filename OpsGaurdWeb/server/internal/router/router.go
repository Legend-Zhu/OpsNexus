// Package router wires the management-plane HTTP routes.
//
// Route groups (skeletons — handlers return placeholders until business
// logic lands):
//
//	/api/v1/clusters        多集群管理（类 Rancher）
//	/api/v1/clusters/:name/workloads   工作负载（经 Worker 编排）
//	/api/v1/clusters/:name/events      监控事件
//	/api/v1/clusters/:name/audit       审计
//	/api/v1/ainexus/*       AiNexus 异常排查服务代理
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
		clusters.GET("/:name/workloads/:service", h.GetWorkload)

		// 监控事件 / 审计
		clusters.GET("/:name/events", h.ListEvents)
		clusters.GET("/:name/audit", h.ListAudit)

		// AiNexus 异常排查服务（整合为独立服务）
		ainexus := v1.Group("/ainexus")
		{
			ainexus.GET("/health", h.AINexusHealth)
			ainexus.GET("/models", h.ListAINexusModels)
			ainexus.POST("/chat", h.AINexusChat)
		}
	}

	return r
}
