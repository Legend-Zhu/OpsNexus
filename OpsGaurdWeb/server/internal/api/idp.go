package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/idp"
)

// 这些 handler 挂在需 admin 权限的路由组下（router.go 用 authSvc.RequireRole("admin")）。

// ListClients godoc: GET /api/v1/idp/clients
func (h *Handlers) ListClients(c *gin.Context) {
	if h.idpSvc == nil {
		fail(c, http.StatusServiceUnavailable, "idp not enabled")
		return
	}
	items, err := h.idpSvc.Clients().List()
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// createClientRequest 创建 client 入参。
type createClientRequest struct {
	Name         string   `json:"name" binding:"required"`
	RedirectURIs []string `json:"redirect_uris" binding:"required"`
	Scopes       []string `json:"scopes"`
	Public       bool     `json:"public"`
	TokenTTL     string   `json:"token_ttl"`
}

// CreateClient godoc: POST /api/v1/idp/clients
// 返回创建后的 client 及一次性明文 secret（机密客户端；secret 仅此一次可见）。
func (h *Handlers) CreateClient(c *gin.Context) {
	if h.idpSvc == nil {
		fail(c, http.StatusServiceUnavailable, "idp not enabled")
		return
	}
	var req createClientRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	cli, secret, err := h.idpSvc.Clients().CreateClient(idp.CreateClientInput{
		Name:         req.Name,
		RedirectURIs: req.RedirectURIs,
		Scopes:       req.Scopes,
		Public:       req.Public,
		TokenTTL:     req.TokenTTL,
	})
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusCreated, gin.H{"client": cli, "secret": secret})
}

// GetClient godoc: GET /api/v1/idp/clients/:id
func (h *Handlers) GetClient(c *gin.Context) {
	if h.idpSvc == nil {
		fail(c, http.StatusServiceUnavailable, "idp not enabled")
		return
	}
	cli, err := h.idpSvc.Clients().Get(c.Param("id"))
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	if cli == nil {
		fail(c, http.StatusNotFound, "client not found")
		return
	}
	ok(c, http.StatusOK, cli)
}

// updateClientRequest 更新 client 入参（指针字段为 nil 表示不改）。
type updateClientRequest struct {
	Name         *string   `json:"name,omitempty"`
	RedirectURIs *[]string `json:"redirect_uris,omitempty"`
	Scopes       *[]string `json:"scopes,omitempty"`
	TokenTTL     *string   `json:"token_ttl,omitempty"`
}

// UpdateClient godoc: PUT /api/v1/idp/clients/:id
func (h *Handlers) UpdateClient(c *gin.Context) {
	if h.idpSvc == nil {
		fail(c, http.StatusServiceUnavailable, "idp not enabled")
		return
	}
	var req updateClientRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	cli, err := h.idpSvc.Clients().UpdateClient(c.Param("id"), idp.UpdateClientInput{
		Name:         req.Name,
		RedirectURIs: req.RedirectURIs,
		Scopes:       req.Scopes,
		TokenTTL:     req.TokenTTL,
	})
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, cli)
}

// DeleteClient godoc: DELETE /api/v1/idp/clients/:id
func (h *Handlers) DeleteClient(c *gin.Context) {
	if h.idpSvc == nil {
		fail(c, http.StatusServiceUnavailable, "idp not enabled")
		return
	}
	if err := h.idpSvc.Clients().Delete(c.Param("id")); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, nil)
}

// RotateClientSecret godoc: POST /api/v1/idp/clients/:id/rotate-secret
// 返回一次性新明文 secret（旧 secret 立即失效）。
func (h *Handlers) RotateClientSecret(c *gin.Context) {
	if h.idpSvc == nil {
		fail(c, http.StatusServiceUnavailable, "idp not enabled")
		return
	}
	secret, err := h.idpSvc.Clients().RotateSecret(c.Param("id"))
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"secret": secret})
}
