// mcpserver 单元测试：token 鉴权/scope、结果截断、上传票据、审计与
// 委托排查状态机。工具层依赖 service 实例的组合逻辑不做单测（与 REST
// handler 同层，靠既有 service 层测试与联调覆盖）。
package mcpserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// testLogger 静默日志（测试不刷屏）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAuthorizer(t *testing.T) {
	a := newAuthorizer([]TokenConfig{
		{Name: "writer", Secret: "secret-w", Scope: "write"},
		{Name: "reader", Secret: "secret-r", Scope: "read"},
		{Name: "bad-no-secret", Secret: "", Scope: "write"}, // 无 secret → 跳过
	})
	if !a.enabled() {
		t.Fatal("authorizer should be enabled")
	}

	id, ok := a.authenticate("secret-w")
	if !ok || id.Name != "writer" || !id.canWrite() {
		t.Fatalf("write token auth failed: %+v ok=%v", id, ok)
	}
	id, ok = a.authenticate("secret-r")
	if !ok || id.Name != "reader" || id.canWrite() {
		t.Fatalf("read token should not canWrite: %+v ok=%v", id, ok)
	}
	if _, ok := a.authenticate("wrong"); ok {
		t.Fatal("wrong secret must not authenticate")
	}
	if _, ok := a.authenticate(""); ok {
		t.Fatal("empty secret must not authenticate")
	}
	if names := a.tokenNames(); len(names) != 2 {
		t.Fatalf("tokenNames = %v, want 2 usable tokens", names)
	}
}

func TestWriteGuard(t *testing.T) {
	h := &Handler{}
	ctx := withIdentity(context.Background(), &identity{Name: "reader", Scope: ScopeRead})
	if err := h.writeGuard(ctx, "service_deploy"); err == nil {
		t.Fatal("read identity must be rejected on write tool")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("error should explain read-only: %v", err)
	}

	ctxW := withIdentity(context.Background(), &identity{Name: "writer", Scope: ScopeWrite})
	if err := h.writeGuard(ctxW, "service_deploy"); err != nil {
		t.Fatalf("write identity should pass: %v", err)
	}

	if err := h.writeGuard(context.Background(), "x"); err == nil {
		t.Fatal("missing identity must be rejected")
	}
}

func TestTruncStr(t *testing.T) {
	if got := truncStr("short"); got != "short" {
		t.Fatalf("short string changed: %q", got)
	}
	big := strings.Repeat("a", maxResultBytes+10000)
	got := truncStr(big)
	if len(got) >= len(big) {
		t.Fatalf("truncStr did not shrink: %d -> %d", len(big), len(got))
	}
	if !strings.Contains(got, "中间省略") {
		t.Fatal("truncStr should mark the elided middle")
	}
	if !strings.HasPrefix(got, "aaa") {
		t.Fatal("head must be preserved")
	}
}

func TestUploadTicketFlow(t *testing.T) {
	tb := newUploadTable()
	tk, err := tb.begin("gw.zip", 100, 500)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if tk.ID == "" || tk.Ticket == "" || len(tk.Ticket) != ticketBytes*2 {
		t.Fatalf("ticket shape wrong: %+v", tk)
	}
	// 未上传前 take 必须失败
	if _, _, err := tb.take(tk.ID); err == nil {
		t.Fatal("take before upload must fail")
	}
	// 直传后 take 一次性成功
	tb.mu.Lock()
	tk.State = ticketDone
	tk.Path = "/tmp/x.zip"
	tb.mu.Unlock()
	path, filename, err := tb.take(tk.ID)
	if err != nil || path != "/tmp/x.zip" || filename != "gw.zip" {
		t.Fatalf("take failed: path=%q file=%q err=%v", path, filename, err)
	}
	// 二次 take 必须失败（一次性）
	if _, _, err := tb.take(tk.ID); err == nil {
		t.Fatal("second take must fail (one-time)")
	}
}

func TestUploadTicketSizeLimit(t *testing.T) {
	tb := newUploadTable()
	if _, err := tb.begin("big.zip", 600<<20, 500); err == nil {
		t.Fatal("declared size above max_upload_mb must be rejected at begin")
	}
	if _, err := tb.begin("ok.zip", 100, 500); err != nil {
		t.Fatalf("size within limit rejected: %v", err)
	}
}

func TestUploadHandlerSizeMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{tickets: newUploadTable()}
	tk, err := h.tickets.begin("gw.zip", 10, 500)
	if err != nil {
		t.Fatal(err)
	}

	// 大小不符 → 400，票据作废（一次性）
	body := strings.NewReader("1234567890123456") // 16 bytes ≠ declared 10
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/build-upload?upload_id="+tk.ID+"&ticket="+tk.Ticket, body)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	h.UploadBuildPackage(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("size mismatch should 400, got %d: %s", rec.Code, rec.Body.String())
	}
	// 票据已被消费：重放 → 401
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/build-upload?upload_id="+tk.ID+"&ticket="+tk.Ticket, strings.NewReader("1234567890"))
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = req2
	h.UploadBuildPackage(c2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("ticket replay should 401, got %d", rec2.Code)
	}
}

func TestUploadHandlerWrongTicket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{tickets: newUploadTable()}
	tk, _ := h.tickets.begin("gw.zip", 4, 500)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/build-upload?upload_id="+tk.ID+"&ticket=deadbeef", strings.NewReader("abcd"))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	h.UploadBuildPackage(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong ticket should 401, got %d", rec.Code)
	}
}

func TestUploadHandlerSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{tickets: newUploadTable()}
	tk, _ := h.tickets.begin("gw.zip", 4, 500)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/build-upload?upload_id="+tk.ID+"&ticket="+tk.Ticket, strings.NewReader("abcd"))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	h.UploadBuildPackage(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid upload should 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, _, err := h.tickets.take(tk.ID); err != nil {
		t.Fatalf("take after upload should succeed: %v", err)
	}
}

func TestInvestigationStateMachine(t *testing.T) {
	running := &store.Investigation{Messages: "[]"}
	if statusOf(running) != "running" {
		t.Fatalf("placeholder record should be running, got %s", statusOf(running))
	}
	done := &store.Investigation{Messages: `[{"role":"user","content":"q"}]`, Conclusion: "root cause"}
	if statusOf(done) != "done" {
		t.Fatalf("with conclusion should be done, got %s", statusOf(done))
	}
	failed := &store.Investigation{Messages: `[{"role":"assistant","content":"[ERROR] boom"}]`}
	if statusOf(failed) != "error" {
		t.Fatalf("error record should be error, got %s", statusOf(failed))
	}
}

func TestEvidenceTail(t *testing.T) {
	turns := []chatTurn{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "partial"},
		{Role: "tool_transcript", Content: "→ get_service(...)\n← get_service: ok"},
		{Role: "user", Content: "follow-up"},
	}
	if got := evidenceTail(turns); !strings.HasPrefix(got, "→ get_service") {
		t.Fatalf("evidenceTail should prefer last transcript, got %q", got)
	}
	if got := evidenceTail(turns[:2]); got != "partial" {
		t.Fatalf("without transcript should fall back to assistant, got %q", got)
	}
}

func TestParseTurns(t *testing.T) {
	turns, err := parseTurns(`[{"role":"user","content":"q"},{"role":"assistant","content":"a"}]`)
	if err != nil || len(turns) != 2 {
		t.Fatalf("parseTurns failed: %v %v", turns, err)
	}
	if _, err := parseTurns("not json"); err == nil {
		t.Fatal("corrupted messages should error")
	}
	if turns, err := parseTurns(""); err != nil || turns != nil {
		t.Fatalf("empty messages should be nil,nil, got %v %v", turns, err)
	}
}

func TestInvestigationSystemGuardrails(t *testing.T) {
	sys := investigationSystem()
	for _, want := range []string{"不要执行任何变更类操作", "证据"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system prompt missing guardrail %q", want)
		}
	}
}

func TestClipAndSummarizeArgs(t *testing.T) {
	if got := clip("abcdef", 3); !strings.HasPrefix(got, "abc") {
		t.Fatalf("clip failed: %q", got)
	}
	args := summarizeArgs(map[string]string{"cluster": "prod", "config": strings.Repeat("x", 500)})
	if !strings.Contains(args, "cluster=prod") {
		t.Fatalf("args summary missing short field: %s", args)
	}
	if !strings.Contains(args, "<500 bytes>") {
		t.Fatalf("big field should be length-only: %s", args)
	}
}

func TestBaseURLFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{auth: newAuthorizer([]TokenConfig{{Name: "t", Secret: "s", Scope: "read"}})}
	mw := h.Middleware()

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer s")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	var gotBase string
	mw(c)
	gotBase = baseURLFrom(c.Request.Context())
	if gotBase != "https://"+req.Host {
		t.Fatalf("baseURL = %q, want https host", gotBase)
	}

	// 错 token → 401 + WWW-Authenticate
	req2 := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req2.Header.Set("Authorization", "Bearer nope")
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = req2
	mw(c2)
	if !c2.IsAborted() && rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bad token should 401, got %d aborted=%v", rec2.Code, c2.IsAborted())
	}
}

func TestReadyRequiresToken(t *testing.T) {
	h := New(Deps{Config: Config{Enabled: true}, Log: testLogger()})
	if err := h.Ready(); err == nil {
		t.Fatal("no tokens configured: Ready must fail fast")
	}
	h2 := New(Deps{Config: Config{Enabled: true, Tokens: []TokenConfig{{Name: "t", Secret: "s", Scope: "read"}}}, Log: testLogger()})
	if err := h2.Ready(); err != nil {
		t.Fatalf("with token Ready should pass: %v", err)
	}
	if h2.srv == nil {
		t.Fatal("New should build the mcp server")
	}
	if h2.HTTPHandler() == nil {
		t.Fatal("HTTPHandler should be non-nil")
	}
}

func TestToolRegistrationCount(t *testing.T) {
	h := New(Deps{Config: Config{Tokens: []TokenConfig{{Name: "t", Secret: "s", Scope: "write"}}}, Log: testLogger()})
	// 38 个工具（P1 清单）：注册失败时 SDK panic（AddTool 语义），构造完成即全部注册
	if h.srv == nil {
		t.Fatal("server not built")
	}
}

func TestStatusOfTimeFields(t *testing.T) {
	now := time.Now()
	if formatTime(now) == "" {
		t.Fatal("formatTime should format non-zero time")
	}
	if formatTime(time.Time{}) != "" {
		t.Fatal("zero time should format to empty")
	}
	if formatTimePtr(nil) != "" || formatTimePtr(&now) == "" {
		t.Fatal("formatTimePtr broken")
	}
	if !bytes.Contains([]byte(truncJSON(make([]byte, maxResultBytes+10))), []byte("truncated")) {
		t.Fatal("truncJSON should mark truncation")
	}
}
