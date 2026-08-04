package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
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

// Handlers groups the HTTP handlers. It is a skeleton holder for the
// dependencies (config, worker proxy client, ainexus client) that the
// business logic will need.
type Handlers struct {
	// future deps: config *config.Config, workerProxy *proxy.Client, ainx *ainexus.Client
}

// NewHandlers constructs the handler set.
func NewHandlers() *Handlers {
	return &Handlers{}
}

// Health godoc: GET /healthz
func (h *Handlers) Health(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"status": "up"})
}
