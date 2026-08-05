// 页面传包构建:zip 上传 → 解压校验 → docker CLI build → push 到本机内嵌
// registry(127.0.0.1:<port>,loopback 免 HTTPS)→ 清理。参考 build-api.go
// 的任务状态机模式,进度与步骤日志供页面轮询。
package registry

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 构建任务状态。
type BuildStatus string

const (
	BsPending    BuildStatus = "PENDING"
	BsExtracting BuildStatus = "EXTRACTING"
	BsBuilding   BuildStatus = "BUILDING"
	BsPushing    BuildStatus = "PUSHING"
	BsCleaning   BuildStatus = "CLEANING"
	BsSuccess    BuildStatus = "SUCCESS"
	BsFailed     BuildStatus = "FAILED"
)

var statusProgress = map[BuildStatus]int{
	BsPending: 5, BsExtracting: 10, BsBuilding: 30, BsPushing: 75,
	BsCleaning: 95, BsSuccess: 100, BsFailed: -1,
}

// BuildTask 一次构建任务。
type BuildTask struct {
	ID         string      `json:"id"`
	Status     BuildStatus `json:"status"`
	Progress   int         `json:"progress"` // -1 = 失败
	Image      string      `json:"image"`    // 完整引用(127.0.0.1:<port>/<name>:<tag>)
	Name       string      `json:"name"`
	Tag        string      `json:"tag"`
	Logs       []string    `json:"logs"`
	Error      string      `json:"error,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
}

// buildTable 内存任务表(保留最近 buildKeep 条)。
type buildTable struct {
	mu   sync.RWMutex
	list []*BuildTask // 新在后
}

const buildKeep = 100

func newBuildTable() *buildTable { return &buildTable{} }

func (t *buildTable) add(task *BuildTask) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.list = append(t.list, task)
	if len(t.list) > buildKeep {
		t.list = t.list[len(t.list)-buildKeep:]
	}
}

func (t *buildTable) get(id string) *BuildTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, task := range t.list {
		if task.ID == id {
			cp := *task
			cp.Logs = append([]string(nil), task.Logs...)
			return &cp
		}
	}
	return nil
}

func (t *buildTable) listRecent(limit int) []*BuildTask {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*BuildTask, 0, len(t.list))
	for i := len(t.list) - 1; i >= 0 && len(out) < limit; i-- {
		cp := *t.list[i]
		cp.Logs = nil // 列表不带日志
		out = append(out, &cp)
	}
	return out
}

// runner 命令执行抽象(测试可注入 fake)。
type runner interface {
	Run(ctx context.Context, dir string, logf func(string, ...any), name string, args ...string) error
	LookPath(name string) bool
}

// cliRunner 真实 docker CLI 执行器。
type cliRunner struct{}

func (cliRunner) LookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (cliRunner) Run(ctx context.Context, dir string, logf func(string, ...any), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			logf("%s", line)
		}
	}
	if err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// --- 镜像名/标签校验(进 docker 命令行,防注入) ---

func validImageName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" {
			return false
		}
		for _, c := range seg {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
				return false
			}
		}
	}
	return true
}

func validImageTag(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			(i > 0 && (c == '_' || c == '.' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// --- 构建流程 ---

// SubmitBuild 接收 zip(已落盘的临时文件路径),登记任务并启动后台构建。
// dockerfile 相对路径默认 Dockerfile。
func (s *Service) SubmitBuild(zipPath, name, tag, dockerfile string) (*BuildTask, error) {
	if !validImageName(name) {
		return nil, fmt.Errorf("镜像名仅允许小写字母/数字/_.-/(多级)")
	}
	if !validImageTag(tag) {
		return nil, fmt.Errorf("tag 仅允许字母数字开头 + _.-")
	}
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if strings.Contains(dockerfile, "..") || filepath.IsAbs(dockerfile) {
		return nil, fmt.Errorf("dockerfile 必须是相对路径")
	}
	task := &BuildTask{
		ID: randHex(12), Status: BsPending, Progress: statusProgress[BsPending],
		Name: name, Tag: tag, CreatedAt: time.Now().UTC(),
	}
	s.builds.add(task)
	go s.runBuild(task, zipPath, dockerfile)
	return task, nil
}

// GetBuild 查询任务。
func (s *Service) GetBuild(id string) *BuildTask { return s.builds.get(id) }

// ListBuilds 最近任务(新在前,不带日志)。
func (s *Service) ListBuilds(limit int) []*BuildTask { return s.builds.listRecent(limit) }

func (s *Service) setStatus(t *BuildTask, st BuildStatus, errMsg string) {
	s.builds.mu.Lock()
	defer s.builds.mu.Unlock()
	for _, x := range s.builds.list {
		if x.ID == t.ID {
			x.Status = st
			x.Progress = statusProgress[st]
			if errMsg != "" {
				x.Error = errMsg
			}
			if st == BsSuccess || st == BsFailed {
				now := time.Now().UTC()
				x.FinishedAt = &now
			}
			*t = *x // 同步回调用副本
			return
		}
	}
}

func (s *Service) logf(t *BuildTask, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.builds.mu.Lock()
	for _, x := range s.builds.list {
		if x.ID == t.ID {
			x.Logs = append(x.Logs, line)
			if len(x.Logs) > 2000 {
				x.Logs = x.Logs[len(x.Logs)-2000:]
			}
		}
	}
	s.builds.mu.Unlock()
}

// runBuild 后台构建主流程。
func (s *Service) runBuild(t *BuildTask, zipPath, dockerfile string) {
	rn := s.runner()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	logf := func(f string, a ...any) { s.logf(t, f, a...) }

	workDir := filepath.Join(s.cfg.Storage, "_builds", t.ID)
	image := fmt.Sprintf("127.0.0.1:%s/%s:%s", s.port, t.Name, t.Tag)

	fail := func(step string, err error) {
		logf("✗ %s: %v", step, err)
		os.RemoveAll(workDir)
		s.setStatus(t, BsFailed, fmt.Sprintf("%s: %v", step, err))
	}

	// EXTRACTING
	s.setStatus(t, BsExtracting, "")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fail("创建工作目录", err)
		return
	}
	buildDir, err := extractZip(zipPath, workDir, dockerfile, logf)
	os.Remove(zipPath)
	if err != nil {
		fail("解压校验", err)
		return
	}
	logf("✓ 解压完成,构建上下文: %s", buildDir)

	// BUILDING
	s.setStatus(t, BsBuilding, "")
	logf("docker build -t %s -f %s .", image, dockerfile)
	if err := rn.Run(ctx, buildDir, logf, "docker", "build", "-t", image, "-f", dockerfile, "."); err != nil {
		fail("docker build", err)
		return
	}
	logf("✓ 构建成功: %s", image)

	// PUSHING(本机内嵌 registry;loopback 免 HTTPS)
	s.setStatus(t, BsPushing, "")
	if err := rn.Run(ctx, "", logf, "docker", "push", image); err != nil {
		fail("docker push", err)
		return
	}
	logf("✓ 推送成功")

	// CLEANING
	s.setStatus(t, BsCleaning, "")
	os.RemoveAll(workDir)
	_ = rn.Run(ctx, "", func(string, ...any) {}, "docker", "rmi", "-f", image)
	logf("✓ 临时文件已清理")

	// SUCCESS
	s.builds.mu.Lock()
	for _, x := range s.builds.list {
		if x.ID == t.ID {
			x.Image = image
		}
	}
	s.builds.mu.Unlock()
	s.setStatus(t, BsSuccess, "")
	logf("拉取命令: docker pull %s:%s/%s:%s", s.cfg.Hostname, s.port, t.Name, t.Tag)

	// 保留策略
	if s.cfg.RetentionPerRepo > 0 {
		trimmed, swept := s.st.ApplyRetention(s.cfg.RetentionPerRepo)
		if trimmed > 0 || swept > 0 {
			logf("保留策略: 裁剪 %d 个 tag,清理 %d 个无引用 blob", trimmed, swept)
		}
	}
}

// runner 返回命令执行器(默认 docker CLI;测试注入 fake)。
func (s *Service) runner() runner {
	if s.testRunner != nil {
		return s.testRunner
	}
	return cliRunner{}
}

// DockerAvailable 探测管理端 docker CLI。
func (s *Service) DockerAvailable() bool { return s.runner().LookPath("docker") }

// DockerLogin 启动时用 builder 账号登录本机内嵌 registry(供后续 push)。
func (s *Service) DockerLogin(ctx context.Context) error {
	if s.cfg.Builder.Username == "" {
		return nil
	}
	if !s.DockerAvailable() {
		return fmt.Errorf("docker CLI 不可用")
	}
	logf := func(string, ...any) {}
	return s.runner().Run(ctx, "", logf, "docker", "login", "127.0.0.1:"+s.port,
		"-u", s.cfg.Builder.Username, "-p", s.cfg.Builder.Password)
}

// extractZip 解压 zip 到 workDir(防 zip-slip),返回 Dockerfile 所在目录。
func extractZip(zipPath, workDir, dockerfile string, logf func(string, ...any)) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("无法打开 zip: %w", err)
	}
	defer r.Close()

	found := false
	for _, f := range r.File {
		// zip-slip 防护:拒绝绝对路径与 ..
		name := filepath.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return "", fmt.Errorf("非法路径条目: %s", f.Name)
		}
		target := filepath.Join(workDir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		src, err := f.Open()
		if err != nil {
			return "", err
		}
		dst, err := os.Create(target)
		if err != nil {
			src.Close()
			return "", err
		}
		_, copyErr := io.Copy(dst, src)
		dst.Close()
		src.Close()
		if copyErr != nil {
			return "", copyErr
		}
		// 脚本可执行权限(entrypoint 等)
		if strings.HasSuffix(name, ".sh") {
			_ = os.Chmod(target, 0o755)
		}
		if name == filepath.ToSlash(dockerfile) || filepath.Base(name) == dockerfile {
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("zip 内未找到 %s", dockerfile)
	}

	// 构建上下文 = Dockerfile 所在目录(优先精确匹配 dockerfile 路径)
	buildDir := workDir
	exact := filepath.Join(workDir, filepath.FromSlash(dockerfile))
	if _, err := os.Stat(exact); err == nil {
		return filepath.Dir(exact), nil
	}
	_ = filepath.Walk(workDir, func(path string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && fi.Name() == filepath.Base(dockerfile) {
			buildDir = filepath.Dir(path)
			return filepath.SkipAll
		}
		return nil
	})
	return buildDir, nil
}
