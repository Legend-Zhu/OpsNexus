package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
)

// seedUsageRecord 经 collector 异步入账一条记录并等待落库。
func seedUsageRecord(t *testing.T, svc *mlops.Service, callID string) {
	t.Helper()
	now := time.Now()
	svc.Record(usage.Record{
		OperationID: "op-" + callID, CallID: callID,
		Provider: "openai", Model: "gpt-4o", Scenario: "chat",
		EntryPoint:   "/api/v1/ainexus/chat",
		PromptTokens: 100, CompletionTokens: 40, TotalTokens: 140, UsagePresent: true,
		OK: true, Status: "success", StartedAt: now, FinishedAt: now.Add(time.Second),
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d, err := svc.CostsDetail(mlops.DetailQuery{}); err == nil && d.Total >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("usage record not persisted in time")
}

func TestMLOpsCostsAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := newTestMlopsStore(t)
	svc := mlops.New(st)
	svc.StartUsage(mlops.UsageSettings{QueueSize: 8})
	defer svc.StopUsage()
	if _, err := svc.SavePricing(mlops.PricingInput{
		Provider: "openai", Model: "gpt-4o", PriceInPerM: "2", PriceOutPerM: "8",
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	seedUsageRecord(t, svc, "call-api-1")

	h := NewHandlers()
	h.SetMlopsService(svc)

	// overview：今日/本月合计 + collector 状态
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mlops/costs/overview", nil)
	h.MLOpsCostsOverview(c)
	if w.Code != http.StatusOK {
		t.Fatalf("overview status = %d body=%s", w.Code, w.Body.String())
	}
	var ovResp struct {
		Code int `json:"code"`
		Data struct {
			Day         string `json:"day"`
			Month       string `json:"month"`
			TodayTotals struct {
				Calls     int64 `json:"calls"`
				CostMinor int64 `json:"cost_minor"`
			} `json:"today_totals"`
			MonthTotals struct {
				Calls     int64 `json:"calls"`
				CostMinor int64 `json:"cost_minor"`
			} `json:"month_totals"`
			ByModel []struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"by_model"`
			Collector struct {
				QueueCap int `json:"queue_cap"`
			} `json:"collector"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ovResp); err != nil {
		t.Fatal(err)
	}
	if ovResp.Data.TodayTotals.Calls != 1 || ovResp.Data.MonthTotals.Calls != 1 {
		t.Fatalf("overview calls today=%d month=%d", ovResp.Data.TodayTotals.Calls, ovResp.Data.MonthTotals.Calls)
	}
	// 100×2 + 40×8 = 520 微元
	if ovResp.Data.MonthTotals.CostMinor != 520 {
		t.Fatalf("month cost = %d, want 520", ovResp.Data.MonthTotals.CostMinor)
	}
	if len(ovResp.Data.ByModel) != 1 || ovResp.Data.ByModel[0].Model != "gpt-4o" {
		t.Fatalf("by_model = %+v", ovResp.Data.ByModel)
	}
	if ovResp.Data.Collector.QueueCap != 8 {
		t.Fatalf("collector queue_cap = %d", ovResp.Data.Collector.QueueCap)
	}

	// trend：30 天窗口含今日行
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mlops/costs/trend", nil)
	h.MLOpsCostsTrend(c)
	if w.Code != http.StatusOK {
		t.Fatalf("trend status = %d", w.Code)
	}
	var trendResp struct {
		Data struct {
			Items []struct {
				Day    string `json:"day"`
				Totals struct {
					Calls int64 `json:"calls"`
				} `json:"totals"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &trendResp); err != nil {
		t.Fatal(err)
	}
	if len(trendResp.Data.Items) != 30 || trendResp.Data.Items[29].Totals.Calls != 1 {
		t.Fatalf("trend = %d rows, last calls = %d", len(trendResp.Data.Items), trendResp.Data.Items[len(trendResp.Data.Items)-1].Totals.Calls)
	}

	// detail：scenario 过滤命中 + 分页 envelope
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mlops/costs/detail?scenario=chat&limit=10", nil)
	h.MLOpsCostsDetail(c)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", w.Code, w.Body.String())
	}
	var detailResp struct {
		Data struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detailResp); err != nil {
		t.Fatal(err)
	}
	if detailResp.Data.Total != 1 || len(detailResp.Data.Items) != 1 {
		t.Fatalf("detail total=%d items=%d", detailResp.Data.Total, len(detailResp.Data.Items))
	}
	item := detailResp.Data.Items[0]
	for _, key := range []string{"seq", "call_id", "operation_id", "provider", "model", "scenario", "usage_present", "priced", "cost_minor", "status", "started_at"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("detail item missing %q: %v", key, item)
		}
	}

	// operation 下钻
	opID := "op-call-api-1"
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mlops/costs/operations/"+opID, nil)
	h.MLOpsCostsOperation(c)
	if w.Code != http.StatusOK {
		t.Fatalf("operation status = %d", w.Code)
	}

	// pricing：列表 + 保存（非法 400）+ 删除（404）
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/mlops/costs/pricing", nil)
	h.ListMLOpsPricing(c)
	if w.Code != http.StatusOK {
		t.Fatalf("pricing list status = %d", w.Code)
	}

	body, _ := json.Marshal(map[string]string{
		"provider": "p", "model": "m", "price_in_per_m": "bad", "price_out_per_m": "1",
	})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/mlops/costs/pricing", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.SaveMLOpsPricing(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad pricing status = %d, want 400", w.Code)
	}

	body, _ = json.Marshal(map[string]string{
		"provider": "p", "model": "m", "price_in_per_m": "1.25", "price_out_per_m": "0",
	})
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/mlops/costs/pricing", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.SaveMLOpsPricing(c)
	if w.Code != http.StatusOK {
		t.Fatalf("save pricing status = %d body=%s", w.Code, w.Body.String())
	}
	var saved struct {
		Data struct {
			PriceInPerM string `json:"price_in_per_m"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Data.PriceInPerM != "1.25" {
		t.Fatalf("price roundtrip = %q", saved.Data.PriceInPerM)
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/mlops/costs/pricing?provider=p&model=m", nil)
	h.DeleteMLOpsPricing(c)
	if w.Code != http.StatusOK {
		t.Fatalf("delete pricing status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/mlops/costs/pricing?provider=p&model=m", nil)
	h.DeleteMLOpsPricing(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete missing pricing status = %d, want 404", w.Code)
	}
}

// mlops 未启用时所有 costs 端点统一 503。
func TestMLOpsCostsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlers()
	for _, tc := range []struct {
		name string
		call func(c *gin.Context)
	}{
		{"overview", h.MLOpsCostsOverview},
		{"trend", h.MLOpsCostsTrend},
		{"detail", h.MLOpsCostsDetail},
		{"operation", h.MLOpsCostsOperation},
		{"pricing-list", h.ListMLOpsPricing},
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		tc.call(c)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", tc.name, w.Code)
		}
	}
}
