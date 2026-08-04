// P6 handlers: notification channels/policies/records, alert rules, and
// auth (login/users). Alert-state changes (ingest/ack/recover) trigger
// notifications via the notify service.
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// --- 通知渠道 ---

// channelRequest 渠道创建/更新请求体。
type channelRequest struct {
	Type     string         `json:"type" binding:"required"` // feishu|sms|webhook
	Name     string         `json:"name" binding:"required"`
	Config   map[string]any `json:"config"`
	ViaProxy bool           `json:"via_proxy"`
	ProxyURL string         `json:"proxy_url"`
	Enabled  bool           `json:"enabled"`
}

// ListChannels godoc: GET /api/v1/notify/channels
func (h *Handlers) ListChannels(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	items, err := h.notifySvc.ListChannels()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list channels: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// CreateChannel godoc: POST /api/v1/notify/channels
func (h *Handlers) CreateChannel(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	var req channelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	ch, err := h.notifySvc.CreateChannel(store.ChannelType(req.Type), req.Name, req.Config, req.ViaProxy, req.ProxyURL, req.Enabled)
	if err != nil {
		var inv notify.ErrInvalid
		if errors.As(err, &inv) {
			fail(c, http.StatusBadRequest, inv.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "create channel: "+err.Error())
		return
	}
	ok(c, http.StatusCreated, ch)
}

// UpdateChannel godoc: PUT /api/v1/notify/channels/:id
func (h *Handlers) UpdateChannel(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	var req channelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	ch, err := h.notifySvc.UpdateChannel(c.Param("id"), req.Name, req.Config, req.ViaProxy, req.ProxyURL, req.Enabled)
	if err != nil {
		var nf notify.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		var inv notify.ErrInvalid
		if errors.As(err, &inv) {
			fail(c, http.StatusBadRequest, inv.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "update channel: "+err.Error())
		return
	}
	ok(c, http.StatusOK, ch)
}

// DeleteChannel godoc: DELETE /api/v1/notify/channels/:id
func (h *Handlers) DeleteChannel(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	if err := h.notifySvc.DeleteChannel(c.Param("id")); err != nil {
		var nf notify.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "delete channel: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// --- 通知策略 ---

// policyRequest 策略请求体。
type policyRequest struct {
	Level      string   `json:"level" binding:"required"`
	ChannelIDs []string `json:"channel_ids"`
	Receivers  []string `json:"receivers"`
}

// ListPolicies godoc: GET /api/v1/notify/policies
func (h *Handlers) ListPolicies(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	items, err := h.notifySvc.ListPolicies()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list policies: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// UpsertPolicy godoc: PUT /api/v1/notify/policies/:level
func (h *Handlers) UpsertPolicy(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	var req policyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p := &store.NotifyPolicy{Level: req.Level, ChannelIDs: req.ChannelIDs, Receivers: req.Receivers}
	if err := h.notifySvc.UpsertPolicy(p); err != nil {
		var nf notify.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		var inv notify.ErrInvalid
		if errors.As(err, &inv) {
			fail(c, http.StatusBadRequest, inv.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "upsert policy: "+err.Error())
		return
	}
	ok(c, http.StatusOK, p)
}

// --- 发送记录 ---

// ListNotifyRecords godoc: GET /api/v1/notify/records?limit=
func (h *Handlers) ListNotifyRecords(c *gin.Context) {
	if h.notifySvc == nil {
		fail(c, http.StatusServiceUnavailable, "notify service not initialized")
		return
	}
	items, err := h.notifySvc.Records(parseLimit(c.Query("limit"), 50))
	if err != nil {
		fail(c, http.StatusInternalServerError, "list records: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// --- 告警规则（Worker monitoring config 管理） ---

// ruleRequest 告警规则请求体。
type ruleRequest struct {
	Cluster    string          `json:"cluster" binding:"required"`
	Service    string          `json:"service" binding:"required"`
	Monitoring store.Monitoring `json:"monitoring"`
}

// ListAlertRules godoc: GET /api/v1/alertrules
func (h *Handlers) ListAlertRules(c *gin.Context) {
	if h.ruleSvc == nil {
		fail(c, http.StatusServiceUnavailable, "alert rule service not initialized")
		return
	}
	items, err := h.ruleSvc.List()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list rules: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// UpsertAlertRule godoc: PUT /api/v1/alertrules
func (h *Handlers) UpsertAlertRule(c *gin.Context) {
	if h.ruleSvc == nil {
		fail(c, http.StatusServiceUnavailable, "alert rule service not initialized")
		return
	}
	var req ruleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	r, err := h.ruleSvc.Upsert(&store.AlertRule{Cluster: req.Cluster, Service: req.Service, Monitoring: req.Monitoring})
	if err != nil {
		var inv alertrule.ErrInvalid
		if errors.As(err, &inv) {
			fail(c, http.StatusBadRequest, inv.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "upsert rule: "+err.Error())
		return
	}
	ok(c, http.StatusOK, r)
}

// ApplyAlertRule godoc: POST /api/v1/alertrules/apply
// 把规则下发到 Worker（合并 monitoring 块 → Update）。
func (h *Handlers) ApplyAlertRule(c *gin.Context) {
	if h.ruleSvc == nil {
		fail(c, http.StatusServiceUnavailable, "alert rule service not initialized")
		return
	}
	var req ruleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	r := &store.AlertRule{Cluster: req.Cluster, Service: req.Service, Monitoring: req.Monitoring}
	if err := h.ruleSvc.Apply(c.Request.Context(), r); err != nil {
		fail(c, http.StatusBadGateway, "apply rule: "+err.Error())
		return
	}
	ok(c, http.StatusOK, r)
}

// DeleteAlertRule godoc: DELETE /api/v1/alertrules/:cluster/:service
func (h *Handlers) DeleteAlertRule(c *gin.Context) {
	if h.ruleSvc == nil {
		fail(c, http.StatusServiceUnavailable, "alert rule service not initialized")
		return
	}
	if err := h.ruleSvc.Delete(c.Param("cluster"), c.Param("service")); err != nil {
		var nf alertrule.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "delete rule: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("cluster") + "/" + c.Param("service")})
}

// --- 认证 / 用户 ---

// loginRequest 登录请求体。
type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Login godoc: POST /api/v1/auth/login
func (h *Handlers) Login(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	token, err := h.authSvc.Login(req.Username, req.Password)
	if err != nil {
		fail(c, http.StatusUnauthorized, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"token": token})
}

// Me godoc: GET /api/v1/auth/me
func (h *Handlers) Me(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"username": c.GetString("username"), "role": c.GetString("role")})
}

// ListUsers godoc: GET /api/v1/users
func (h *Handlers) ListUsers(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	items, err := h.authSvc.ListUsers()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list users: "+err.Error())
		return
	}
	pub := make([]*store.User, 0, len(items))
	for _, u := range items {
		pub = append(pub, u.Public())
	}
	ok(c, http.StatusOK, gin.H{"items": pub})
}

// CreateUser godoc: POST /api/v1/users
func (h *Handlers) CreateUser(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
		Role     string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	role := req.Role
	if role == "" {
		role = "viewer"
	}
	u, err := h.authSvc.CreateUser(req.Username, req.Password, role, true)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusCreated, u.Public())
}
