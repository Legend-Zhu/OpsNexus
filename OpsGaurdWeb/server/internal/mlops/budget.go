// 月度预算（MLOps P3，方案 §4.8）。超限只告警不拦截 LLM 请求。
// 检查在计量入账后异步触发（限频：每分钟至多一次全量对账），档位
// （warn_at 与 100%）以月份+档位在 Store 原子去重，重复入账/重启不重复
// 通知。通知经 NotifySender（notify.Service 适配）发送并留发送记录。
package mlops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// budgetCheckInterval 预算对账最小间隔（入账频繁时限频，避免每次调用
// 都重读日聚合）。
const budgetCheckInterval = time.Minute

// SetNotifier 注入预算通知发送器（main 装配 notify.Service 适配）。
func (s *Service) SetNotifier(n NotifySender) { s.notifier = n }

// SaveBudget 保存（覆盖）指定月份预算。
func (s *Service) SaveBudget(month string, limitMinor int64, warnAt float64, operator string) (*store.MLBudget, error) {
	month = strings.TrimSpace(month)
	if err := validMonth(month); err != nil {
		return nil, ErrInvalid{Msg: err.Error()}
	}
	if limitMinor <= 0 {
		return nil, ErrInvalid{Msg: "limit_minor must be positive"}
	}
	if limitMinor > 1<<53 {
		return nil, ErrInvalid{Msg: "limit_minor too large"}
	}
	if warnAt <= 0 || warnAt >= 1 {
		return nil, ErrInvalid{Msg: "warn_at must be in (0,1), e.g. 0.8"}
	}
	before, _ := s.st.GetMLBudget(month)
	b := &store.MLBudget{
		Month:      month,
		Currency:   s.currency,
		LimitMinor: limitMinor,
		WarnAt:     warnAt,
		Notified:   nil, // 新预算重置通知状态（改预算 = 重新告警）
		UpdatedBy:  operator,
		UpdatedAt:  time.Now().UTC(),
	}
	if err := s.st.SaveMLBudget(b); err != nil {
		return nil, err
	}
	bh, ah := "", fmt.Sprintf("limit=%d warn=%.2f", limitMinor, warnAt)
	if before != nil {
		bh = fmt.Sprintf("limit=%d warn=%.2f", before.LimitMinor, before.WarnAt)
	}
	s.audit("budget", operator, "budget_save", month, bh, ah, "ok", "")
	return b, nil
}

// DeleteBudget 删除指定月份预算。
func (s *Service) DeleteBudget(month, operator string) error {
	if err := validMonth(month); err != nil {
		return ErrInvalid{Msg: err.Error()}
	}
	b, err := s.st.GetMLBudget(month)
	if err != nil {
		return err
	}
	if b == nil {
		return ErrNotFound{ID: "budget " + month}
	}
	if err := s.st.DeleteMLBudget(month); err != nil {
		return err
	}
	s.audit("budget", operator, "budget_delete", month, fmt.Sprintf("limit=%d", b.LimitMinor), "", "ok", "")
	return nil
}

