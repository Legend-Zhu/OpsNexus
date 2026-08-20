package mlops

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// 保存/渲染限制（方案 §3.4 模板安全约束）。
const (
	maxMessages      = 8        // 单版本消息条数上限
	maxTemplateBytes = 64 << 10 // 单条消息模板上限
	maxTotalTemplate = 256 << 10
	maxRenderBytes   = 1 << 20 // 单次渲染输出上限
)

// compiled 一个场景 active 版本的编译缓存。
type compiled struct {
	promptID string
	version  int
	msgs     []compiledMsg
}

type compiledMsg struct {
	role string
	text string
	tpl  *template.Template
}

// Service MLOps 运营层服务（P1：Prompt Hub；P2：provider call 用量费用）。
type Service struct {
	st     *store.Store
	logger *log.Logger

	// mu 串行化 Prompt 记录的读-改-写（保存版本/激活/重置/删除）。
	mu sync.Mutex

	cacheMu sync.RWMutex
	cache   map[string]*compiled // scenario -> active 编译缓存

	// fallbacks 线上 active 模板渲染失败回退内置默认的次数（可观测）。
	fallbacks atomic.Int64

	// ---- P2：provider call 用量计量（异步 collector，实现 usage.Sink）----
	usageMu      sync.Mutex        // 保护 collector 启停状态与队列引用
	usageQueue   chan usage.Record // 有界队列；满时丢弃并计数，绝不阻塞响应
	usageStop    chan struct{}
	usageWG      sync.WaitGroup
	usageRunning bool
	loc          *time.Location // 业务时区（日/月边界口径）
	currency     string         // 当前固定 CNY
	retainDays   int            // 明细保留天数
	gcInterval   time.Duration

	usageDropped atomic.Int64 // 队列满丢弃数
	usageDeduped atomic.Int64 // call_id 重复跳过数（幂等命中）
	usageFailed  atomic.Int64 // 落库失败数

	opDayMu sync.Mutex
	opDay   map[string]string // operation_id -> 最近入账日（当日 operation 去重估算）

	pricingMu    sync.RWMutex
	pricingCache map[string]*store.MLPricing // 含 nil 值（未配价缓存负查）
}

// New 创建 MLOps 服务。
func New(st *store.Store) *Service {
	return &Service{
		st:           st,
		logger:       log.New(os.Stderr, "[OpsGaurdWeb.MLOps] ", log.LstdFlags|log.Lshortfile),
		cache:        make(map[string]*compiled),
		opDay:        make(map[string]string),
		pricingCache: make(map[string]*store.MLPricing),
		loc:          time.Local,
		currency:     "CNY",
	}
}

// Fallbacks 返回线上模板渲染失败回退次数（诊断/报表用）。
func (s *Service) Fallbacks() int64 { return s.fallbacks.Load() }

// ScenarioTemplate 返回场景 active 版本首条消息的模板原文。
// 实现 server.PromptSource 与 agent.ScenarioTemplateSource；
// ok=false（未自定义）时调用方使用代码内置默认。
func (s *Service) ScenarioTemplate(scenario string) (string, bool) {
	c := s.compiledFor(scenario)
	if c == nil || len(c.msgs) == 0 {
		return "", false
	}
	return c.msgs[0].text, true
}

// InvestigateMessages 渲染深度排查的 system+user 消息。
// 任一场景已自定义即走模板（另一侧用内置 v1 模板渲染，二者字节等价由
// 回归测试保证）；未自定义返回 ok=false，调用方回退代码内置组装。
// 渲染失败：留痕（日志 + fallbacks 计数）并回退，不阻断排查。
func (s *Service) InvestigateMessages(data InvestigateData) ([]map[string]any, bool) {
	sysTpl := builtinParsed(ScenarioInvestigateSystem)
	usrTpl := builtinParsed(ScenarioInvestigateUser)
	customized := false
	if c := s.compiledFor(ScenarioInvestigateSystem); c != nil && len(c.msgs) > 0 {
		sysTpl = c.msgs[0].tpl
		customized = true
	}
	if c := s.compiledFor(ScenarioInvestigateUser); c != nil && len(c.msgs) > 0 {
		usrTpl = c.msgs[0].tpl
		customized = true
	}
	if !customized {
		return nil, false
	}
	sysText, errSys := execLimited(sysTpl, &data)
	usrText, errUsr := execLimited(usrTpl, &data)
	if errSys != nil || errUsr != nil {
		s.logger.Printf("investigate prompt render failed (system=%v user=%v), falling back to builtin", errSys, errUsr)
		s.fallbacks.Add(1)
		return nil, false
	}
	return []map[string]any{
		{"role": "system", "content": sysText},
		{"role": "user", "content": usrText},
	}, true
}

