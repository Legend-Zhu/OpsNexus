// 场景模型绑定 + 模型健康缓存（MLOps P3，方案 §5.3/§5.4）。
// 绑定只影响之后的新 operation；绑定模型不可路由时由运行时回退默认
// 可用并留痕（见 ainexusrt.Service）。健康缓存只存最近一次显式测试
// 结果（内存，重启清空）；GET 不触发真实请求。
package mlops

import (
	"sort"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// bindableScenarios 支持绑定的业务场景（与 usage 计量场景一致；
// compress/health 为内部调用不开放绑定）。
var bindableScenarios = map[string]bool{
	"chat": true, "investigate": true, "native_chat": true, "patrol_report": true,
}

// ModelUsage 单模型用量合计。
type ModelUsage struct {
	Calls     int64
	CostMinor int64
}

// MonthModelUsage 本月（业务时区）按 provider+model 的用量合计（模型运营视图）。
func (s *Service) MonthModelUsage() map[string]ModelUsage {
	month := time.Now().In(s.loc).Format("2006-01")
	out := map[string]ModelUsage{}
	days, err := s.st.ListMLUsageDays(month+"-01", month+"-31")
	if err != nil {
		return out
	}
	for _, d := range days {
		key := d.Provider + "\x00" + d.Model
		u := out[key]
		u.Calls += d.Calls
		u.CostMinor += d.CostMinor
		out[key] = u
	}
	return out
}

// AuditModelToggle 模型启停审计留痕（由 API 层在热重载成功后调用）。
func (s *Service) AuditModelToggle(operator, provider, model string, enabled bool) {
	action := "model_disable"
	if enabled {
		action = "model_enable"
	}
	s.audit("model", operator, action, provider+"/"+model, "", "", "ok", "")
}

// ScenarioModel 实现 ainexusrt.ScenarioModelBinder：返回场景绑定的模型。
func (s *Service) ScenarioModel(scenario string) (string, bool) {
	b, err := s.st.GetScenarioBinding(scenario)
	if err != nil || b == nil {
		return "", false
	}
	return b.Model, true
}

// ListBindings 列出全部场景绑定。
func (s *Service) ListBindings() ([]*store.ScenarioBinding, error) {
	return s.st.ListScenarioBindings()
}

// SaveBinding 保存场景绑定（覆盖）。模型存在性由网关配置决定，这里只做
// 基础校验；绑定到不存在/被禁用的模型时运行时回退默认并留痕。
func (s *Service) SaveBinding(scenario, model, operator string) (*store.ScenarioBinding, error) {
	scenario = strings.TrimSpace(scenario)
	model = strings.TrimSpace(model)
	if !bindableScenarios[scenario] {
		return nil, ErrInvalid{Msg: "scenario must be one of chat/investigate/native_chat/patrol_report"}
	}
	if model == "" {
		return nil, ErrInvalid{Msg: "model is required (use delete to unbind)"}
	}
	if len(model) > 128 {
		return nil, ErrInvalid{Msg: "model too long (max 128)"}
	}
	before, _ := s.st.GetScenarioBinding(scenario)
	b := &store.ScenarioBinding{
		Scenario:  scenario,
		Model:     model,
		UpdatedBy: operator,
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.st.SaveScenarioBinding(b); err != nil {
		return nil, err
	}
	bh, ah := "", model
	if before != nil {
		bh = before.Model
	}
	s.audit("binding", operator, "binding_save", scenario, bh, ah, "ok", "")
	return b, nil
}

// DeleteBinding 删除场景绑定（回退默认可用模型）。
func (s *Service) DeleteBinding(scenario, operator string) error {
	if !bindableScenarios[scenario] {
		return ErrInvalid{Msg: "unknown scenario"}
	}
	b, err := s.st.GetScenarioBinding(scenario)
	if err != nil {
		return err
	}
	if b == nil {
		return ErrNotFound{ID: "binding " + scenario}
	}
	if err := s.st.DeleteScenarioBinding(scenario); err != nil {
		return err
	}
	s.audit("binding", operator, "binding_delete", scenario, b.Model, "", "ok", "")
	return nil
}

// --- 模型健康缓存 ---

// HealthResult 最近一次显式健康测试结果（内存缓存，重启清空）。
type HealthResult struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	OK        bool      `json:"ok"`
	LatencyMs int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	TestedAt  time.Time `json:"tested_at"`
}

// RecordHealth 记录一次显式测试结果（覆盖同模型旧结果）。
func (s *Service) RecordHealth(r HealthResult) {
	s.healthMu.Lock()
	if s.health == nil {
		s.health = make(map[string]HealthResult)
	}
	s.health[r.Provider+"/"+r.Model] = r
	s.healthMu.Unlock()
}

// HealthResults 全部缓存的健康结果（provider/model 排序稳定）。
func (s *Service) HealthResults() []HealthResult {
	s.healthMu.Lock()
	out := make([]HealthResult, 0, len(s.health))
	for _, r := range s.health {
		out = append(out, r)
	}
	s.healthMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}