// BudgetView 预算视图（含当月实际用量）。
type BudgetView struct {
	Month      string    `json:"month"`
	Currency   string    `json:"currency"`
	LimitMinor int64     `json:"limit_minor"`
	WarnAt     float64   `json:"warn_at"`
	Notified   []float64 `json:"notified"`
	UpdatedBy  string    `json:"updated_by,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	// SpendMinor 当月已计价金额；UnpricedCalls 未计价调用数（金额不可用）。
	SpendMinor    int64   `json:"spend_minor"`
	UnpricedCalls int64   `json:"unpriced_calls"`
	UsageRatio    float64 `json:"usage_ratio"` // spend/limit（无预算为 0）
}

// ListBudgets 列出预算及当月实际用量。
func (s *Service) ListBudgets() ([]BudgetView, error) {
	budgets, err := s.st.ListMLBudgets()
	if err != nil {
		return nil, err
	}
	month := time.Now().In(s.loc).Format("2006-01")
	var monthTotals CostTotals
	if days, err := s.st.ListMLUsageDays(month+"-01", month+"-31"); err == nil {
		acc := CostTotals{}
		for _, d := range days {
			accrueTotals(&acc, d)
		}
		monthTotals = acc
	}
	out := make([]BudgetView, 0, len(budgets))
	for _, b := range budgets {
		v := BudgetView{
			Month: b.Month, Currency: b.Currency, LimitMinor: b.LimitMinor,
			WarnAt: b.WarnAt, Notified: b.Notified, UpdatedBy: b.UpdatedBy,
			UpdatedAt: b.UpdatedAt,
		}
		v.Notified = append([]float64(nil), b.Notified...)
		if b.Month == month {
			v.SpendMinor = monthTotals.CostMinor
			v.UnpricedCalls = monthTotals.Calls - monthTotals.PricedCalls
			if b.LimitMinor > 0 {
				v.UsageRatio = float64(v.SpendMinor) / float64(b.LimitMinor)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// budgetLastCheck 预算对账限频时间戳（unix nano，CAS 更新）。
var budgetLastCheck atomic.Int64

// maybeCheckBudget 入账后触发预算检查（限频；失败只留痕不影响计量）。
func (s *Service) maybeCheckBudget() {
	now := time.Now().UnixNano()
	last := budgetLastCheck.Load()
	if now-last < int64(budgetCheckInterval) {
		return
	}
	if !budgetLastCheck.CompareAndSwap(last, now) {
		return // 并发触发只放一个
	}
	if err := s.checkBudget(context.Background()); err != nil {
		s.logger.Printf("budget check: %v", err)
	}
}

// checkBudget 对账当月预算：金额/档位达到未通知档位时发送通知并原子补记。
func (s *Service) checkBudget(ctx context.Context) error {
	month := time.Now().In(s.loc).Format("2006-01")
	b, err := s.st.GetMLBudget(month)
	if err != nil {
		return err
	}
	if b == nil || b.LimitMinor <= 0 {
		return nil // 未设预算
	}
	days, err := s.st.ListMLUsageDays(month+"-01", month+"-31")
	if err != nil {
		return err
	}
	var spend, calls, priced int64
	for _, d := range days {
		spend += d.CostMinor
		calls += d.Calls
		priced += d.PricedCalls
	}
	ratio := float64(spend) / float64(b.LimitMinor)

	tiers := []float64{b.WarnAt, 1.0}
	if tiers[0] > tiers[1] { // 防御：warn_at 不可能 >1（保存已校验）
		tiers = []float64{1.0}
	}
	sort.Float64s(tiers)
	for _, tier := range tiers {
		if ratio+1e-9 < tier {
			continue
		}
		notified, err := s.st.MarkBudgetNotified(month, tier)
		if err != nil {
			return fmt.Errorf("mark budget tier %.0f%%: %w", tier*100, err)
		}
		if !notified {
			continue // 已通知过
		}
		s.sendBudgetNotify(ctx, b, month, spend, calls-priced, tier)
	}
	return nil
}

// sendBudgetNotify 发送一档预算通知（含月份/金额/预算/档位/未计价数）。
func (s *Service) sendBudgetNotify(ctx context.Context, b *store.MLBudget, month string, spendMinor, unpriced int64, tier float64) {
	title := fmt.Sprintf("MLOps 预算告警：%s 已达 %.0f%%", month, tier*100)
	content := fmt.Sprintf(
		"月份：%s（业务时区 %s）\n当前已计价金额：%s %s\n预算上限：%s %s\n触发档位：%.0f%%\n未计价调用：%d 次（未配单价或无 usage，未计入金额）",
		month, s.loc.String(),
		MicroToDecimal(spendMinor), b.Currency,
		MicroToDecimal(b.LimitMinor), b.Currency,
		tier*100, unpriced,
	)
	if s.notifier == nil {
		s.logger.Printf("budget notify (no notifier): %s", title)
		return
	}
	ids := s.notifier.EnabledChannelIDs()
	if len(ids) == 0 {
		s.logger.Printf("budget notify skipped: no enabled channels (%s)", title)
		return
	}
	if err := s.notifier.Send(ctx, ids, title, content); err != nil {
		// 档位已补记，发送失败留痕即可（发送记录由 notify 侧逐渠道落库）
		s.logger.Printf("budget notify send failed (%s): %v", title, err)
	}
}

// validMonth 校验 yyyy-mm。
func validMonth(m string) error {
	if len(m) != 7 || m[4] != '-' {
		return fmt.Errorf("month must be yyyy-mm")
	}
	if _, err := time.Parse("2006-01", m); err != nil {
		return fmt.Errorf("month must be yyyy-mm")
	}
	return nil
}