// RenderBuiltinMessages 用内置 v1 模板渲染单消息场景（等价性回归测试用：
// 验证模板路径与代码内置组装逐字节一致；线上混合路径内部同源）。
func RenderBuiltinMessages(scenario string, data any) (string, error) {
	return execLimited(builtinParsed(scenario), data)
}

// builtinParsed 内置模板的解析缓存（进程级，启动即校验语法）。
var builtinCache sync.Map // scenario -> *template.Template

func builtinParsed(scenario string) *template.Template {
	if v, ok := builtinCache.Load(scenario); ok {
		return v.(*template.Template)
	}
	msgs := builtinMessages(scenario)
	if len(msgs) == 0 {
		return nil
	}
	t := template.Must(template.New(scenario).Parse(msgs[0].Template))
	builtinCache.Store(scenario, t)
	return t
}

// compiledFor 读取场景 active 版本的编译缓存；未自定义/解析失败返回 nil。
func (s *Service) compiledFor(scenario string) *compiled {
	if findScenarioDef(scenario) == nil {
		return nil // custom 场景不参与引擎解析
	}
	s.cacheMu.RLock()
	c := s.cache[scenario]
	s.cacheMu.RUnlock()
	if c != nil {
		return c
	}

	p, err := s.st.GetPrompt(builtinPromptID(scenario))
	if err != nil {
		s.logger.Printf("load prompt %s: %v", scenario, err)
		return nil
	}
	if p == nil {
		return nil // 未自定义
	}
	ver := findVersion(p, p.ActiveVersion)
	if ver == nil {
		s.logger.Printf("prompt %s active version %d missing", scenario, p.ActiveVersion)
		return nil
	}
	nc := &compiled{promptID: p.ID, version: ver.Version}
	for _, m := range ver.Messages {
		t, err := template.New(scenario).Parse(m.Template)
		if err != nil {
			s.logger.Printf("prompt %s v%d parse failed (%v), using builtin", scenario, ver.Version, err)
			s.fallbacks.Add(1)
			return nil
		}
		nc.msgs = append(nc.msgs, compiledMsg{role: m.Role, text: m.Template, tpl: t})
	}
	// active 内容与内置默认一致 → 视同未自定义：调用方直接走代码内置路径，
	// 保证「回滚 v1 / 还原默认」后行为与从未自定义完全一致。
	if sameAsBuiltin(scenario, nc) {
		return nil
	}
	s.cacheMu.Lock()
	s.cache[scenario] = nc
	s.cacheMu.Unlock()
	return nc
}

// sameAsBuiltin active 版本消息序列与内置默认是否完全一致。
func sameAsBuiltin(scenario string, c *compiled) bool {
	bm := builtinMessages(scenario)
	if len(bm) != len(c.msgs) {
		return false
	}
	for i := range bm {
		if c.msgs[i].role != bm[i].Role || c.msgs[i].text != bm[i].Template {
			return false
		}
	}
	return true
}

// invalidate 场景缓存失效（保存/激活/重置/删除后调用）。
func (s *Service) invalidate(scenario string) {
	s.cacheMu.Lock()
	delete(s.cache, scenario)
	s.cacheMu.Unlock()
}

// execLimited 执行模板并限制输出大小。
func execLimited(t *template.Template, data any) (string, error) {
	if t == nil {
		return "", fmt.Errorf("nil template")
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	if b.Len() > maxRenderBytes {
		return "", fmt.Errorf("rendered output exceeds %d bytes", maxRenderBytes)
	}
	return b.String(), nil
}

func builtinPromptID(scenario string) string { return "sc-" + scenario }

func findVersion(p *store.Prompt, version int) *store.PromptVersion {
	for i := range p.Versions {
		if p.Versions[i].Version == version {
			return &p.Versions[i]
		}
	}
	return nil
}
