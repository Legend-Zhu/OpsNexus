// MLOps 模型运营 API（P3，方案 §8.2/§8.3）。provider/model 本体配置的
// 唯一事实来源仍是 /api/v1/ainexus/config（热重载）；本组只提供运营视图
// 与操作（启停/绑定/健康/预算），不重复提交整份 providers。
//
// 模型名可含 '/'，启停与健康测试用 JSON body / query 参数传
// provider+model，不用路径段。读操作普通认证；写操作挂 admin 守卫。
package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
)

// mlopsModelView 模型运营视图行。
type mlopsModelView struct {
	Provider    string `json:"provider"`
	Type        string `json:"type"`
	Model       string `json:"model"`
	DisplayName string `json:"display_name,omitempty"`
	// Enabled 配置启停（运营层）；Routed=当前网关是否实际路由（网关关闭时
	// 恒 false）。IsDefault=配置默认模型；EffectiveDefault=未指定请求实际
	// 落到的模型。
	Enabled          bool   `json:"enabled"`
	Routed           bool   `json:"routed"`
	IsDefault        bool   `json:"is_default"`
	EffectiveDefault string `json:"effective_default,omitempty"`
	// MonthCalls/MonthCostMinor 本月该模型用量（业务时区口径）。
	MonthCalls     int64 `json:"month_calls"`
	MonthCostMinor int64 `json:"month_cost_minor"`
	// Priced 是否已配单价（false = 调用只计 token）。
	Priced bool `json:"priced"`
}

// mlopsModelsView 模型运营总览。
type mlopsModelsView struct {
	GatewayEnabled   bool             `json:"gateway_enabled"`
	GatewayActive    bool             `json:"gateway_active"`
	DefaultModel     string           `json:"default_model,omitempty"`
	EffectiveDefault string           `json:"effective_default,omitempty"`
	Items            []mlopsModelView `json:"items"`
}

// ListMLOpsModels godoc: GET /api/v1/mlops/models
// 当前网关模型池运营视图（启停/路由状态/默认标记/本月用量/计价标记）。
func (h *Handlers) ListMLOpsModels(c *gin.Context) {
	if h.mlopsSvc == nil || h.AINexusRT == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	cfg := h.AINexusRT.Config()
	srv := h.AINexusRT.Server()

	monthByModel := h.mlopsSvc.MonthModelUsage() // 本月（业务时区）
	priced := map[string]bool{}
	if items, err := h.mlopsSvc.ListPricings(); err == nil {
		for _, p := range items {
			priced[p.Provider+"\x00"+p.Model] = true
		}
	}

	view := mlopsModelsView{
		GatewayEnabled: cfg.Enabled,
		GatewayActive:  srv != nil,
		DefaultModel:   cfg.DefaultModel,
	}
	if srv != nil {
		view.EffectiveDefault = srv.ResolveModel("")
	}
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			key := p.Name + "\x00" + m.Name
			usage := monthByModel[key]
			view.Items = append(view.Items, mlopsModelView{
				Provider:         p.Name,
				Type:             string(p.Type),
				Model:            m.Name,
				DisplayName:      m.DisplayName,
				Enabled:          m.Enabled,
				Routed:           srv != nil && srv.IsModelRoutable(m.Name),
				IsDefault:        cfg.DefaultModel == m.Name,
				EffectiveDefault: view.EffectiveDefault,
				MonthCalls:       usage.Calls,
				MonthCostMinor:   usage.CostMinor,
				Priced:           priced[key],
			})
		}
	}
	if view.Items == nil {
		view.Items = []mlopsModelView{}
	}
	ok(c, http.StatusOK, view)
}

type mlopsModelTarget struct {
	Provider string `json:"provider" binding:"required"`
	Model    string `json:"model" binding:"required"`
}

// SetMLOpsModelEnabled godoc: POST /api/v1/mlops/models/enable|disable（admin）
// 模型启停：经运行时服务全量热重载（构建校验成功才生效并持久化）；
// 禁用最后一个启用模型返回 409。
func (h *Handlers) setMLOpsModelEnabled(c *gin.Context, enabled bool) {
	if h.mlopsSvc == nil || h.AINexusRT == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsModelTarget
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := h.AINexusRT.SetModelEnabled(c.Request.Context(), req.Provider, req.Model, enabled); err != nil {
		fail(c, http.StatusConflict, err.Error())
		return
	}
	h.mlopsSvc.AuditModelToggle(c.GetString("username"), req.Provider, req.Model, enabled)
	ok(c, http.StatusOK, gin.H{"provider": req.Provider, "model": req.Model, "enabled": enabled})
}

