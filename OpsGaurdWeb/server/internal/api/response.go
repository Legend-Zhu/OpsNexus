package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexusrt"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/auth"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/patrol"
)

// Response is the standard envelope for all management-plane endpoints.
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ok writes a success envelope.
func ok(c *gin.Context, status int, data any) {
	c.JSON(status, Response{Code: status, Message: "ok", Data: data})
}

// fail writes an error envelope.
func fail(c *gin.Context, status int, message string) {
	c.JSON(status, Response{Code: status, Message: message})
}

// Handlers groups the HTTP handlers. Cluster registry + Worker proxying are
// wired in P1/P2; alert ingest (P3), patrol (P5), notify/alertrule/auth (P6)
// and the embedded AiNexus gateway runtime (P7, hot-reload config) follow.
type Handlers struct {
	clusters    *cluster.Service
	ingestSvc   *ingest.Service
	ingestToken string
	patrolSvc   *patrol.Service
	notifySvc   *notify.Service
	ruleSvc     *alertrule.Service
	authSvc     *auth.Service
	AINexusRT   *ainexusrt.Service // 内嵌 AiNexus 网关运行时（热重载配置；Server() 为空 = 未启用）
	// AuthMiddleware 认证中间件（P6；nil = 未启用认证）。
	AuthMiddleware gin.HandlerFunc
}

// NewHandlers constructs the handler set.
func NewHandlers() *Handlers {
	return &Handlers{}
}

// SetClusterService wires the cluster registry service (P1).
func (h *Handlers) SetClusterService(s *cluster.Service) { h.clusters = s }

// SetIngestService wires the webhook ingest service (P3) and its token.
func (h *Handlers) SetIngestService(s *ingest.Service, token string) {
	h.ingestSvc = s
	h.ingestToken = token
}

// SetPatrolService wires the patrol service (P5).
func (h *Handlers) SetPatrolService(s *patrol.Service) { h.patrolSvc = s }

// SetNotifyService wires the notification service (P6).
func (h *Handlers) SetNotifyService(s *notify.Service) { h.notifySvc = s }

// SetAlertRuleService wires the alert-rule service (P6).
func (h *Handlers) SetAlertRuleService(s *alertrule.Service) { h.ruleSvc = s }

// SetAuthService wires the auth service (P6).
func (h *Handlers) SetAuthService(s *auth.Service) { h.authSvc = s }

// SetAuthMiddleware wires the auth middleware (P6; nil disables auth).
func (h *Handlers) SetAuthMiddleware(m gin.HandlerFunc) { h.AuthMiddleware = m }

// SetAINexusRT wires the AiNexus gateway runtime service (P7; hot-reload).
func (h *Handlers) SetAINexusRT(s *ainexusrt.Service) { h.AINexusRT = s }

// Health godoc: GET /healthz
func (h *Handlers) Health(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"status": "up"})
}
