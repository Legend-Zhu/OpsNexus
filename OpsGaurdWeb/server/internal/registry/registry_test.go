package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "registry"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return st
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ---- 存储层 ----

func TestBlobUploadRoundtrip(t *testing.T) {
	st := tempStore(t)
	content := []byte("fake layer content")
	digest := sha256Digest(content)

	uuid := st.StartUpload()
	if _, err := st.AppendUpload(uuid, bytes.NewReader(content[:5])); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendUpload(uuid, bytes.NewReader(content[5:])); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishUpload(uuid, digest, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !st.HasBlob(digest) {
		t.Fatal("blob should exist")
	}
	r, size, err := st.OpenBlob(digest)
	if err != nil || size != int64(len(content)) {
		t.Fatalf("open: %v size=%d", err, size)
	}
	r.Close()

	// digest 不匹配 → 拒绝
	uuid2 := st.StartUpload()
	_, _ = st.AppendUpload(uuid2, bytes.NewReader(content))
	bad := "sha256:" + strings.Repeat("0", 64)
	if err := st.FinishUpload(uuid2, bad, nil); err == nil {
		t.Fatal("digest mismatch should fail")
	}
}

func TestManifestLifecycle(t *testing.T) {
	st := tempStore(t)
	cfgBlob := []byte(`{"architecture":"amd64"}`)
	cfgDigest := sha256Digest(cfgBlob)
	layerBlob := []byte("layer-tar-bytes")
	layerDigest := sha256Digest(layerBlob)
	for d, c := range map[string][]byte{cfgDigest: cfgBlob, layerDigest: layerBlob} {
		if err := st.putBlob(d, c); err != nil {
			t.Fatal(err)
		}
	}
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json",
		"config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, cfgDigest, layerDigest)
	mt := "application/vnd.docker.distribution.manifest.v2+json"

	digest, err := st.PutManifest("ops/myapp", "v1", mt, []byte(manifest))
	if err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	// 按 tag / digest 读
	if _, data, err := st.GetManifest("ops/myapp", "v1"); err != nil || !bytes.Equal(data, []byte(manifest)) {
		t.Fatalf("get by tag: %v", err)
	}
	if ct, _, err := st.GetManifest("ops/myapp", digest); err != nil || ct != mt {
		t.Fatalf("get by digest: %v ct=%q", err, ct)
	}
	// 引用缺失的 blob → 拒绝
	badManifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"%s"},"layers":[{"digest":"sha256:%s"}]}`,
		cfgDigest, strings.Repeat("f", 64))
	if _, err := st.PutManifest("ops/myapp", "v2", mt, []byte(badManifest)); err == nil {
		t.Fatal("manifest with missing blob should fail")
	}
	// catalog / tags / size
	if cats := st.Catalog(); len(cats) != 1 || cats[0] != "ops/myapp" {
		t.Fatalf("catalog: %v", cats)
	}
	tags, _ := st.Tags("ops/myapp")
	if len(tags) != 1 || tags[0].Tag != "v1" || tags[0].Size != int64(len(cfgBlob)+len(layerBlob)) {
		t.Fatalf("tags: %+v", tags)
	}
	// 删除 manifest → sweep 后 blob 清理
	if err := st.DeleteManifest("ops/myapp", digest); err != nil {
		t.Fatal(err)
	}
	if tags, _ := st.Tags("ops/myapp"); len(tags) != 0 {
		t.Fatalf("tags after delete: %v", tags)
	}
	if n := st.SweepBlobs(); n != 2 {
		t.Fatalf("swept %d blobs, want 2", n)
	}
	if st.HasBlob(cfgDigest) || st.HasBlob(layerDigest) {
		t.Fatal("orphan blobs should be swept")
	}
}

func TestRetention(t *testing.T) {
	st := tempStore(t)
	cfgDigest := sha256Digest([]byte("{}"))
	_ = st.putBlob(cfgDigest, []byte("{}"))
	mt := "application/vnd.docker.distribution.manifest.v2+json"
	// 5 个 tag,各自独立 layer blob
	for i := 0; i < 5; i++ {
		layer := []byte(fmt.Sprintf("layer-%d", i))
		ld := sha256Digest(layer)
		_ = st.putBlob(ld, layer)
		m := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, mt, cfgDigest, ld)
		if _, err := st.PutManifest("app", fmt.Sprintf("v%d", i), mt, []byte(m)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // 保证 tag 时间有序
	}
	trimmed, _ := st.ApplyRetention(2)
	if trimmed != 3 {
		t.Fatalf("trimmed=%d, want 3", trimmed)
	}
	tags, _ := st.Tags("app")
	if len(tags) != 2 || tags[0].Tag != "v4" || tags[1].Tag != "v3" {
		t.Fatalf("remaining tags: %+v", tags)
	}
	// 被裁剪 tag 的 layer blob 应被 GC;共享 config blob 保留
	if st.HasBlob(sha256Digest([]byte("layer-0"))) {
		t.Fatal("orphan layer should be swept")
	}
	if !st.HasBlob(cfgDigest) {
		t.Fatal("shared config blob should remain")
	}
}

// ---- HTTP 全流程(模拟 docker client) ----

func TestV2HTTPFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, err := NewService(Config{Storage: filepath.Join(t.TempDir(), "reg")}, "8090")
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Any("/v2/*rest", svc.V2)

	do := func(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 探活
	if w := do("GET", "/v2/", nil, nil); w.Code != 200 {
		t.Fatalf("ping: %d", w.Code)
	}
	// 推 blob(分片:POST → PATCH → PUT digest)
	layer := []byte("some layer bytes")
	ld := sha256Digest(layer)
	w := do("POST", "/v2/myapp/blobs/uploads/", nil, nil)
	if w.Code != 202 {
		t.Fatalf("start upload: %d %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	w = do("PATCH", loc, layer, nil)
	if w.Code != 202 {
		t.Fatalf("patch: %d", w.Code)
	}
	w = do("PUT", loc+"?digest="+ld, nil, nil)
	if w.Code != 201 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	// config blob(monolithic POST)
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	cd := sha256Digest(cfg)
	if w := do("POST", "/v2/myapp/blobs/uploads/?digest="+cd, cfg, nil); w.Code != 201 {
		t.Fatalf("monolithic: %d %s", w.Code, w.Body.String())
	}
	// 推 manifest
	mt := "application/vnd.docker.distribution.manifest.v2+json"
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`, mt, cd, ld)
	w = do("PUT", "/v2/myapp/manifests/v1.0", []byte(manifest), map[string]string{"Content-Type": mt})
	if w.Code != 201 {
		t.Fatalf("put manifest: %d %s", w.Code, w.Body.String())
	}
	// 拉 manifest(tag + digest)
	w = do("GET", "/v2/myapp/manifests/v1.0", nil, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != mt || w.Body.String() != manifest {
		t.Fatalf("get manifest: %d %v", w.Code, w.Header())
	}
	md := w.Header().Get("Docker-Content-Digest")
	if w := do("GET", "/v2/myapp/manifests/"+md, nil, nil); w.Code != 200 {
		t.Fatalf("get by digest: %d", w.Code)
	}
	// 拉 blob / HEAD blob
	if w := do("GET", "/v2/myapp/blobs/"+ld, nil, nil); w.Code != 200 || w.Body.String() != string(layer) {
		t.Fatalf("get blob: %d", w.Code)
	}
	if w := do("HEAD", "/v2/myapp/blobs/"+ld, nil, nil); w.Code != 200 {
		t.Fatalf("head blob: %d", w.Code)
	}
	// catalog / tags
	w = do("GET", "/v2/_catalog", nil, nil)
	if !strings.Contains(w.Body.String(), "myapp") {
		t.Fatalf("catalog: %s", w.Body.String())
	}
	w = do("GET", "/v2/myapp/tags/list", nil, nil)
	if !strings.Contains(w.Body.String(), "v1.0") {
		t.Fatalf("tags: %s", w.Body.String())
	}
	// 删除 manifest → 再拉 404
	if w := do("DELETE", "/v2/myapp/manifests/"+md, nil, nil); w.Code != 202 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := do("GET", "/v2/myapp/manifests/v1.0", nil, nil); w.Code != 404 {
		t.Fatalf("get after delete: %d", w.Code)
	}
}

