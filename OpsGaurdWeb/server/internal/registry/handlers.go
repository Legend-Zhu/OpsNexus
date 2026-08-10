// OCI 分布协议 /v2 HTTP 处理:pull(GET/HEAD manifests+blobs)、
// push(POST/PATCH/PUT uploads + PUT manifest)、catalog、tags/list、删除。
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Config 内嵌 registry 配置(server.yaml registry 块)。
type Config struct {
	Enabled          bool              `yaml:"enabled" json:"enabled"`
	Hostname         string            `yaml:"hostname" json:"hostname"`                     // 统一寻址主机名(双网段 hosts 解析)
	Storage          string            `yaml:"storage" json:"storage"`                       // 默认 ./data/registry
	RetentionPerRepo int               `yaml:"retention_per_repo" json:"retention_per_repo"` // 每仓库保留 tag 数,0=不裁剪
	MaxUploadMB      int               `yaml:"max_upload_mb" json:"max_upload_mb"`           // 构建 zip 上限,默认 500
	Users            map[string]string `yaml:"users" json:"-"`                               // /v2 basic auth(bcrypt 或 {PLAIN})
	Builder          BuilderAccount    `yaml:"builder" json:"-"`                             // 管理端构建 push 账号
	Relay            RelayAccount      `yaml:"relay" json:"-"`                               // 集群隧道中继拉取凭据
}

// RelayAccount 隧道中继(local registry relay)配置:集群内 dockerd 经 gRPC
// 反向隧道拉取本仓库镜像时,由 server 侧用该账号请求本机 /v2(集群侧 dockerd
// 匿名访问本地中继端点)。缺省 enabled=true(仅在 registry 启用时有意义);
// registry.users 为空(匿名仓库)时账号无需配置。
type RelayAccount struct {
	Enabled  *bool  `yaml:"enabled" json:"enabled"`
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
}

// RelayOn 报告 relay 是否启用(缺省开启)。
func (a RelayAccount) RelayOn() bool { return a.Enabled == nil || *a.Enabled }

// BuilderAccount 管理端构建后 push 用的账号(启动时 docker login 一次)。
type BuilderAccount struct {
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
}

// Service 内嵌镜像仓库服务。
type Service struct {
	st     *Store
	cfg    Config
	port   string // server 监听端口(构建 push 目标 127.0.0.1:<port>)
	builds *buildTable
	// testRunner 测试注入的命令执行器(空 = 真实 docker CLI)。
	testRunner runner
}

// NewService 创建 registry 服务。
func NewService(cfg Config, serverPort string) (*Service, error) {
	st, err := NewStore(cfg.Storage)
	if err != nil {
		return nil, err
	}
	return &Service{st: st, cfg: cfg, port: serverPort, builds: newBuildTable()}, nil
}

// Store 暴露存储(测试/管理 API 用)。
func (s *Service) Store() *Store { return s.st }

// MaxUploadMB 构建包上传上限(MB)。
func (s *Service) MaxUploadMB() int { return s.cfg.MaxUploadMB }

// RepoView 镜像列表视图(管理 API)。
type RepoView struct {
	Name string    `json:"name"`
	Tags []TagInfo `json:"tags"`
}

// Info 服务信息(管理 API /info)。
func (s *Service) Info() gin.H {
	return gin.H{
		"enabled":            true,
		"hostname":           s.cfg.Hostname,
		"port":               s.port,
		"docker_available":   s.DockerAvailable(),
		"retention_per_repo": s.cfg.RetentionPerRepo,
		"max_upload_mb":      s.cfg.MaxUploadMB,
		"auth_enabled":       len(s.cfg.Users) > 0,
	}
}

// AuthMiddleware /v2 的 basic auth(未配 users 则放行)。
func (s *Service) AuthMiddleware() gin.HandlerFunc { return basicAuth(s.cfg.Users) }

