package mlops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"text/template"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// ErrInvalid 输入校验失败。
type ErrInvalid struct{ Msg string }

func (e ErrInvalid) Error() string { return "invalid mlops request: " + e.Msg }

// ErrNotFound 对象不存在。
type ErrNotFound struct{ ID string }

func (e ErrNotFound) Error() string { return fmt.Sprintf("mlops object %q not found", e.ID) }

// ErrConflict 版本并发冲突。
type ErrConflict struct {
	ID               string
	Expected, Actual int
}

func (e ErrConflict) Error() string {
	return fmt.Sprintf("version conflict on %q: expected active %d, got %d", e.ID, e.Expected, e.Actual)
}

// MessageInput / VariableInput / VersionInput API 入参。
type MessageInput struct {
	Role     string `json:"role"`
	Template string `json:"template"`
}

type VariableInput struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Note     string `json:"note"`
}

type VersionInput struct {
	Messages  []MessageInput  `json:"messages"`
	Variables []VariableInput `json:"variables,omitempty"`
	Note      string          `json:"note,omitempty"`
}

// SaveInput 保存新版本入参。
type SaveInput struct {
	ExpectedActiveVersion int          `json:"expected_active_version,omitempty"`
	Activate              bool         `json:"activate"`
	Version               VersionInput `json:"version"`
}

// RenderedMessage 预览渲染结果。
type RenderedMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// synthBuiltin 构造未物化内置场景的只读视图（v1 = 代码内置默认）。
func synthBuiltin(def *scenarioDef) *store.Prompt {
	return &store.Prompt{
		ID:            builtinPromptID(def.Key),
		Scenario:      def.Key,
		Name:          def.Name,
		ActiveVersion: 1,
		Builtin:       true,
		Materialized:  false,
		Versions:      []store.PromptVersion{builtinV1(def.Key)},
		UpdatedAt:     time.Now().UTC(),
	}
}

func builtinV1(scenario string) store.PromptVersion {
	v := store.PromptVersion{
		Version:   1,
		Note:      "代码内置默认",
		CreatedAt: time.Now().UTC(),
	}
	for _, m := range builtinMessages(scenario) {
		v.Messages = append(v.Messages, store.PromptMessage{Role: m.Role, Template: m.Template})
	}
	for _, pv := range builtinVariables(scenario) {
		v.Variables = append(v.Variables, store.PromptVariable{Name: pv.Name, Required: pv.Required, Note: pv.Note})
	}
	return v
}

// List 列出提示词：内置场景（含未物化的默认视图）+ 已落库的自定义。
func (s *Service) List() ([]*store.Prompt, error) {
	stored, err := s.st.ListPrompts()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*store.Prompt, len(stored))
	var customs []*store.Prompt
	for _, p := range stored {
		byID[p.ID] = p
		if !p.Builtin {
			customs = append(customs, p)
		}
	}
	out := make([]*store.Prompt, 0, len(stored))
	for i := range scenarioDefs {
		def := &scenarioDefs[i]
		if p := byID[builtinPromptID(def.Key)]; p != nil {
			out = append(out, p)
			continue
		}
		out = append(out, synthBuiltin(def))
	}
	out = append(out, customs...)
	return out, nil
}

// Get 读取单条（内置未物化返回合成默认视图）。
func (s *Service) Get(id string) (*store.Prompt, error) {
	p, err := s.st.GetPrompt(id)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return p, nil
	}
	for i := range scenarioDefs {
		if builtinPromptID(scenarioDefs[i].Key) == id {
			return synthBuiltin(&scenarioDefs[i]), nil
		}
	}
	return nil, ErrNotFound{id}
}

// resolveForWrite 取写入基准记录：已落库用库里的，内置未物化用合成 v1。
func (s *Service) resolveForWrite(id string) (*store.Prompt, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if !p.Builtin && len(p.Versions) == 0 {
		return nil, ErrNotFound{id}
	}
	return p, nil
}

