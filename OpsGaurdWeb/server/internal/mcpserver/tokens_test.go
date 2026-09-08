// token 运营与用量的集成测试（真实 LevelDB，临时目录）。
package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTokenTestHandler(t *testing.T, seeds []TokenConfig) (*Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h := New(Deps{
		Store:  st,
		Config: Config{Enabled: true, Tokens: seeds},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, st
}

func TestRuntimeTokenLifecycle(t *testing.T) {
	h, _ := newTokenTestHandler(t, []TokenConfig{{Name: "seed", Secret: "seed-secret", Scope: "write"}})

	// 静态种子镜像进库
	views, err := h.ListTokens()
	if err != nil || len(views) != 1 || views[0].Name != "seed" || !views[0].Static {
		t.Fatalf("static seed not mirrored: %+v err=%v", views, err)
	}
	// 静态种子仍可鉴权
	if _, ok := h.auth.authenticate("seed-secret"); !ok {
		t.Fatal("seed token should authenticate")
	}

	// 新建运行时 token：secret 一次性返回
	view, secret, err := h.CreateToken("zcode", "write")
	if err != nil || secret == "" || view.Scope != "write" {
		t.Fatalf("create token failed: view=%+v secret=%q err=%v", view, secret, err)
	}
	if _, ok := h.auth.authenticate(secret); !ok {
		t.Fatal("runtime token should authenticate right after create (hot effect)")
	}
	id, _ := h.auth.authenticate(secret)
	if !id.canWrite() {
		t.Fatal("write scope should canWrite")
	}

	// 重名拒绝
	if _, _, err := h.CreateToken("zcode", "read"); err == nil {
		t.Fatal("duplicate name must fail")
	}
	// 非法名拒绝
	if _, _, err := h.CreateToken("Bad Name", "read"); err == nil {
		t.Fatal("invalid name must fail")
	}

	// 禁用热生效
	if err := h.SetTokenEnabled("zcode", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := h.auth.authenticate(secret); ok {
		t.Fatal("disabled token must not authenticate")
	}
	if err := h.SetTokenEnabled("zcode", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, ok := h.auth.authenticate(secret); !ok {
		t.Fatal("re-enabled token should authenticate")
	}

	// 删除后失效；静态种子删除被拒
	if err := h.DeleteToken("seed"); err == nil {
		t.Fatal("deleting static seed must be refused")
	}
	if err := h.DeleteToken("zcode"); err != nil {
		t.Fatalf("delete runtime token: %v", err)
	}
	if _, ok := h.auth.authenticate(secret); ok {
		t.Fatal("deleted token must not authenticate")
	}
	// 静态种子名保留：同名新建被拒
	if _, _, err := h.CreateToken("seed", "read"); err == nil {
		t.Fatal("creating token with a static seed name must be refused")
	}
	// 静态种子可禁用
	if err := h.SetTokenEnabled("seed", false); err != nil {
		t.Fatalf("disable seed: %v", err)
	}
	if _, ok := h.auth.authenticate("seed-secret"); ok {
		t.Fatal("disabled static seed must not authenticate")
	}
}

func TestUsageFlushAndSummary(t *testing.T) {
	h, st := newTokenTestHandler(t, []TokenConfig{{Name: "t", Secret: "s", Scope: "read"}})

	day := store.UsageDay(time.Now(), bizLocation())
	h.usage.record(day, "zcode", "project_list")
	h.usage.record(day, "zcode", "project_list")
	h.usage.record(day, "other", "service_logs")
	h.flushUsage()

	sum := h.Usage(7)
	if len(sum.Rows) == 0 {
		t.Fatal("usage rows empty after flush")
	}
	total := int64(0)
	for _, r := range sum.Rows {
		total += r.Calls
	}
	if total != 3 {
		t.Fatalf("usage calls = %d, want 3: %+v", total, sum.Rows)
	}
	// 再次读取（从库），数字应稳定
	if err := st.AddMCPUsage(day, map[string]int64{"x|y": 5}); err != nil {
		t.Fatal(err)
	}
	sum = h.Usage(7)
	var xy int64
	for _, r := range sum.Rows {
		if r.Actor == "x" && r.Tool == "y" {
			xy = r.Calls
		}
	}
	if xy != 5 {
		t.Fatalf("external usage row missing: %+v", sum.Rows)
	}
}

func TestExecGuard(t *testing.T) {
	newH := func(execEnabled bool) *Handler {
		return &Handler{
			auth: newAuthorizer([]TokenConfig{{Name: "ops", Secret: "s", Scope: "exec"}}),
			deps: Deps{Config: Config{ExecEnabled: execEnabled}},
		}
	}

	// 开关关闭：即使 exec token 也拒绝
	h := newH(false)
	ctx := withIdentity(context.Background(), &identity{Name: "ops", Scope: ScopeExec})
	if err := h.execGuard(ctx, "service_exec"); err == nil {
		t.Fatal("exec with switch off must be rejected")
	} else if !strings.Contains(err.Error(), "exec_enabled") {
		t.Fatalf("error should point at exec_enabled: %v", err)
	}

	// 开关开 + exec token：通过
	h = newH(true)
	if err := h.execGuard(ctx, "service_exec"); err != nil {
		t.Fatalf("exec token with switch on should pass: %v", err)
	}

	// write/read token：拒绝且可解释
	ctxW := withIdentity(context.Background(), &identity{Name: "w", Scope: ScopeWrite})
	if err := h.execGuard(ctxW, "node_exec"); err == nil || !strings.Contains(err.Error(), "scope=exec") {
		t.Fatalf("write token must be rejected with exec-scope hint: %v", err)
	}

	// canWrite 语义：exec 含 write
	if !(&identity{Scope: ScopeExec}).canWrite() {
		t.Fatal("exec scope should imply write")
	}
}

func TestClusterURLAllowlist(t *testing.T) {
	mk := func(cidrs ...string) *Handler {
		return &Handler{deps: Deps{Config: Config{ClusterURLAllowCIDRs: cidrs}}}
	}

	// 空白名单 = 不限制
	if err := mk().checkClusterURLAllowed("10.9.9.9:9080"); err != nil {
		t.Fatalf("empty allowlist should allow: %v", err)
	}
	// 命中
	h := mk("10.0.0.0/8", "192.168.0.0/16")
	if err := h.checkClusterURLAllowed("10.0.1.5:9080"); err != nil {
		t.Fatalf("in-range host should pass: %v", err)
	}
	if err := h.checkClusterURLAllowed("http://192.168.1.2:8080"); err != nil {
		t.Fatalf("URL form should parse and pass: %v", err)
	}
	// 不命中
	if err := h.checkClusterURLAllowed("172.16.0.1:9080"); err == nil {
		t.Fatal("out-of-range host must be rejected")
	}
	// 非法网段配置：报配置错误而非放行
	if err := mk("not-a-cidr").checkClusterURLAllowed("10.0.1.5:9080"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("bad cidr entry must fail loudly: %v", err)
	}
}