// EnableMLOpsModel godoc: POST /api/v1/mlops/models/enable（admin）
func (h *Handlers) EnableMLOpsModel(c *gin.Context) { h.setMLOpsModelEnabled(c, true) }

// DisableMLOpsModel godoc: POST /api/v1/mlops/models/disable（admin）
func (h *Handlers) DisableMLOpsModel(c *gin.Context) { h.setMLOpsModelEnabled(c, false) }

// TestMLOpsModelHealth godoc: POST /api/v1/mlops/models/health（admin）
// 显式真实连通性测试（发送一次最小 LLM 请求，计量归属 scenario=health）；
// 结果写入最近缓存。GET 只读缓存不触发请求。
func (h *Handlers) TestMLOpsModelHealth(c *gin.Context) {
	if h.mlopsSvc == nil || h.AINexusRT == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsModelTarget
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	srv := h.AINexusRT.Server()
	if srv == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	start := time.Now()
	err := srv.TestModel(c.Request.Context(), req.Model)
	res := mlops.HealthResult{
		Provider:  req.Provider,
		Model:     req.Model,
		OK:        err == nil,
		LatencyMs: time.Since(start).Milliseconds(),
		TestedAt:  time.Now().UTC(),
	}
	if err != nil {
		res.Error = err.Error()
	}
	h.mlopsSvc.RecordHealth(res)
	if err != nil {
		ok(c, http.StatusOK, gin.H{"ok": false, "latency_ms": res.LatencyMs, "model": req.Model, "error": res.Error})
		return
	}
	ok(c, http.StatusOK, gin.H{"ok": true, "latency_ms": res.LatencyMs, "model": req.Model})
}

// ListMLOpsModelHealth godoc: GET /api/v1/mlops/models/health
// 最近健康测试缓存（不隐式触发真实请求）。
func (h *Handlers) ListMLOpsModelHealth(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	ok(c, http.StatusOK, gin.H{"items": h.mlopsSvc.HealthResults()})
}

// ListMLOpsBindings godoc: GET /api/v1/mlops/bindings
func (h *Handlers) ListMLOpsBindings(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.ListBindings()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list bindings: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

type mlopsBindingRequest struct {
	Model string `json:"model" binding:"required"`
}

// SaveMLOpsBinding godoc: PUT /api/v1/mlops/bindings/:scenario（admin）
func (h *Handlers) SaveMLOpsBinding(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsBindingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	b, err := h.mlopsSvc.SaveBinding(c.Param("scenario"), req.Model, c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, b)
}

// DeleteMLOpsBinding godoc: DELETE /api/v1/mlops/bindings/:scenario（admin）
func (h *Handlers) DeleteMLOpsBinding(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	if err := h.mlopsSvc.DeleteBinding(c.Param("scenario"), c.GetString("username")); err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("scenario")})
}

// ListMLOpsBudgets godoc: GET /api/v1/mlops/budgets
// 预算列表（含当月实际金额/使用率/已通知档位）。
func (h *Handlers) ListMLOpsBudgets(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.ListBudgets()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list budgets: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

type mlopsBudgetRequest struct {
	Month      string  `json:"month" binding:"required"` // yyyy-mm
	LimitMinor int64   `json:"limit_minor" binding:"required"`
	WarnAt     float64 `json:"warn_at"` // 0~1，如 0.8
}

// SaveMLOpsBudget godoc: POST /api/v1/mlops/budgets（admin）
// 新增或覆盖指定月份预算（重置通知档位）。
func (h *Handlers) SaveMLOpsBudget(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsBudgetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	b, err := h.mlopsSvc.SaveBudget(req.Month, req.LimitMinor, req.WarnAt, c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusCreated, b)
}

// DeleteMLOpsBudget godoc: DELETE /api/v1/mlops/budgets/:month（admin）
func (h *Handlers) DeleteMLOpsBudget(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	if err := h.mlopsSvc.DeleteBudget(c.Param("month"), c.GetString("username")); err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("month")})
}
