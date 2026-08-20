// MLOps 费用 API（P2，方案 §8.3）。读操作（overview/trend/detail/
// operation 下钻/价格列表）走普通认证；价格写操作挂 admin 守卫。
// 查询限制：趋势 ≤92 天、明细 ≤400 天、分页 ≤500，防止无上限扫描。
package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// MLOpsCostsOverview godoc: GET /api/v1/mlops/costs/overview
// 今日/本月摘要 + 按模型/场景统计（业务时区口径）+ collector 状态。
func (h *Handlers) MLOpsCostsOverview(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	ov, err := h.mlopsSvc.CostsOverview()
	if err != nil {
		fail(c, http.StatusInternalServerError, "costs overview: "+err.Error())
		return
	}
	ok(c, http.StatusOK, ov)
}

// MLOpsCostsTrend godoc: GET /api/v1/mlops/costs/trend?from=&to=
// 日趋势（缺省最近 30 天；最多 92 天）。
func (h *Handlers) MLOpsCostsTrend(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.CostsTrend(c.Query("from"), c.Query("to"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// MLOpsCostsDetail godoc: GET /api/v1/mlops/costs/detail
// call 明细分页（最新在前）：from/to/provider/model/scenario/status/
// operation_id/before_seq/limit/offset。
func (h *Handlers) MLOpsCostsDetail(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	q := mlops.DetailQuery{
		FromDay:     c.Query("from"),
		ToDay:       c.Query("to"),
		Provider:    c.Query("provider"),
		Model:       c.Query("model"),
		Scenario:    c.Query("scenario"),
		Status:      c.Query("status"),
		OperationID: c.Query("operation_id"),
		Limit:       parseLimit(c.Query("limit"), 50),
	}
	if v, err := strconv.ParseUint(c.Query("before_seq"), 10, 64); err == nil {
		q.BeforeSeq = v
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		q.Offset = v
	}
	d, err := h.mlopsSvc.CostsDetail(q)
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, d)
}

// MLOpsCostsOperation godoc: GET /api/v1/mlops/costs/operations/:id
// 一次业务 operation 下的全部 provider call（时间序，Agent 多轮可见）。
func (h *Handlers) MLOpsCostsOperation(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.CostsOperation(c.Param("id"))
	if err != nil {
		fail(c, http.StatusInternalServerError, "costs operation: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items, "operation_id": c.Param("id")})
}

// mlopsPricingView 价格 DTO：定点微元 → 十进制字符串（元/百万 token）。
type mlopsPricingView struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Currency     string `json:"currency"`
	PriceInPerM  string `json:"price_in_per_m"`
	PriceOutPerM string `json:"price_out_per_m"`
	Version      string `json:"version"`
	UpdatedAt    string `json:"updated_at"`
	UpdatedBy    string `json:"updated_by,omitempty"`
	Note         string `json:"note,omitempty"`
}

func pricingView(p *store.MLPricing) mlopsPricingView {
	return mlopsPricingView{
		Provider:     p.Provider,
		Model:        p.Model,
		Currency:     p.Currency,
		PriceInPerM:  mlops.MicroToDecimal(p.PriceInPerMMicro),
		PriceOutPerM: mlops.MicroToDecimal(p.PriceOutPerMMicro),
		Version:      p.Version,
		UpdatedAt:    p.UpdatedAt.Format("2006-01-02 15:04:05"),
		UpdatedBy:    p.UpdatedBy,
		Note:         p.Note,
	}
}

// ListMLOpsPricing godoc: GET /api/v1/mlops/costs/pricing
func (h *Handlers) ListMLOpsPricing(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.ListPricings()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list pricing: "+err.Error())
		return
	}
	views := make([]mlopsPricingView, 0, len(items))
	for _, p := range items {
		views = append(views, pricingView(p))
	}
	ok(c, http.StatusOK, gin.H{"items": views})
}

type mlopsSavePricingRequest struct {
	Provider     string `json:"provider" binding:"required"`
	Model        string `json:"model" binding:"required"`
	PriceInPerM  string `json:"price_in_per_m" binding:"required"`
	PriceOutPerM string `json:"price_out_per_m" binding:"required"`
	Note         string `json:"note,omitempty"`
}

// SaveMLOpsPricing godoc: PUT /api/v1/mlops/costs/pricing（admin）
// 保存（覆盖）单价；立即影响之后的调用计价，不回溯历史。
func (h *Handlers) SaveMLOpsPricing(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsSavePricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.mlopsSvc.SavePricing(mlops.PricingInput{
		Provider:     req.Provider,
		Model:        req.Model,
		PriceInPerM:  req.PriceInPerM,
		PriceOutPerM: req.PriceOutPerM,
		Note:         req.Note,
	}, c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, pricingView(p))
}

// DeleteMLOpsPricing godoc: DELETE /api/v1/mlops/costs/pricing?provider=&model=（admin）
func (h *Handlers) DeleteMLOpsPricing(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	if err := h.mlopsSvc.DeletePricing(c.Query("provider"), c.Query("model"), c.GetString("username")); err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Query("provider") + "/" + c.Query("model")})
}