// Create 创建自定义提示词（scenario=custom）。
func (s *Service) Create(name string, in VersionInput, operator string) (*store.Prompt, error) {
	if name == "" {
		return nil, ErrInvalid{"name is required"}
	}
	if err := s.validateVersion(ScenarioCustom, in); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p := &store.Prompt{
		ID:            fmt.Sprintf("pr-%d", now.UnixNano()),
		Scenario:      ScenarioCustom,
		Name:          name,
		ActiveVersion: 1,
		Versions:      []store.PromptVersion{toStoreVersion(1, in, operator, now)},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.st.PutPrompt(p); err != nil {
		return nil, err
	}
	s.audit("prompt", operator, "prompt_create", p.ID, "", hashVersion(p.Versions[0]), "ok", "")
	return p, nil
}

// SaveVersion 保存新版本。内置场景首次保存时物化（v1 = 内置默认拷贝），
// 新版本追加为 v(n+1)；Activate=true 时同时切换 active。
// ExpectedActiveVersion 非 0 时做并发冲突校验。
func (s *Service) SaveVersion(id string, in SaveInput, operator string) (*store.Prompt, error) {
	if err := s.validateVersion("", in.Version); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	base, err := s.resolveForWrite(id)
	if err != nil {
		return nil, err
	}
	if err := s.validateVersion(base.Scenario, in.Version); err != nil {
		return nil, err
	}
	if in.ExpectedActiveVersion != 0 && in.ExpectedActiveVersion != base.ActiveVersion {
		return nil, ErrConflict{ID: id, Expected: in.ExpectedActiveVersion, Actual: base.ActiveVersion}
	}

	before := hashActive(base)
	next := len(base.Versions) + 1
	base.Versions = append(base.Versions, toStoreVersion(next, in.Version, operator, time.Now().UTC()))
	base.Builtin = findScenarioDef(base.Scenario) != nil
	base.Materialized = base.Builtin
	if in.Activate {
		base.ActiveVersion = next
	}
	base.UpdatedAt = time.Now().UTC()
	if err := s.st.PutPrompt(base); err != nil {
		return nil, err
	}
	s.invalidate(base.Scenario)
	s.audit("prompt", operator, "prompt_save", id, before, hashActive(base), "ok", "")
	return base, nil
}

// Activate 激活/回滚到指定版本（内置场景 v1 = 代码内置默认）。
func (s *Service) Activate(id string, version, expected int, operator string) (*store.Prompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.resolveForWrite(id)
	if err != nil {
		return nil, err
	}
	if expected != 0 && expected != p.ActiveVersion {
		return nil, ErrConflict{ID: id, Expected: expected, Actual: p.ActiveVersion}
	}
	if findVersion(p, version) == nil {
		return nil, ErrInvalid{fmt.Sprintf("version %d not found", version)}
	}
	before := hashActive(p)
	p.ActiveVersion = version
	p.UpdatedAt = time.Now().UTC()
	if err := s.st.PutPrompt(p); err != nil {
		return nil, err
	}
	s.invalidate(p.Scenario)
	s.audit("prompt", operator, "prompt_activate", id, before, hashActive(p), "ok", "")
	return p, nil
}

// Reset 内置场景恢复代码默认：v1 刷新为当前内置默认并激活 v1
// （用户版本保留在历史里）。
func (s *Service) Reset(id string, operator string) (*store.Prompt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.resolveForWrite(id)
	if err != nil {
		return nil, err
	}
	if findScenarioDef(p.Scenario) == nil {
		return nil, ErrInvalid{"only builtin scenarios can be reset"}
	}
	if p.Scenario != "" && builtinPromptID(p.Scenario) != id {
		return nil, ErrInvalid{"builtin prompt id mismatch"}
	}
	before := hashActive(p)
	fresh := builtinV1(p.Scenario)
	fresh.CreatedBy = operator
	if v := findVersion(p, 1); v != nil {
		*v = fresh
	} else {
		p.Versions = append([]store.PromptVersion{fresh}, p.Versions...)
	}
	p.ActiveVersion = 1
	p.Materialized = true
	p.UpdatedAt = time.Now().UTC()
	if err := s.st.PutPrompt(p); err != nil {
		return nil, err
	}
	s.invalidate(p.Scenario)
	s.audit("prompt", operator, "prompt_reset", id, before, hashActive(p), "ok", "")
	return p, nil
}

// Delete 删除提示词（仅 custom）。
func (s *Service) Delete(id string, operator string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.st.GetPrompt(id)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound{id}
	}
	if p.Builtin {
		return ErrInvalid{"builtin scenarios cannot be deleted, use reset"}
	}
	if err := s.st.DeletePrompt(id); err != nil {
		return err
	}
	s.audit("prompt", operator, "prompt_delete", id, hashActive(p), "", "ok", "")
	return nil
}

