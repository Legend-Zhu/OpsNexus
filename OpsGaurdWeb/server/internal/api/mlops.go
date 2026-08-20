// MLOps 运营层 API（P1：Prompt Hub）。读操作（列表/详情/预览）走普通
// 认证；写操作（创建/保存/激活/重置/删除）与审计查询挂 admin 守卫
// （router.go 按 h.MLOps() != nil 决定是否注册整组路由）。
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
)

// mlopsPromptMessageRequest 单条消息模板入参。
type mlopsPromptMessageRequest struct {
	Role     string `json:"role" binding:"required"`
	Template string `json:"template" binding:"required"`
}

type mlopsVariableRequest struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Note     string `json:"note,omitempty"`
}

type mlopsVersionRequest struct {
	Messages  []mlopsPromptMessageRequest `json:"messages" binding:"required"`
	Variables []mlopsVariableRequest      `json:"variables,omitempty"`
	Note      string                      `json:"note,omitempty"`
}

type mlopsSavePromptRequest struct {
	ExpectedActiveVersion int                `json:"expected_active_version,omitempty"`
	Activate              bool               `json:"activate"`
	Version               mlopsVersionRequest `json:"version"`
}

type mlopsCreatePromptRequest struct {
	Name    string              `json:"name" binding:"required"`
	Version mlopsVersionRequest `json:"version" binding:"required"`
}

type mlopsRenderPromptRequest struct {
	Version int             `json:"version,omitempty"` // 0 = active
	Data    json.RawMessage `json:"data,omitempty"`
}

type mlopsActivatePromptRequest struct {
	Version              int `json:"version" binding:"required"`
	ExpectedActiveVersion int `json:"expected_active_version,omitempty"`
}

// ListMLOpsPrompts godoc: GET /api/v1/mlops/prompts
// 提示词列表：内置场景（含未物化的默认视图）+ 自定义。
func (h *Handlers) ListMLOpsPrompts(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.List()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list prompts: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// GetMLOpsPrompt godoc: GET /api/v1/mlops/prompts/:id
func (h *Handlers) GetMLOpsPrompt(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	p, err := h.mlopsSvc.Get(c.Param("id"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, p)
}

// CreateMLOpsPrompt godoc: POST /api/v1/mlops/prompts（admin）
// 创建自定义提示词（scenario=custom）。
func (h *Handlers) CreateMLOpsPrompt(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsCreatePromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.mlopsSvc.Create(req.Name, toVersionInput(req.Version), c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusCreated, p)
}

// SaveMLOpsPromptVersion godoc: PUT /api/v1/mlops/prompts/:id（admin）
// 保存新版本（校验语法/变量/大小；可选同时激活；支持并发版本校验）。
func (h *Handlers) SaveMLOpsPromptVersion(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsSavePromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	in := mlops.SaveInput{
		ExpectedActiveVersion: req.ExpectedActiveVersion,
		Activate:              req.Activate,
		Version:               toVersionInput(req.Version),
	}
	p, err := h.mlopsSvc.SaveVersion(c.Param("id"), in, c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, p)
}

// RenderMLOpsPrompt godoc: POST /api/v1/mlops/prompts/:id/render
// 预览渲染（version=0 用 active；data 按场景数据结构解码）。
func (h *Handlers) RenderMLOpsPrompt(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsRenderPromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	msgs, err := h.mlopsSvc.Preview(c.Param("id"), req.Version, req.Data)
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"messages": msgs})
}

// ActivateMLOpsPrompt godoc: POST /api/v1/mlops/prompts/:id/activate（admin）
// 激活/回滚到指定版本。
func (h *Handlers) ActivateMLOpsPrompt(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	var req mlopsActivatePromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.mlopsSvc.Activate(c.Param("id"), req.Version, req.ExpectedActiveVersion, c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, p)
}

// ResetMLOpsPrompt godoc: POST /api/v1/mlops/prompts/:id/reset（admin）
// 内置场景恢复代码默认（保留用户版本历史）。
func (h *Handlers) ResetMLOpsPrompt(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	p, err := h.mlopsSvc.Reset(c.Param("id"), c.GetString("username"))
	if err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, p)
}

// DeleteMLOpsPrompt godoc: DELETE /api/v1/mlops/prompts/:id（admin）
// 删除自定义提示词（内置场景只可 reset）。
func (h *Handlers) DeleteMLOpsPrompt(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	if err := h.mlopsSvc.Delete(c.Param("id"), c.GetString("username")); err != nil {
		failMlopsErr(c, err)
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// ListMLOpsAudits godoc: GET /api/v1/mlops/audit?limit=（admin）
// 管理操作审计（最新在前；敏感内容只记录 hash 摘要）。
func (h *Handlers) ListMLOpsAudits(c *gin.Context) {
	if h.mlopsSvc == nil {
		fail(c, http.StatusServiceUnavailable, "mlops is not enabled in config")
		return
	}
	items, err := h.mlopsSvc.Audits(parseLimit(c.Query("limit"), 50))
	if err != nil {
		fail(c, http.StatusInternalServerError, "list audits: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// toVersionInput DTO -> 服务入参。
func toVersionInput(v mlopsVersionRequest) mlops.VersionInput {
	in := mlops.VersionInput{Note: v.Note}
	for _, m := range v.Messages {
		in.Messages = append(in.Messages, mlops.MessageInput{Role: m.Role, Template: m.Template})
	}
	for _, pv := range v.Variables {
		in.Variables = append(in.Variables, mlops.VariableInput{Name: pv.Name, Required: pv.Required, Note: pv.Note})
	}
	return in
}

// failMlopsErr 按错误类型映射 HTTP 状态码。
func failMlopsErr(c *gin.Context, err error) {
	var inv mlops.ErrInvalid
	if errors.As(err, &inv) {
		fail(c, http.StatusBadRequest, inv.Error())
		return
	}
	var nf mlops.ErrNotFound
	if errors.As(err, &nf) {
		fail(c, http.StatusNotFound, nf.Error())
		return
	}
	var cf mlops.ErrConflict
	if errors.As(err, &cf) {
		fail(c, http.StatusConflict, cf.Error())
		return
	}
	fail(c, http.StatusInternalServerError, err.Error())
}
