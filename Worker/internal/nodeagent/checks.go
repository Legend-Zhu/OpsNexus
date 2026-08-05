// On-demand ad-hoc probes: GET /api/v1/local/check/port and
// POST /api/v1/local/check/http. Unlike the monitor package's periodic
// service-bound checks, these are one-shot probes of an arbitrary host:port /
// URL — consumed by the patrol orchestration (via the manager proxy) and the
// MCP check_* tools. Read-only: not subject to the command policy.
//
// Probes originate from the Worker container's network namespace: to probe a
// middleware bound to the host (e.g. MySQL on the node), target the node IP,
// not 127.0.0.1 (same caveat as monitor/httpcheck.go).
package nodeagent

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// checkTimeoutMax 单次探测超时上限（manager NodeClient 总预算 15s 内）。
const checkTimeoutMax = 10 * time.Second

// ---- port check ----

type portCheckResp struct {
	Node      string `json:"node"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// checkPort handles GET /api/v1/local/check/port?host=&port=&timeout=3s.
func (a *API) checkPort(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	host, port := q.Get("host"), q.Get("port")
	if host == "" {
		writeErr(w, http.StatusBadRequest, errEmpty("host"))
		return
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid port %q", port))
		return
	}
	timeout, err := checkTimeout(q.Get("timeout"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	node, _ := hostname()
	resp := portCheckResp{Node: node, Host: host, Port: port}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	resp.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		resp.Error = err.Error()
	} else {
		_ = conn.Close()
		resp.OK = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- http check ----

type httpCheckReq struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	ExpectedStatus []int             `json:"expectedStatus,omitempty"` // 空 = 任意 2xx
	ExpectedBody   string            `json:"expectedBody,omitempty"`   // 正则
	Timeout        string            `json:"timeout,omitempty"`
}

type httpCheckResp struct {
	Node      string `json:"node"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// checkHTTP handles POST /api/v1/local/check/http（JSON body，含 headers map）。
// 判定逻辑与 monitor/httpcheck.go 一致：64KiB body 上限、状态码精确列表、body 正则。
func (a *API) checkHTTP(w http.ResponseWriter, r *http.Request) {
	var req httpCheckReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.URL == "" {
		writeErr(w, http.StatusBadRequest, errEmpty("url"))
		return
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	timeout, err := checkTimeout(req.Timeout)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var bodyRe *regexp.Regexp
	if req.ExpectedBody != "" {
		bodyRe, err = regexp.Compile(req.ExpectedBody)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("expectedBody: %w", err))
			return
		}
	}
	hreq, err := http.NewRequestWithContext(r.Context(), req.Method, req.URL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}

	node, _ := hostname()
	resp := httpCheckResp{Node: node, URL: req.URL}
	start := time.Now()
	hresp, err := (&http.Client{Timeout: timeout}).Do(hreq)
	resp.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	defer hresp.Body.Close()
	resp.Status = hresp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(hresp.Body, 64*1024))

	switch {
	case !statusExpected(hresp.StatusCode, req.ExpectedStatus):
		if len(req.ExpectedStatus) > 0 {
			resp.Error = fmt.Sprintf("status %d not in expected %v", hresp.StatusCode, req.ExpectedStatus)
		} else {
			resp.Error = fmt.Sprintf("status %d not 2xx", hresp.StatusCode)
		}
	case bodyRe != nil && !bodyRe.Match(body):
		resp.Error = fmt.Sprintf("body does not match %q", req.ExpectedBody)
	default:
		resp.OK = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// statusExpected：expected 为空时任意 2xx 视为健康，否则精确匹配列表。
func statusExpected(code int, expected []int) bool {
	if len(expected) == 0 {
		return code >= 200 && code < 300
	}
	for _, s := range expected {
		if code == s {
			return true
		}
	}
	return false
}

// checkTimeout 解析 timeout 参数（默认 3s，上限 checkTimeoutMax）。
func checkTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 3 * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("timeout: %w", err)
	}
	if d <= 0 || d > checkTimeoutMax {
		return 0, fmt.Errorf("timeout must be within (0, %s]", checkTimeoutMax)
	}
	return d, nil
}