// Preview 用给定数据渲染指定版本（version=0 用 active），不落库。
// 内置场景按场景数据结构解码 data；custom 场景按 map 解码并在缺 key 时报错。
func (s *Service) Preview(id string, version int, data json.RawMessage) ([]RenderedMessage, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		version = p.ActiveVersion
	}
	ver := findVersion(p, version)
	if ver == nil {
		return nil, ErrInvalid{fmt.Sprintf("version %d not found", version)}
	}

	var v any
	strict := false
	if def := findScenarioDef(p.Scenario); def != nil && def.Prototype != nil {
		proto := def.Prototype()
		if len(data) > 0 {
			if err := json.Unmarshal(data, proto); err != nil {
				return nil, ErrInvalid{"data does not match scenario schema: " + err.Error()}
			}
		}
		v = proto
	} else {
		m := map[string]any{}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, ErrInvalid{"data must be a JSON object: " + err.Error()}
			}
		}
		v = m
		strict = true // 自定义场景：缺失 key 显式报错
	}

	out := make([]RenderedMessage, 0, len(ver.Messages))
	for _, msg := range ver.Messages {
		t, err := template.New(p.Scenario).Parse(msg.Template)
		if err != nil {
			return nil, ErrInvalid{"template syntax error: " + err.Error()}
		}
		if strict {
			t = t.Option("missingkey=error")
		}
		text, err := execLimited(t, v)
		if err != nil {
			return nil, ErrInvalid{"render failed: " + err.Error()}
		}
		out = append(out, RenderedMessage{Role: msg.Role, Content: text})
	}
	return out, nil
}

// validateVersion 校验版本内容：角色、条数、大小、语法，已知场景加零值
// 试渲染（捕获未知变量/运行时错误——「空变量试渲染」）。scenario 为空时
// 只做语法级校验（SaveVersion 先校验语法再用基准记录的场景补全校验）。
func (s *Service) validateVersion(scenario string, in VersionInput) error {
	if len(in.Messages) == 0 {
		return ErrInvalid{"at least one message is required"}
	}
	if len(in.Messages) > maxMessages {
		return ErrInvalid{fmt.Sprintf("too many messages (max %d)", maxMessages)}
	}
	total := 0
	for _, m := range in.Messages {
		if m.Role != "system" && m.Role != "user" {
			return ErrInvalid{"message role must be system|user"}
		}
		if m.Template == "" {
			return ErrInvalid{"message template is empty"}
		}
		if len(m.Template) > maxTemplateBytes {
			return ErrInvalid{fmt.Sprintf("message template exceeds %d bytes", maxTemplateBytes)}
		}
		total += len(m.Template)
	}
	if total > maxTotalTemplate {
		return ErrInvalid{fmt.Sprintf("total template exceeds %d bytes", maxTotalTemplate)}
	}

	def := findScenarioDef(scenario)
	for _, m := range in.Messages {
		t, err := template.New(scenario).Parse(m.Template)
		if err != nil {
			return ErrInvalid{"template syntax error: " + err.Error()}
		}
		if def == nil || def.Prototype == nil {
			continue // custom：无法预知数据结构，仅语法校验（预览时严格）
		}
		if _, err := execLimited(t, def.Prototype()); err != nil {
			return ErrInvalid{fmt.Sprintf("template execution failed on zero-value data (unknown variable?): %v", err)}
		}
	}
	return nil
}

func toStoreVersion(version int, in VersionInput, operator string, now time.Time) store.PromptVersion {
	v := store.PromptVersion{Version: version, Note: in.Note, CreatedAt: now, CreatedBy: operator}
	for _, m := range in.Messages {
		v.Messages = append(v.Messages, store.PromptMessage{Role: m.Role, Template: m.Template})
	}
	for _, pv := range in.Variables {
		v.Variables = append(v.Variables, store.PromptVariable{Name: pv.Name, Required: pv.Required, Note: pv.Note})
	}
	return v
}

// hashActive active 版本内容的 hash 摘要（审计留痕，不落模板正文）。
func hashActive(p *store.Prompt) string {
	if v := findVersion(p, p.ActiveVersion); v != nil {
		return hashVersion(*v)
	}
	return ""
}

func hashVersion(v store.PromptVersion) string {
	b, _ := json.Marshal(v.Messages)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// audit 写管理审计（失败仅记日志，不阻断业务）。
func (s *Service) audit(objectType, operator, action, objectID, before, after, result, errMsg string) {
	seq, err := s.st.NextSeq("mlopsaudit")
	if err != nil {
		s.logger.Printf("mlops audit nextseq: %v", err)
		return
	}
	a := &store.MLOpsAudit{
		ID:         fmt.Sprintf("ma-%d", time.Now().UnixNano()),
		Seq:        seq,
		Operator:   operator,
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		BeforeHash: before,
		AfterHash:  after,
		Result:     result,
		Error:      errMsg,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.st.SaveMLOpsAudit(a); err != nil {
		s.logger.Printf("mlops audit save: %v", err)
	}
}

// Audits 列出管理审计（最新在前）。
func (s *Service) Audits(limit int) ([]*store.MLOpsAudit, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.st.ListMLOpsAudits(limit)
}
