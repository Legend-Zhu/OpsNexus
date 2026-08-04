package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
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
// wired in P1/P2; alert ingest (P3) and the embedded AiNexus gateway
// (nil when disabled) follow.
type Handlers struct {
	clusters    *cluster.Service
	ingestSvc   *ingest.Service
	ingestToken string
	AINexus     *ainexusserver.Server // 内嵌 AiNexus 网关（config 未启用时为 nil）
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

// Health godoc: GET /healthz
func (h *Handlers) Health(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"status": "up"})
}