// ---- basic auth ----

func TestBasicAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hash, _ := bcryptHash("s3cret")
	users := map[string]string{"ops": hash, "dev": "{PLAIN}pass123"}

	mw := basicAuth(users)
	check := func(authHeader string) int {
		r := gin.New()
		r.Use(mw)
		r.GET("/v2/", func(c *gin.Context) { c.Status(200) })
		req := httptest.NewRequest("GET", "/v2/", nil)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if check("") != 401 {
		t.Fatal("no auth should 401")
	}
	if check(basicHdr("ops", "s3cret")) != 200 {
		t.Fatal("bcrypt user should pass")
	}
	if check(basicHdr("dev", "pass123")) != 200 {
		t.Fatal("plain user should pass")
	}
	if check(basicHdr("ops", "wrong")) != 401 {
		t.Fatal("wrong password should 401")
	}
	if check(basicHdr("nobody", "x")) != 401 {
		t.Fatal("unknown user should 401")
	}
	// 无 users → 放行
	open := basicAuth(nil)
	r := gin.New()
	r.Use(open)
	r.GET("/v2/", func(c *gin.Context) { c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/v2/", nil))
	if w.Code != 200 {
		t.Fatal("empty users should allow")
	}
}

func basicHdr(u, p string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p))
}

func bcryptHash(p string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(h), err
}

