package nodeagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginMock 模拟「登录拿 token → 带 token 访问业务接口」的目标系统。
// 正确凭据 admin/s3cret → token t0k3n；/api/me 需 Bearer t0k3n。
func loginMock(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["username"] == "admin" && body["password"] == "s3cret" {
				w.Write([]byte(`{"code":0,"data":{"token":"t0k3n"}}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":401,"msg":"bad credentials"}`))
		case "/api/me":
			if r.Header.Get("Authorization") == "Bearer t0k3n" {
				w.Write([]byte(`{"user":"admin","enabled":true}`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func flowBody(t *testing.T, up *httptest.Server, password string) string {
	t.Helper()
	body := map[string]any{
		"vars": map[string]string{"user": "admin", "pass": password},
		"steps": []map[string]any{
			{
				"name":    "login",
				"method":  "POST",
				"url":     up.URL + "/login",
				"body":    `{"username":"{{user}}","password":"{{pass}}"}`,
				"extract": map[string]string{"token": "$.data.token"},
			},
			{
				"name":       "verify",
				"url":        up.URL + "/api/me",
				"headers":    map[string]string{"Authorization": "Bearer {{token}}"},
				"expectBody": `"enabled":true`,
			},
		},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestCheckFlowLoginChain(t *testing.T) {
	a := newTestAPI()
	up := loginMock(t)

	// 正确凭据:两步全过,token 提取但不回显值
	code, out := doReq(t, a.checkFlow, "POST", "/api/v1/local/check/flow", flowBody(t, up, "s3cret"))
	if code != http.StatusOK {
		t.Fatalf("code=%d %v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("flow should pass: %v", out)
	}
	steps := out["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps=%v", steps)
	}
	s0 := steps[0].(map[string]any)
	if s0["ok"] != true {
		t.Fatalf("step0: %v", s0)
	}
	// 提取值不落响应(只有变量名)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "t0k3n") {
		t.Fatal("extracted token must not be echoed")
	}
	if ex, ok := s0["extracted"].([]any); !ok || len(ex) != 1 || ex[0] != "token" {
		t.Fatalf("extracted names: %v", s0["extracted"])
	}

	// 错误凭据:卡在 login 步,failedStep 定位
	code, out = doReq(t, a.checkFlow, "POST", "/api/v1/local/check/flow", flowBody(t, up, "wrong"))
	if out["ok"] != false || out["failedStep"] != "login" {
		t.Fatalf("should fail at login: %v", out)
	}
	if len(out["steps"].([]any)) != 1 {
		t.Fatal("失败后应终止,不执行后续步骤")
	}
}

func TestCheckFlowExtractModes(t *testing.T) {
	// JSON 路径(含数组下标)
	v, err := extractValue([]byte(`{"data":{"items":[{"id":7}]}}`), "$.data.items.0.id")
	if err != nil || v != "7" {
		t.Fatalf("json path: %q %v", v, err)
	}
	// 正则首个捕获组
	v, err = extractValue([]byte(`token=abc123&exp=9`), `re:token=([a-z0-9]+)`)
	if err != nil || v != "abc123" {
		t.Fatalf("regex: %q %v", v, err)
	}
	// 路径不存在
	if _, err = extractValue([]byte(`{"a":1}`), "$.b.c"); err == nil {
		t.Fatal("missing path should error")
	}
	// 非法表达式
	if _, err = extractValue([]byte(`{}`), "token"); err == nil {
		t.Fatal("bad spec should error")
	}
}

func TestSubstituteVars(t *testing.T) {
	out, err := substituteVars("Bearer {{token}}", map[string]string{"token": "x"})
	if err != nil || out != "Bearer x" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err = substituteVars("{{missing}}", nil); err == nil {
		t.Fatal("missing var should error")
	}
}
