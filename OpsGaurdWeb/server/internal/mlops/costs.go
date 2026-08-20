// 费用查询（MLOps P2，方案 §8.3）：总览（今日/本月 + 按模型/场景）、
// 日趋势、明细分页、operation 下钻。全部基于日聚合/明细读取，不回溯
// 重算历史成本。
package mlops

import (
	"sort"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// maxTrendDays 趋势查询最大天数（方案：限制最大时间范围）。
const maxTrendDays = 92

// CostTotals 一组日聚合行合计的报表口径。
type CostTotals struct {
	Calls            int64 `json:"calls"`
	SuccessCalls     int64 `json:"success_calls"`
	ErrorCalls       int64 `json:"error_calls"`
	CanceledCalls    int64 `json:"canceled_calls"`
	UnmeteredCalls   int64 `json:"unmetered_calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	PricedCalls      int64 `json:"priced_calls"`
	UnpricedCalls    int64 `json:"unpriced_calls"` // 无单价或无 usage（金额不可用）
	CostMinor        int64 `json:"cost_minor"`
	Operations       int64 `json:"operations"` // 当日不同 operation 估算合计
}

func accrueTotals(t *CostTotals, d *store.MLUsageDay) {
	t.Calls += d.Calls
	t.SuccessCalls += d.SuccessCalls
	t.ErrorCalls += d.ErrorCalls
	t.CanceledCalls += d.CanceledCalls
	t.UnmeteredCalls += d.UnmeteredCalls
	t.PromptTokens += d.PromptTokens
	t.CompletionTokens += d.CompletionTokens
	t.TotalTokens += d.TotalTokens
	t.PricedCalls += d.PricedCalls
	t.Operations += d.Operations
	t.CostMinor += d.CostMinor
}

// CostModelRow 按模型（provider+model）合计。
type CostModelRow struct {
	Provider string     `json:"provider"`
	Model    string     `json:"model"`
	Totals   CostTotals `json:"totals"`
}

// CostScenarioRow 按场景合计。
type CostScenarioRow struct {
	Scenario string     `json:"scenario"`
	Totals   CostTotals `json:"totals"`
}

// CostsOverview 总览：业务时区的今日 + 本月，按模型/场景（本月口径）。
type CostsOverview struct {
	Day         string            `json:"day"`
	Month       string            `json:"month"`
	Currency    string            `json:"currency"`
	Timezone    string            `json:"timezone"`
	TodayTotals CostTotals        `json:"today_totals"`
	MonthTotals CostTotals        `json:"month_totals"`
	ByModel     []CostModelRow    `json:"by_model"`
	ByScenario  []CostScenarioRow `json:"by_scenario"`
	Collector   CollectorStats    `json:"collector"`
}

// CostsOverview 计算总览（today/month 来自日聚合，无数据时为零值）。
func (s *Service) CostsOverview() (*CostsOverview, error) {
	now := time.Now().In(s.loc)
	today := now.Format("2006-01-02")
	month := now.Format("2006-01")

	days, err := s.st.ListMLUsageDays(month+"-01", month+"-31")
	if err != nil {
		return nil, err
	}
	byModel := map[string]*CostModelRow{}
	byScenario := map[string]*CostScenarioRow{}
	ov := &CostsOverview{
		Day:        today,
		Month:      month,
		Currency:   s.currency,
		Timezone:   s.loc.String(),
		ByModel:    []CostModelRow{},
		ByScenario: []CostScenarioRow{},
		Collector:  s.collectorStats(),
	}
	for _, d := range days {
		accrueTotals(&ov.MonthTotals, d)
		if d.Day == today {
			accrueTotals(&ov.TodayTotals, d)
		}
		mk := d.Provider + "\x00" + d.Model
		mr := byModel[mk]
		if mr == nil {
			mr = &CostModelRow{Provider: d.Provider, Model: d.Model}
			byModel[mk] = mr
		}
		accrueTotals(&mr.Totals, d)
		sr := byScenario[d.Scenario]
		if sr == nil {
			sr = &CostScenarioRow{Scenario: d.Scenario}
			byScenario[d.Scenario] = sr
		}
		accrueTotals(&sr.Totals, d)
	}
	for _, mr := range byModel {
		ov.ByModel = append(ov.ByModel, *mr)
	}
	sort.Slice(ov.ByModel, func(i, j int) bool {
		if ov.ByModel[i].Totals.CostMinor != ov.ByModel[j].Totals.CostMinor {
			return ov.ByModel[i].Totals.CostMinor > ov.ByModel[j].Totals.CostMinor
		}
		return ov.ByModel[i].Model < ov.ByModel[j].Model
	})
	for _, sr := range byScenario {
		ov.ByScenario = append(ov.ByScenario, *sr)
	}
	sort.Slice(ov.ByScenario, func(i, j int) bool {
		return ov.ByScenario[i].Totals.Calls > ov.ByScenario[j].Totals.Calls
	})
	return ov, nil
}

// CostTrendRow 一天的合计行。
type CostTrendRow struct {
	Day    string     `json:"day"`
	Totals CostTotals `json:"totals"`
}

// CostsTrend 日趋势（升序，缺数据的日期补零行；最多 maxTrendDays 天；
// from/to 为业务时区 yyyy-mm-dd，缺省取最近 30 天）。
func (s *Service) CostsTrend(fromDay, toDay string) ([]CostTrendRow, error) {
	if fromDay == "" || toDay == "" {
		to := time.Now().In(s.loc)
		if toDay == "" {
			toDay = to.Format("2006-01-02")
		}
		if fromDay == "" {
			fromDay = to.AddDate(0, 0, -29).Format("2006-01-02")
		}
	}
	from, err := time.ParseInLocation("2006-01-02", fromDay, s.loc)
	if err != nil {
		return nil, ErrInvalid{Msg: "from must be yyyy-mm-dd"}
	}
	to, err := time.ParseInLocation("2006-01-02", toDay, s.loc)
	if err != nil {
		return nil, ErrInvalid{Msg: "to must be yyyy-mm-dd"}
	}
	if to.Before(from) {
		return nil, ErrInvalid{Msg: "to must not be before from"}
	}
	if int(to.Sub(from).Hours()/24)+1 > maxTrendDays {
		return nil, ErrInvalid{Msg: "time range exceeds 92 days"}
	}
	days, err := s.st.ListMLUsageDays(fromDay, toDay)
	if err != nil {
		return nil, err
	}
	byDay := map[string]*CostTrendRow{}
	var order []string
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		byDay[key] = &CostTrendRow{Day: key}
		order = append(order, key)
	}
	for _, d := range days {
		if row := byDay[d.Day]; row != nil {
			accrueTotals(&row.Totals, d)
		}
	}
	out := make([]CostTrendRow, 0, len(order))
	for _, key := range order {
		out = append(out, *byDay[key])
	}
	return out, nil
}

// DetailQuery 明细查询入参（日期为业务时区 yyyy-mm-dd，均可选：
// 缺省 from = 今天-90d（明细保留窗口），to = 今天）。
type DetailQuery struct {
	FromDay, ToDay   string
	Provider, Model  string
	Scenario, Status string
	OperationID      string
	BeforeSeq        uint64
	Limit, Offset    int
}

// CostsDetail 明细分页（最新在前）。
type CostsDetail struct {
	Items     []*store.MLUsageRecord `json:"items"`
	Total     int                    `json:"total"`
	Truncated bool                   `json:"truncated"` // 超过扫描上限截断
}

func (s *Service) CostsDetail(q DetailQuery) (*CostsDetail, error) {
	now := time.Now().In(s.loc)
	f := store.MLUsageFilter{
		Provider:    q.Provider,
		Model:       q.Model,
		Scenario:    q.Scenario,
		Status:      q.Status,
		OperationID: q.OperationID,
		BeforeSeq:   q.BeforeSeq,
		Limit:       q.Limit,
		Offset:      q.Offset,
	}
	if q.FromDay != "" {
		t, err := time.ParseInLocation("2006-01-02", q.FromDay, s.loc)
		if err != nil {
			return nil, ErrInvalid{Msg: "from must be yyyy-mm-dd"}
		}
		f.From = t
	} else {
		f.From = now.AddDate(0, 0, -90)
	}
	if q.ToDay != "" {
		t, err := time.ParseInLocation("2006-01-02", q.ToDay, s.loc)
		if err != nil {
			return nil, ErrInvalid{Msg: "to must be yyyy-mm-dd"}
		}
		f.To = t.AddDate(0, 0, 1).Add(-time.Nanosecond) // 含当天全天
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.To.Sub(f.From).Hours() > 24*400 {
		return nil, ErrInvalid{Msg: "time range exceeds 400 days"}
	}
	items, total, trunc, err := s.st.ListMLUsage(f)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []*store.MLUsageRecord{}
	}
	return &CostsDetail{Items: items, Total: total, Truncated: trunc}, nil
}

// CostsOperation 一次业务 operation 下的全部 provider call（时间序）。
func (s *Service) CostsOperation(operationID string) ([]*store.MLUsageRecord, error) {
	items, err := s.st.ListMLUsageByOperation(operationID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []*store.MLUsageRecord{}
	}
	return items, nil
}
