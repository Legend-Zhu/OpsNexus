// P6 handlers: notification channels/policies/records, alert rules, and
// auth (login/users). Alert-state changes (ingest/ack/recover) trigger
// notifications via the notify service.
package api

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/auth"
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
	Cluster    string           `json:"cluster" binding:"required"`
	Service    string           `json:"service" binding:"required"`
	Monitoring store.Monitoring `json:"monitoring"`
}

// ListAlertRules godoc: GET /api/v1/alertrules?cluster=
func (h *Handlers) ListAlertRules(c *gin.Context) {
	if h.ruleSvc == nil {
		fail(c, http.StatusServiceUnavailable, "alert rule service not initialized")
		return
	}
	items, err := h.ruleSvc.List(c.Query("cluster"))
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
// 删除规则并级联停止其已生效的监控；?force=true 在停止失败时强删（响应如实报告未停止）。
func (h *Handlers) DeleteAlertRule(c *gin.Context) {
	if h.ruleSvc == nil {
		fail(c, http.StatusServiceUnavailable, "alert rule service not initialized")
		return
	}
	force := c.Query("force") == "true"
	res, err := h.ruleSvc.Delete(c.Request.Context(), c.Param("cluster"), c.Param("service"), force)
	if err != nil {
		var nf alertrule.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		var sf alertrule.ErrStopFailed
		if errors.As(err, &sf) {
			fail(c, http.StatusBadGateway, err.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "delete rule: "+err.Error())
		return
	}
	ok(c, http.StatusOK, res)
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
	user, token, err := h.authSvc.LoginUser(req.Username, req.Password)
	if err != nil {
		fail(c, http.StatusUnauthorized, err.Error())
		return
	}
	// IdP 启用时，登录成功同步建立 IdP SSO 会话 cookie，
	// 使该用户随后跳转 /authorize 授权其他 RP 时免再登录（单点登录体验）。
	if h.idpSvc != nil {
		if _, err := h.idpSvc.EstablishSession(c, user, 0); err != nil {
			// 会话建立失败不影响登录本身，仅记日志（cookie 缺失会在 authorize 时跳登录）。
			_ = err
		}
	}
	ok(c, http.StatusOK, gin.H{"token": token})
}

// Me godoc: GET /api/v1/auth/me
func (h *Handlers) Me(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"username": c.GetString("username"), "role": c.GetString("role")})
}

// --- SSO (OIDC) ---

// LoginSSO godoc: GET /api/v1/auth/sso/login
// 发起 OIDC 授权码跳转；未配置 SSO 时返回 400。
func (h *Handlers) LoginSSO(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	url, err := h.authSvc.LoginURL()
	if err != nil {
		fail(c, http.StatusBadRequest, "sso login: "+err.Error())
		return
	}
	c.Redirect(http.StatusFound, url)
}

// SSOCallback godoc: GET /api/v1/auth/callback?state=&code=&error=
// OIDC 回调：校验 state → 换 token → find-or-create 用户 → 签发管理端 token，
// 302 到前端落地页，token 放 URL hash（#token=…）。
func (h *Handlers) SSOCallback(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	front := h.authSvc.SSOFrontendURL()
	if errMsg := c.Query("error"); errMsg != "" {
		c.Redirect(http.StatusFound, front+"#error="+url.QueryEscape(errMsg))
		return
	}
	token, err := h.authSvc.CompleteLogin(c.Request.Context(), c.Query("state"), c.Query("code"))
	if err != nil {
		c.Redirect(http.StatusFound, front+"#error="+url.QueryEscape(err.Error()))
		return
	}
	c.Redirect(http.StatusFound, front+"#token="+url.QueryEscape(token))
}

// SSOStatus godoc: GET /api/v1/auth/sso/status（公开，前端登录页据此展示 SSO 入口）
func (h *Handlers) SSOStatus(c *gin.Context) {
	sso := gin.H{"enabled": false}
	if h.authSvc != nil && h.authSvc.SSOEnabled() {
		sso = gin.H{"enabled": true, "issuer": h.authSvc.OIDC.Issuer, "frontendUrl": h.authSvc.SSOFrontendURL()}
	}
	ok(c, http.StatusOK, gin.H{
		"local": h.authSvc != nil,
		"sso":   sso,
	})
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

// passwordRequest 修改密码请求体（旧密码校验）。
type passwordRequest struct {
	OldPassword string `json:"old_password" binding:"required"`
	NewPassword string `json:"new_password" binding:"required"`
}

// ChangePassword godoc: PUT /api/v1/auth/password
// 当前登录用户修改自己的本地密码（SSO 用户无本地口令，返回 400 提示）。
func (h *Handlers) ChangePassword(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	var req passwordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := h.authSvc.ChangePassword(c.GetString("username"), req.OldPassword, req.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrBadOldPassword), errors.Is(err, auth.ErrNoLocalPassword), errors.Is(err, auth.ErrUserNotFound):
			// 均为请求侧校验失败，用 400（401 会触发前端全局登出，不适合旧密码输错）
			fail(c, http.StatusBadRequest, err.Error())
		default:
			fail(c, http.StatusInternalServerError, "change password: "+err.Error())
		}
		return
	}
	ok(c, http.StatusOK, gin.H{"changed": c.GetString("username")})
}

// resetPasswordRequest 管理员重置密码请求体（免旧密码）。
type resetPasswordRequest struct {
	NewPassword string `json:"new_password" binding:"required"`
}

// ResetPassword godoc: PUT /api/v1/users/:username/password（admin）
// 管理员重置指定用户的本地密码；对 SSO 用户重置即设置本地口令。
func (h *Handlers) ResetPassword(c *gin.Context) {
	if h.authSvc == nil {
		fail(c, http.StatusServiceUnavailable, "auth service not initialized")
		return
	}
	var req resetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := h.authSvc.ResetPassword(c.Param("username"), req.NewPassword); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			fail(c, http.StatusNotFound, err.Error())
			return
		}
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"reset": c.Param("username")})
}
