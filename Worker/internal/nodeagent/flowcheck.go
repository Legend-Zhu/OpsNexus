// Multi-step HTTP transaction probe: POST /api/v1/local/check/flow.
// 单步 check_http 覆盖不了的"事务型"探测——典型场景：端口通但用户登录
// 不了系统，需要 POST /login 拿 token → 带 token 调业务接口验证。
// 步骤顺序执行，extract 把响应里的值提取为变量供后续步骤 {{var}} 引用；
// 任一失败即终止并报告卡在哪一步。提取值（token/密码）绝不回显，只回变量名。
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// flowStepReq 一个探测步骤。
type flowStepReq struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Method       string            `json:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	ExpectStatus []int             `json:"expectStatus,omitempty"` // 空 = 任意 2xx
	ExpectBody   string            `json:"expectBody,omitempty"`   // 正则
	Extract      map[string]string `json:"extract,omitempty"`      // var -> "$.json.path" 或 "re:正则(首个捕获组)"
}

// flowReq 多步探测请求。
type flowReq struct {
	Steps   []flowStepReq     `json:"steps"`
	Vars    map[string]string `json:"vars,omitempty"`
	Timeout string            `json:"timeout,omitempty"` // 每步请求超时（默认 3s，上限 10s）
}

// flowStepResp 单步结果（不含提取值，防 token 泄漏）。
type flowStepResp struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Status    int      `json:"status"`
	LatencyMS int64    `json:"latencyMs"`
	Extracted []string `json:"extracted,omitempty"` // 提取成功的变量名（值不回显）
	Error     string   `json:"error,omitempty"`
}

// flowResp 多步探测结果。
type flowResp struct {
	Node       string         `json:"node"`
	OK         bool           `json:"ok"`
	Steps      []flowStepResp `json:"steps"`
	FailedStep string         `json:"failedStep,omitempty"`
	LatencyMS  int64          `json:"latencyMs"`
}

// checkFlow handles POST /api/v1/local/check/flow。
func (a *API) checkFlow(w http.ResponseWriter, r *http.Request) {
	var req flowReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Steps) == 0 {
		writeErr(w, http.StatusBadRequest, errEmpty("steps"))
		return
	}
	timeout, err := checkTimeout(req.Timeout)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	for i, s := range req.Steps {
		if s.Name == "" || s.URL == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("steps[%d]: name 和 url 必填", i))
			return
		}
	}

	node, _ := hostname()
	resp := flowResp{Node: node, Steps: make([]flowStepResp, 0, len(req.Steps))}
	vars := map[string]string{}
	for k, v := range req.Vars {
		vars[k] = v
	}
	client := &http.Client{Timeout: timeout}
	start := time.Now()

	for _, s := range req.Steps {
		sr := s.runStep(r.Context(), client, vars, timeout)
		resp.Steps = append(resp.Steps, sr)
		if !sr.OK {
			resp.FailedStep = s.Name
			break
		}
	}
	resp.LatencyMS = time.Since(start).Milliseconds()
	resp.OK = resp.FailedStep == ""
	writeJSON(w, http.StatusOK, resp)
}

// runStep 执行单步：变量替换 → 请求 → 断言 → 提取（写入共享 vars）。
func (s flowStepReq) runStep(ctx context.Context, client *http.Client, vars map[string]string, timeout time.Duration) flowStepResp {
	out := flowStepResp{Name: s.Name}

	url, err := substituteVars(s.URL, vars)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	body, err := substituteVars(s.Body, vars)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	method := s.Method
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for k, v := range s.Headers {
		vv, err := substituteVars(v, vars)
		if err != nil {
			out.Error = err.Error()
			return out
		}
		hreq.Header.Set(k, vv)
	}
	if bodyReader != nil && hreq.Header.Get("Content-Type") == "" {
		hreq.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	hresp, err := client.Do(hreq)
	out.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer hresp.Body.Close()
	out.Status = hresp.StatusCode
	respBody, _ := io.ReadAll(io.LimitReader(hresp.Body, 64*1024))

	if !statusExpected(hresp.StatusCode, s.ExpectStatus) {
		if len(s.ExpectStatus) > 0 {
			out.Error = fmt.Sprintf("status %d not in expected %v", hresp.StatusCode, s.ExpectStatus)
		} else {
			out.Error = fmt.Sprintf("status %d not 2xx", hresp.StatusCode)
		}
		return out
	}
	if s.ExpectBody != "" {
		re, err := regexp.Compile(s.ExpectBody)
		if err != nil {
			out.Error = "expectBody 正则非法: " + err.Error()
			return out
		}
		if !re.Match(respBody) {
			out.Error = fmt.Sprintf("body does not match %q", s.ExpectBody)
			return out
		}
	}

	// 提取变量（写共享 vars 供后续步骤；值不进响应）
	for name, spec := range s.Extract {
		val, err := extractValue(respBody, spec)
		if err != nil {
			out.Error = fmt.Sprintf("extract %s: %v", name, err)
			return out
		}
		vars[name] = val
		out.Extracted = append(out.Extracted, name)
	}
	out.OK = true
	return out
}

// substituteVars 替换 {{var}} 占位符；未定义变量报错。
func substituteVars(s string, vars map[string]string) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var miss string
	out := osExpandVars(s, vars, &miss)
	if miss != "" {
		return "", fmt.Errorf("变量 %q 未定义（前序步骤未提取）", miss)
	}
	return out, nil
}

func osExpandVars(s string, vars map[string]string, miss *string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		key := strings.TrimSpace(s[i+2 : i+j])
		v, ok := vars[key]
		if !ok && *miss == "" {
			*miss = key
		}
		b.WriteString(v)
		s = s[i+j+2:]
	}
	return b.String()
}

// extractValue 从响应体提取值："$.data.token" JSON 路径（支持数组下标）
// 或 "re:正则"（首个捕获组，无捕获组则整体匹配）。
func extractValue(body []byte, spec string) (string, error) {
	if re, ok := strings.CutPrefix(spec, "re:"); ok {
		r, err := regexp.Compile(re)
		if err != nil {
			return "", fmt.Errorf("正则非法: %v", err)
		}
		m := r.FindSubmatch(body)
		if m == nil {
			return "", fmt.Errorf("正则不匹配")
		}
		if len(m) > 1 {
			return string(m[1]), nil
		}
		return string(m[0]), nil
	}
	path, ok := strings.CutPrefix(spec, "$.")
	if !ok {
		return "", fmt.Errorf("提取表达式须为 $.json.path 或 re:正则")
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("响应不是 JSON: %v", err)
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur, ok = node[seg]
			if !ok {
				return "", fmt.Errorf("路径 %q 不存在（键 %q）", spec, seg)
			}
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return "", fmt.Errorf("路径 %q 数组下标 %q 越界", spec, seg)
			}
			cur = node[idx]
		default:
			return "", fmt.Errorf("路径 %q 在 %q 处不是对象/数组", spec, seg)
		}
	}
	switch v := cur.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	default:
		b, _ := json.Marshal(v)
		return string(b), nil
	}
}
