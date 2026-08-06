// 密钥管理 API（巡检 flow 拨测账号等 ${secret:name} 引用的存储）。
// 安全约束：列表/读取绝不返回密钥值；值只在写入时提交、执行时服务端解析。
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ListSecrets godoc: GET /api/v1/secrets
// 密钥名列表（不含值）。
func (h *Handlers) ListSecrets(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	items, err := h.clusters.Secrets()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list secrets: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// PutSecret godoc: PUT /api/v1/secrets/:name
// 写入（新增或覆盖）密钥。body: {"value": "..."}。
func (h *Handlers) PutSecret(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	name := c.Param("name")
	if name == "" {
		fail(c, http.StatusBadRequest, "name is required")
		return
	}
	var req struct {
		Value string `json:"value" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := h.clusters.SaveSecret(name, req.Value); err != nil {
		fail(c, http.StatusInternalServerError, "save secret: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"name": name})
}

// DeleteSecret godoc: DELETE /api/v1/secrets/:name
func (h *Handlers) DeleteSecret(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	if err := h.clusters.DeleteSecret(c.Param("name")); err != nil {
		fail(c, http.StatusInternalServerError, "delete secret: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("name")})
}