// ---- zip 解压 ----

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "ctx.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("app/Dockerfile")
	w.Write([]byte("FROM scratch"))
	w, _ = zw.Create("app/docker-entrypoint.sh")
	w.Write([]byte("#!/bin/sh"))
	w, _ = zw.Create("../evil") // zip-slip 条目
	zw.Close()
	f.Close()

	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	if _, err := extractZip(zipPath, filepath.Join(dir, "out"), "Dockerfile", logf); err == nil ||
		!strings.Contains(err.Error(), "非法路径") {
		t.Fatalf("zip-slip should be rejected: %v", err)
	}

	// 正常包:Dockerfile 在子目录 → context 取其目录
	zipPath2 := filepath.Join(dir, "ok.zip")
	f, _ = os.Create(zipPath2)
	zw = zip.NewWriter(f)
	w, _ = zw.Create("app/Dockerfile")
	w.Write([]byte("FROM scratch"))
	w, _ = zw.Create("app/run.sh")
	w.Write([]byte("#!/bin/sh"))
	zw.Close()
	f.Close()
	out := filepath.Join(dir, "out2")
	buildDir, err := extractZip(zipPath2, out, "Dockerfile", logf)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if filepath.Base(buildDir) != "app" {
		t.Fatalf("buildDir=%s, want .../app", buildDir)
	}
	if runtime.GOOS != "windows" {
		// Windows 无 Unix 可执行位,跳过断言(部署目标为 Linux)
		fi, err := os.Stat(filepath.Join(buildDir, "run.sh"))
		if err != nil || fi.Mode().Perm()&0o100 == 0 {
			t.Fatal(".sh should be executable")
		}
	}

	// 缺 Dockerfile → 报错
	zipPath3 := filepath.Join(dir, "bad.zip")
	f, _ = os.Create(zipPath3)
	zw = zip.NewWriter(f)
	w, _ = zw.Create("readme.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	if _, err := extractZip(zipPath3, filepath.Join(dir, "out3"), "Dockerfile", logf); err == nil {
		t.Fatal("missing Dockerfile should fail")
	}
}

// ---- 构建状态机(fake runner) ----

type fakeRunner struct{ failAt string }

func (f fakeRunner) LookPath(string) bool { return true }
func (f fakeRunner) Run(_ context.Context, _ string, logf func(string, ...any), name string, args ...string) error {
	logf("$ %s %v", name, args)
	if f.failAt != "" && len(args) > 0 && args[0] == f.failAt {
		return fmt.Errorf("exit 1")
	}
	return nil
}

func TestBuildFlowSuccess(t *testing.T) {
	svc, err := NewService(Config{Storage: filepath.Join(t.TempDir(), "reg"), Hostname: "registry.opsguard"}, "8090")
	if err != nil {
		t.Fatal(err)
	}
	svc.testRunner = fakeRunner{}

	zipPath := makeCtxZip(t)
	task, err := svc.SubmitBuild(zipPath, "ops/myapp", "v1", "")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	final := waitTask(t, svc, task.ID)
	if final.Status != BsSuccess {
		t.Fatalf("status=%s err=%s logs=%v", final.Status, final.Error, final.Logs)
	}
	if final.Image != "127.0.0.1:8090/ops/myapp:v1" {
		t.Fatalf("image=%s", final.Image)
	}
	// 名称校验
	if _, err := svc.SubmitBuild(makeCtxZip(t), "bad;name", "v1", ""); err == nil {
		t.Fatal("bad name should be rejected")
	}
}

func TestBuildFlowFailure(t *testing.T) {
	svc, _ := NewService(Config{Storage: filepath.Join(t.TempDir(), "reg")}, "8090")
	svc.testRunner = fakeRunner{failAt: "build"}
	task, _ := svc.SubmitBuild(makeCtxZip(t), "app", "v1", "")
	final := waitTask(t, svc, task.ID)
	if final.Status != BsFailed || final.Error == "" {
		t.Fatalf("status=%s err=%s", final.Status, final.Error)
	}
}

func makeCtxZip(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ctx.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("Dockerfile")
	w.Write([]byte("FROM scratch"))
	zw.Close()
	f.Close()
	return p
}

func waitTask(t *testing.T, svc *Service, id string) *BuildTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task := svc.GetBuild(id)
		if task != nil && (task.Status == BsSuccess || task.Status == BsFailed) {
			return task
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not finish in time")
	return nil
}