// V2 处理 /v2/* 全部请求(路由:gin Any("/v2/*rest"))。
func (s *Service) V2(c *gin.Context) {
	rest := strings.TrimPrefix(c.Param("rest"), "/")
	seg := strings.Split(rest, "/")

	// GET /v2/ 探活(docker login 会打这里)
	if rest == "" {
		c.Header("Docker-Distribution-API-Version", "registry/2.0")
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	// GET /v2/_catalog
	if rest == "_catalog" && c.Request.Method == http.MethodGet {
		c.JSON(http.StatusOK, gin.H{"repositories": s.st.Catalog()})
		return
	}

	// <name>/blobs/... | <name>/manifests/... | <name>/tags/list
	name, kind, tail := splitV2Path(seg)
	if name == "" {
		ociError(c, http.StatusNotFound, "NAME_UNKNOWN", "invalid path")
		return
	}
	switch kind {
	case "blobs":
		s.handleBlobs(c, name, tail)
	case "manifests":
		s.handleManifests(c, name, tail)
	case "tags":
		if c.Request.Method == http.MethodGet && tail == "list" {
			s.handleTagsList(c, name)
			return
		}
		ociError(c, http.StatusNotFound, "NOT_FOUND", "unknown path")
	default:
		ociError(c, http.StatusNotFound, "NOT_FOUND", "unknown path")
	}
}

// splitV2Path 把 <name>/<kind>/<tail> 拆开;name 允许多级(ops/myapp)。
func splitV2Path(seg []string) (name, kind, tail string) {
	for i, p := range seg {
		if p == "blobs" || p == "manifests" || p == "tags" {
			name = strings.Join(seg[:i], "/")
			kind = p
			tail = strings.Join(seg[i+1:], "/")
			return name, kind, tail
		}
	}
	return "", "", ""
}

// ---- blobs ----

func (s *Service) handleBlobs(c *gin.Context, name, tail string) {
	if !validName(name) {
		ociError(c, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
		return
	}
	// 上传会话:/blobs/uploads/ 或 /blobs/uploads/<uuid>
	if up, ok := strings.CutPrefix(tail, "uploads"); ok {
		uuid := strings.TrimPrefix(up, "/")
		s.handleUpload(c, name, uuid)
		return
	}
	digest := tail
	switch c.Request.Method {
	case http.MethodHead:
		size, err := s.st.BlobSize(digest)
		if err != nil {
			writeStoreErr(c, err)
			return
		}
		c.Header("Docker-Content-Digest", digest)
		c.Header("Content-Length", strconv.FormatInt(size, 10))
		c.Status(http.StatusOK)
	case http.MethodGet:
		r, size, err := s.st.OpenBlob(digest)
		if err != nil {
			writeStoreErr(c, err)
			return
		}
		defer r.Close()
		c.Header("Docker-Content-Digest", digest)
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Length", strconv.FormatInt(size, 10))
		io.Copy(c.Writer, r)
	default:
		ociError(c, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (s *Service) handleUpload(c *gin.Context, name, uuid string) {
	switch c.Request.Method {
	case http.MethodPost:
		// monolithic: POST /blobs/uploads/?digest=sha256:...(body 即 blob)
		if dg := c.Query("digest"); dg != "" {
			if err := s.st.putBlob(dg, mustReadAll(c)); err != nil {
				writeStoreErr(c, err)
				return
			}
			c.Header("Docker-Content-Digest", dg)
			c.Header("Location", "/v2/"+name+"/blobs/"+dg)
			c.Status(http.StatusCreated)
			return
		}
		id := s.st.StartUpload()
		c.Header("Docker-Upload-UUID", id)
		c.Header("Location", "/v2/"+name+"/blobs/uploads/"+id)
		c.Header("Range", "0-0")
		c.Status(http.StatusAccepted)
	case http.MethodPatch:
		size, err := s.st.AppendUpload(uuid, c.Request.Body)
		if err != nil {
			writeStoreErr(c, err)
			return
		}
		c.Header("Docker-Upload-UUID", uuid)
		c.Header("Location", "/v2/"+name+"/blobs/uploads/"+uuid)
		c.Header("Range", "0-"+strconv.FormatInt(size-1, 10))
		c.Status(http.StatusAccepted)
	case http.MethodPut:
		dg := c.Query("digest")
		if err := s.st.FinishUpload(uuid, dg, c.Request.Body); err != nil {
			writeStoreErr(c, err)
			return
		}
		c.Header("Docker-Content-Digest", dg)
		c.Header("Location", "/v2/"+name+"/blobs/"+dg)
		c.Status(http.StatusCreated)
	case http.MethodDelete:
		s.st.CancelUpload(uuid)
		c.Status(http.StatusNoContent)
	default:
		ociError(c, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// ---- manifests ----

func (s *Service) handleManifests(c *gin.Context, name, ref string) {
	if !validName(name) {
		ociError(c, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
		return
	}
	switch c.Request.Method {
	case http.MethodPut:
		data, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20)) // manifest ≤4MB
		if err != nil {
			ociError(c, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		ct := c.ContentType()
		if ct == "" {
			ct = "application/vnd.docker.distribution.manifest.v2+json"
		}
		digest, err := s.st.PutManifest(name, ref, ct, data)
		if err != nil {
			writeStoreErr(c, err)
			return
		}
		// 保留策略 + GC(每次写入后)
		if s.cfg.RetentionPerRepo > 0 {
			s.st.ApplyRetention(s.cfg.RetentionPerRepo)
		}
		c.Header("Docker-Content-Digest", digest)
		c.Header("Location", "/v2/"+name+"/manifests/"+digest)
		c.Status(http.StatusCreated)
	case http.MethodGet, http.MethodHead:
		ctype, data, err := s.st.GetManifest(name, ref)
		if err != nil {
			writeStoreErr(c, err)
			return
		}
		sum := digestOf(data)
		c.Header("Docker-Content-Digest", sum)
		if ctype != "" {
			c.Header("Content-Type", ctype)
		}
		c.Header("Content-Length", strconv.Itoa(len(data)))
		if c.Request.Method == http.MethodGet {
			c.Writer.Write(data)
		} else {
			c.Status(http.StatusOK)
		}
	case http.MethodDelete:
		if err := s.st.DeleteManifest(name, ref); err != nil {
			writeStoreErr(c, err)
			return
		}
		s.st.SweepBlobs()
		c.Status(http.StatusAccepted)
	default:
		ociError(c, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// ---- tags ----

func (s *Service) handleTagsList(c *gin.Context, name string) {
	tags, err := s.st.Tags(name)
	if err != nil {
		writeStoreErr(c, err)
		return
	}
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Tag)
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "tags": names})
}

// ---- 错误映射 ----

func writeStoreErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNameUnknown):
		ociError(c, http.StatusNotFound, "NAME_UNKNOWN", err.Error())
	case errors.Is(err, ErrBlobUnknown):
		ociError(c, http.StatusNotFound, "BLOB_UNKNOWN", err.Error())
	case errors.Is(err, ErrUploadUnknown):
		ociError(c, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", err.Error())
	case errors.Is(err, ErrDigestInvalid):
		ociError(c, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
	case errors.Is(err, ErrManifestInvalid):
		ociError(c, http.StatusNotFound, "MANIFEST_UNKNOWN", err.Error())
	default:
		ociError(c, http.StatusInternalServerError, "UNKNOWN", err.Error())
	}
}

// ociError OCI 标准错误体。
func ociError(c *gin.Context, status int, code, message string) {
	c.Header("Content-Type", "application/json")
	c.JSON(status, gin.H{"errors": []gin.H{{"code": code, "message": message}}})
}

func mustReadAll(c *gin.Context) []byte {
	data, _ := io.ReadAll(c.Request.Body)
	return data
}

// digestOf 计算内容的 sha256 digest（sha256:<hex>）。
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
