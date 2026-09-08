// 用量统计：token×tool 调用计数。计数点在 /mcp 的 usage 中间件（解析
// JSON-RPC 的 tools/call 方法名），全部工具零埋点覆盖；写工具的失败原因
// 见审计（mcp_audit）。内存聚合 + 30s 周期落盘（mcp_usage 日聚合）。
package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

const usageFlushInterval = 30 * time.Second

type usageTable struct {
	mu   sync.Mutex
	inc  map[string]int64 // 未落盘增量：day|actor|tool -> count
	stop chan struct{}
	done chan struct{}
}

func newUsageTable() *usageTable {
	return &usageTable{inc: map[string]int64{}, stop: make(chan struct{}), done: make(chan struct{})}
}

// record 记一次工具调用。
func (u *usageTable) record(day, actor, tool string) {
	k := day + "|" + actor + "|" + tool
	u.mu.Lock()
	u.inc[k]++
	u.mu.Unlock()
}

// drain 取走全部增量。
func (u *usageTable) drain() map[string]int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.inc
	u.inc = map[string]int64{}
	return out
}

// Start 启动周期 flush。
func (u *usageTable) Start(h *Handler) {
	go func() {
		defer close(u.done)
		t := time.NewTicker(usageFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-u.stop:
				h.flushUsage()
				return
			case <-t.C:
				h.flushUsage()
			}
		}
	}()
}

// Stop 停止并做最后一次 flush。
func (u *usageTable) Stop(h *Handler) {
	close(u.stop)
	<-u.done
}

// flushUsage 落盘增量（按天分组）。
func (h *Handler) flushUsage() {
	inc := h.usage.drain()
	if len(inc) == 0 || h.deps.Store == nil {
		return
	}
	byDay := map[string]map[string]int64{}
	for k, v := range inc {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			continue
		}
		day := parts[0]
		if byDay[day] == nil {
			byDay[day] = map[string]int64{}
		}
		byDay[day][parts[1]+"|"+parts[2]] += v
	}
	for day, m := range byDay {
		if err := h.deps.Store.AddMCPUsage(day, m); err != nil {
			h.log.Error("mcp usage flush failed", "day", day, "err", err)
		}
	}
}

// UsageRow 用量查询行（actor×tool 聚合）。
type UsageRow struct {
	Actor string `json:"actor"`
	Tool  string `json:"tool"`
	Calls int64  `json:"calls"`
}

// UsageSummary 用量查询结果。
type UsageSummary struct {
	Days int        `json:"days"`
	Rows []UsageRow `json:"rows"`
}

// Usage 用量查询（管理 API 用）。
func (h *Handler) Usage(days int) UsageSummary { return h.usageSummary(days) }

// usageSummary 汇总最近 days 天（含当日未落盘增量）。
func (h *Handler) usageSummary(days int) UsageSummary {
	if days <= 0 {
		days = 7
	}
	sum := UsageSummary{Days: days, Rows: []UsageRow{}}
	if h.deps.Store == nil {
		return sum
	}
	dayList, err := h.deps.Store.ListMCPUsage(days)
	if err != nil {
		h.log.Error("mcp usage read failed", "err", err)
		return sum
	}
	agg := map[string]*UsageRow{}
	apply := func(day string, counts map[string]int64) {
		for k, v := range counts {
			parts := strings.SplitN(k, "|", 2)
			if len(parts) != 2 {
				continue
			}
			key := parts[0] + "|" + parts[1]
			row := agg[key]
			if row == nil {
				row = &UsageRow{Actor: parts[0], Tool: parts[1]}
				agg[key] = row
			}
			row.Calls += v
		}
	}
	for _, d := range dayList {
		apply(d.Day, d.Counts)
	}
	// 当日未落盘增量
	today := store.UsageDay(time.Now(), bizLocation())
	h.usage.mu.Lock()
	pending := make(map[string]int64)
	for k, v := range h.usage.inc {
		p := strings.SplitN(k, "|", 3)
		if len(p) == 3 && p[0] == today {
			pending[p[1]+"|"+p[2]] += v
		}
	}
	h.usage.mu.Unlock()
	apply(today, pending)

	for _, row := range agg {
		sum.Rows = append(sum.Rows, *row)
	}
	sort.Slice(sum.Rows, func(i, j int) bool { return sum.Rows[i].Calls > sum.Rows[j].Calls })
	if len(sum.Rows) > 200 {
		sum.Rows = sum.Rows[:200]
	}
	return sum
}

// bizLocation 业务时区（与巡检/mlops 默认一致）。
func bizLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Local
	}
	return loc
}

// usageMW 工具调用计数中间件：从 JSON-RPC 报文中识别 tools/call 的工具名。
// 只读不阻断——解析失败按未计调用处理。挂在鉴权中间件之后（actor 可用）。
func (h *Handler) usageMW() gin.HandlerFunc {
	var peek struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodPost && c.Request.Body != nil {
			body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
			if err == nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
				if json.Unmarshal(body, &peek) == nil && peek.Method == "tools/call" && peek.Params.Name != "" {
					actor := "unknown"
					if id := identityFrom(c.Request.Context()); id != nil {
						actor = id.Name
					}
					h.usage.record(store.UsageDay(time.Now(), bizLocation()), actor, peek.Params.Name)
				}
			}
		}
		c.Next()
	}
}
