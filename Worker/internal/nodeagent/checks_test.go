package nodeagent

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// check 类端点不触碰 docker client/policy，nil 依赖即可。
func newTestAPI() *API { return New(nil, nil, nil) }

func doReq(t *testing.T, h http.HandlerFunc, method, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestCheckPort(t *testing.T) {
	a := newTestAPI()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	// 开放的端口 → ok
	code, out := doReq(t, a.checkPort, "GET", "/api/v1/local/check/port?host=127.0.0.1&port="+port, "")
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("open port: code=%d out=%v", code, out)
	}
	if out["node"] == "" {
		t.Fatal("response missing node field")
	}
	ln.Close()

	// 关闭的端口 → ok=false 且带 error（仍是 200，探测结果是数据不是传输错误）
	code, out = doReq(t, a.checkPort, "GET", "/api/v1/local/check/port?host=127.0.0.1&port="+port+"&timeout=1s", "")
	if code != http.StatusOK || out["ok"] != false || out["error"] == "" {
		t.Fatalf("closed port: code=%d out=%v", code, out)
	}

	// 参数校验
	for _, target := range []string{
		"/api/v1/local/check/port?port=80",                    // 缺 host
		"/api/v1/local/check/port?host=x",                     // 缺 port
		"/api/v1/local/check/port?host=x&port=abc",            // 非数字
		"/api/v1/local/check/port?host=x&port=0",              // 越界
		"/api/v1/local/check/port?host=x&port=80&timeout=99s", // 超上限
	} {
		if code, _ = doReq(t, a.checkPort, "GET", target, ""); code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d, want 400", target, code)
		}
	}
}

func TestCheckHTTP(t *testing.T) {
	a := newTestAPI()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello middleware"))
	}))
	defer up.Close()

	// 默认 2xx 即健康
	code, out := doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{"url":"`+up.URL+`"}`)
	if code != http.StatusOK || out["ok"] != true || out["status"] != 200.0 {
		t.Fatalf("default 2xx: code=%d out=%v", code, out)
	}
	// expectedStatus 精确列表不匹配
	code, out = doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{"url":"`+up.URL+`","expectedStatus":[201]}`)
	if code != http.StatusOK || out["ok"] != false || out["error"] == "" {
		t.Fatalf("expectedStatus mismatch: code=%d out=%v", code, out)
	}
	// body 正则命中
	code, out = doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{"url":"`+up.URL+`","expectedBody":"hello.*ware"}`)
	if out["ok"] != true {
		t.Fatalf("body regex hit: out=%v", out)
	}
	// body 正则不中
	code, out = doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{"url":"`+up.URL+`","expectedBody":"^nope$"}`)
	if out["ok"] != false {
		t.Fatalf("body regex miss: out=%v", out)
	}
	// 非法正则 / 缺 url → 400
	if code, _ = doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{"url":"`+up.URL+`","expectedBody":"(["}`); code != http.StatusBadRequest {
		t.Fatalf("bad regex: code=%d", code)
	}
	if code, _ = doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{}`); code != http.StatusBadRequest {
		t.Fatalf("missing url: code=%d", code)
	}
	// 不可达地址 → ok=false（200 + error）
	code, out = doReq(t, a.checkHTTP, "POST", "/api/v1/local/check/http", `{"url":"http://127.0.0.1:1/x","timeout":"1s"}`)
	if code != http.StatusOK || out["ok"] != false || out["error"] == "" {
		t.Fatalf("unreachable: code=%d out=%v", code, out)
	}
}

func TestCheckTimeout(t *testing.T) {
	if d, err := checkTimeout(""); err != nil || d.String() != "3s" {
		t.Fatalf("default: %v %v", d, err)
	}
	if _, err := checkTimeout("500ms"); err != nil {
		t.Fatalf("500ms: %v", err)
	}
	for _, s := range []string{"0s", "-1s", "11s", "abc"} {
		if _, err := checkTimeout(s); err == nil {
			t.Fatalf("%s: want error", s)
		}
	}
}

func TestMatchProcess(t *testing.T) {
	cases := []struct {
		filter, name, cmdline string
		want                  bool
	}{
		{"java", "java", "/usr/bin/java -jar app.jar", true},
		{"redis", "redis-server", "", true},
		{"mysql", "mysqld", "/usr/sbin/mysqld --datadir=/data", true},
		{"JAVA", "java", "", false}, // filter 由调用方转小写，helper 不重复转
		{"nginx", "java", "-jar app.jar", false},
		{"app.jar", "java", "/usr/bin/java -jar app.jar", true},
	}
	for _, c := range cases {
		if got := matchProcess(c.filter, c.name, c.cmdline); got != c.want {
			t.Errorf("matchProcess(%q,%q,%q)=%v, want %v", c.filter, c.name, c.cmdline, got, c.want)
		}
	}
}
