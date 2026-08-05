// 内嵌镜像仓库的管理 API（页面用，会话认证）：构建提交/轮询、镜像列表、
// 删除 tag、服务信息。/v2 协议端点由 registry.Service.V2 直接挂载（basic auth）。
package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/registry"
)

// RegistryInfo godoc: GET /api/v1/registry/info
// 服务信息:启用状态/统一主机名/端口/docker 可用性/保留策略/是否启用认证。
func (h *Handlers) RegistryInfo(c *gin.Context) {
	if h.registrySvc == nil {
		fail(c, http.StatusServiceUnavailable, "registry is not enabled in config")
		return
	}
	info := h.registrySvc.Info()
	ok(c, http.StatusOK, info)
}

// SubmitRegistryBuild godoc: POST /api/v1/registry/builds
// 上传构建包(zip)并启动后台构建。支持 multipart(field=file)与
// application/octet-stream(参数走 query)两种模式。
func (h *Handlers) SubmitRegistryBuild(c *gin.Context) {
	if h.registrySvc == nil {
		fail(c, http.StatusServiceUnavailable, "registry is not enabled in config")
		return
	}
	if !h.registrySvc.DockerAvailable() {
		fail(c, http.StatusServiceUnavailable, "管理端未检测到 docker CLI，无法构建")
		return
	}

	var src io.Reader
	var name, tag, dockerfile string
	ct := strings.ToLower(c.ContentType())
	if strings.HasPrefix(ct, "application/octet-stream") {
		src = c.Request.Body
		name = c.Query("name")
		tag = c.Query("tag")
		dockerfile = c.Query("dockerfile")
	} else {
		f, header, err := c.Request.FormFile("file")
		if err != nil {
			fail(c, http.StatusBadRequest, "缺少上传文件(file)")
			return
		}
		defer f.Close()
		src = f
		name = c.PostForm("name")
		tag = c.PostForm("tag")
		dockerfile = c.PostForm("dockerfile")
		_ = header
	}
	if name == "" || tag == "" {
		fail(c, http.StatusBadRequest, "name 和 tag 不能为空")
		return
	}

	// 落临时文件(带上限),交给构建流程
	maxBytes := int64(h.registrySvc.MaxUploadMB()) << 20
	tmp, err := os.CreateTemp("", "opsguard-build-*.zip")
	if err != nil {
		fail(c, http.StatusInternalServerError, "create temp: "+err.Error())
		return
	}
	written, err := io.Copy(tmp, io.LimitReader(src, maxBytes+1))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		fail(c, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if written > maxBytes {
		os.Remove(tmp.Name())
		fail(c, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("上传包超过 %dMB 上限", h.registrySvc.MaxUploadMB()))
		return
	}

	task, err := h.registrySvc.SubmitBuild(tmp.Name(), name, tag, dockerfile)
	if err != nil {
		os.Remove(tmp.Name())
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, task)
}

// ListRegistryBuilds godoc: GET /api/v1/registry/builds?limit=
func (h *Handlers) ListRegistryBuilds(c *gin.Context) {
	if h.registrySvc == nil {
		fail(c, http.StatusServiceUnavailable, "registry is not enabled in config")
		return
	}
	ok(c, http.StatusOK, gin.H{"items": h.registrySvc.ListBuilds(20)})
}

// GetRegistryBuild godoc: GET /api/v1/registry/builds/:id
func (h *Handlers) GetRegistryBuild(c *gin.Context) {
	if h.registrySvc == nil {
		fail(c, http.StatusServiceUnavailable, "registry is not enabled in config")
		return
	}
	task := h.registrySvc.GetBuild(c.Param("id"))
	if task == nil {
		fail(c, http.StatusNotFound, "build task not found")
		return
	}
	ok(c, http.StatusOK, task)
}

// ListRegistryImages godoc: GET /api/v1/registry/images
// 镜像列表:repo + tags(大小/更新时间)。
func (h *Handlers) ListRegistryImages(c *gin.Context) {
	if h.registrySvc == nil {
		fail(c, http.StatusServiceUnavailable, "registry is not enabled in config")
		return
	}
	st := h.registrySvc.Store()
	var items []registry.RepoView
	for _, repo := range st.Catalog() {
		tags, err := st.Tags(repo)
		if err != nil {
			continue
		}
		items = append(items, registry.RepoView{Name: repo, Tags: tags})
	}
	if items == nil {
		items = []registry.RepoView{}
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// DeleteRegistryTag godoc: DELETE /api/v1/registry/images/*ref
// ref 形如 /ops/myapp/tags/v1(删除 tag;manifest 本体由 GC 清理)。
func (h *Handlers) DeleteRegistryTag(c *gin.Context) {
	if h.registrySvc == nil {
		fail(c, http.StatusServiceUnavailable, "registry is not enabled in config")
		return
	}
	ref := strings.TrimPrefix(c.Param("ref"), "/")
	idx := strings.LastIndex(ref, "/tags/")
	if idx <= 0 {
		fail(c, http.StatusBadRequest, "ref 应为 <name>/tags/<tag>")
		return
	}
	name, tag := ref[:idx], filepath.Base(ref[idx+6:])
	if !h.registrySvc.Store().DeleteTag(name, tag) {
		fail(c, http.StatusNotFound, "tag not found")
		return
	}
	h.registrySvc.Store().SweepBlobs()
	ok(c, http.StatusOK, gin.H{"deleted": name + ":" + tag})
}
