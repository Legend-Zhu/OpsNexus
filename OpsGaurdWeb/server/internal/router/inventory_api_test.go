package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// newInventoryTestServer 构造完整路由 + 播种集群 dev（单条纳管操作只读写
// store，无需真实 Worker）。走完整 HTTP 栈，覆盖路由注册与错误映射。
// withRules 时额外挂载告警规则服务（验证纳管 ↔ 规则自动同步）。
func newInventoryTestServer(t *testing.T, withRules bool) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutCluster(&store.Cluster{Name: "dev"}); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	cs := cluster.New(st)
	h := api.NewHandlers()
	h.SetClusterService(cs)
	if withRules {
		h.SetAlertRuleService(alertrule.New(st, cs))
	}
	srv := httptest.NewServer(New(h))
	t.Cleanup(srv.Close)
	return srv
}

// inventoryRequest 对 /api/v1 下相对路径发 JSON 请求，返回状态码与响应封装。
func inventoryRequest(t *testing.T, srv *httptest.Server, method, v1Path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/api/v1"+v1Path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, v1Path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, out
}

// TestInventoryItemAPI 单条纳管 API 全流程：新增(201) → 重名/校验失败(400) →
// 原位更新 → 改名 → 改名撞名(400) → 删除 → 重复删除(404)，最终清单只剩一条。
func TestInventoryItemAPI(t *testing.T) {
	srv := newInventoryTestServer(t, false)

	// 新增
	if code, _ := inventoryRequest(t, srv, "POST", "/clusters/dev/inventory/items",
		`{"name":"r-nacos","type":"standalone-container","ref":"r-nacos","node":"node-01","ports":["8848"],"category":"middleware"}`); code != http.StatusCreated {
		t.Fatal("add should 201")
	}

	// 重名 → 400
	if code, _ := inventoryRequest(t, srv, "POST", "/clusters/dev/inventory/items",
		`{"name":"r-nacos","type":"host-service","ref":"10.0.0.1","ports":["8848"]}`); code != http.StatusBadRequest {
		t.Fatalf("duplicate add should 400, got %d", code)
	}

	// 校验失败（standalone 缺 node）→ 400
	if code, _ := inventoryRequest(t, srv, "POST", "/clusters/dev/inventory/items",
		`{"name":"bad","type":"standalone-container","ref":"bad"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid item should 400, got %d", code)
	}

	// 原位更新（端口变化，名称不变——不得误判为重名）
	if code, _ := inventoryRequest(t, srv, "PUT", "/clusters/dev/inventory/items/r-nacos",
		`{"name":"r-nacos","type":"standalone-container","ref":"r-nacos","node":"node-01","ports":["8848","9848"]}`); code != http.StatusOK {
		t.Fatalf("in-place update should 200, got %d", code)
	}

	// 改名更新
	if code, _ := inventoryRequest(t, srv, "PUT", "/clusters/dev/inventory/items/r-nacos",
		`{"name":"nacos","type":"standalone-container","ref":"r-nacos","node":"node-01","ports":["8848","9848"]}`); code != http.StatusOK {
		t.Fatalf("rename should 200, got %d", code)
	}

	// 改名撞已有条目 → 400（先补一个 grafana）
	inventoryRequest(t, srv, "POST", "/clusters/dev/inventory/items",
		`{"name":"grafana","type":"host-service","ref":"10.0.0.10","ports":["3000"]}`)
	if code, _ := inventoryRequest(t, srv, "PUT", "/clusters/dev/inventory/items/nacos",
		`{"name":"grafana","type":"standalone-container","ref":"r-nacos","node":"node-01"}`); code != http.StatusBadRequest {
		t.Fatalf("rename collision should 400, got %d", code)
	}

	// 更新不存在的条目 → 404
	if code, _ := inventoryRequest(t, srv, "PUT", "/clusters/dev/inventory/items/ghost",
		`{"name":"ghost","type":"host-service","ref":"10.0.0.2","ports":["80"]}`); code != http.StatusNotFound {
		t.Fatalf("update missing item should 404, got %d", code)
	}

	// 删除 + 重复删除 404
	if code, _ := inventoryRequest(t, srv, "DELETE", "/clusters/dev/inventory/items/nacos", ""); code != http.StatusOK {
		t.Fatalf("delete: status=%d", code)
	}
	if code, _ := inventoryRequest(t, srv, "DELETE", "/clusters/dev/inventory/items/nacos", ""); code != http.StatusNotFound {
		t.Fatalf("double delete should 404, got %d", code)
	}

	// 集群不存在 → 404
	if code, _ := inventoryRequest(t, srv, "POST", "/clusters/nope/inventory/items",
		`{"name":"x","type":"host-service","ref":"10.0.0.3","ports":["80"]}`); code != http.StatusNotFound {
		t.Fatalf("add to missing cluster should 404, got %d", code)
	}

	// 清单最终状态：只剩 grafana
	code, resp := inventoryRequest(t, srv, "GET", "/clusters/dev/inventory", "")
	if code != http.StatusOK {
		t.Fatalf("get inventory: status=%d", code)
	}
	items := resp["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "grafana" {
		t.Fatalf("unexpected final inventory: %v", items)
	}
}

// TestInventoryItemRuleSync 带监控配置的纳管条目：新增后告警规则自动出现，
// 删除条目后规则自动清理（复用 SyncFromInventory 的 diff 同步）。
func TestInventoryItemRuleSync(t *testing.T) {
	srv := newInventoryTestServer(t, true)

	body := `{"name":"mysql","type":"host-service","ref":"10.0.0.10","ports":["3306"],"category":"middleware",
		  "monitoring":{"enabled":true,"portChecks":[{"port":"3306","interval":"30s"}]}}`
	if code, _ := inventoryRequest(t, srv, "POST", "/clusters/dev/inventory/items", body); code != http.StatusCreated {
		t.Fatal("add monitored item should 201")
	}
	code, resp := inventoryRequest(t, srv, "GET", "/alertrules?cluster=dev", "")
	if code != http.StatusOK {
		t.Fatalf("list rules: status=%d", code)
	}
	rules := resp["data"].(map[string]any)["items"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["service"] != "mysql" {
		t.Fatalf("expected rule for mysql, got %v", rules)
	}

	if code, _ := inventoryRequest(t, srv, "DELETE", "/clusters/dev/inventory/items/mysql", ""); code != http.StatusOK {
		t.Fatal("delete should 200")
	}
	_, resp = inventoryRequest(t, srv, "GET", "/alertrules?cluster=dev", "")
	// 空清单时 items 序列化为 null，视为空
	if raw, ok := resp["data"].(map[string]any)["items"].([]any); ok && len(raw) != 0 {
		t.Fatalf("rule should be removed with its item, got %v", raw)
	}
}
