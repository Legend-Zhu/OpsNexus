// Package patrol implements YAML-declared inspection flows (P5): flow CRUD,
// single-instance cron scheduling, and execution that runs checks against
// cluster Workers, collects anomalies, and generates an AI report through the
// embedded AiNexus gateway.
package patrol

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// Flow 巡检流程定义（YAML 内嵌结构，与 store.Patrol.YAML 对应）。
type Flow struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description,omitempty"`
	Checks      []Check `yaml:"checks"`
	Report      struct {
		Model  string `yaml:"model,omitempty"`
		Prompt string `yaml:"prompt,omitempty"`
	} `yaml:"report"`
}

// Check 单个检查项。
type Check struct {
	Type    string  `yaml:"type"` // resource | health
	Cluster string  `yaml:"cluster"`
	Service string  `yaml:"service"`
	// resource 阈值
	CPUThreshold float64 `yaml:"cpu_threshold,omitempty"` // 百分比
	MemThreshold float64 `yaml:"mem_threshold,omitempty"`
	// health 期望副本（0=全部）
	MinReplicas int `yaml:"min_replicas,omitempty"`
}

// ErrInvalidFlow 流程定义校验失败。
type ErrInvalidFlow struct{ Msg string }

func (e ErrInvalidFlow) Error() string { return "invalid patrol flow: " + e.Msg }

// ErrNotFound 流程不存在。
type ErrNotFound struct{ ID string }

func (e ErrNotFound) Error() string { return fmt.Sprintf("patrol %q not found", e.ID) }

// ParseFlow 解析并校验 YAML 流程定义。
func ParseFlow(yamlText string) (*Flow, error) {
	if strings.TrimSpace(yamlText) == "" {
		return nil, ErrInvalidFlow{"yaml is empty"}
	}
	var f Flow
	if err := yaml.Unmarshal([]byte(yamlText), &f); err != nil {
		return nil, ErrInvalidFlow{err.Error()}
	}
	if f.Name == "" {
		return nil, ErrInvalidFlow{"name is required"}
	}
	if len(f.Checks) == 0 {
		return nil, ErrInvalidFlow{"at least one check is required"}
	}
	for i, c := range f.Checks {
		switch c.Type {
		case "resource", "health":
		default:
			return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].type must be resource|health, got %q", i, c.Type)}
		}
		if c.Cluster == "" {
			return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].cluster is required", i)}
		}
		if c.Service == "" {
			return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].service is required", i)}
		}
	}
	return &f, nil
}

// ValidCron 校验 5 字段 cron 表达式。
func ValidCron(expr string) error {
	if expr == "" {
		return ErrInvalidFlow{"cron is required"}
	}
	_, err := cron.ParseStandard(expr)
	if err != nil {
		return ErrInvalidFlow{"invalid cron " + expr + ": " + err.Error()}
	}
	return nil
}

// Service 巡检服务。
type Service struct {
	st      *store.Store
	clusters *cluster.Service
	ainx    *server.Server // 内嵌 AiNexus（可为 nil，无报告生成）
	sched   *cron.Cron
	// onRun 调度触发的执行（供测试注入/替换）。
	onRun func(patrolID string)
}

// New 创建巡检服务。sched 为 nil 时自动创建（单实例调度器）。
func New(st *store.Store, clusters *cluster.Service, ainx *server.Server) *Service {
	s := &Service{st: st, clusters: clusters, ainx: ainx}
	// 5 字段标准 cron（与 ValidCron 的 ParseStandard 一致）
	s.sched = cron.New()
	s.onRun = func(id string) { _, _ = s.Run(context.Background(), id) }
	return s
}

// Start 启动调度器并加载已启用的流程。
func (s *Service) Start() { s.Reload(); s.sched.Start() }

// Stop 停止调度器。
func (s *Service) Stop() { s.sched.Stop() }

// Reload 重载调度（CRUD 后调用）：清空并重新注册所有 enabled 流程。
func (s *Service) Reload() {
	ctx := s.sched.Stop() // 停止并等待已开始的作业结束（单实例）
	_ = ctx
	s.sched = cron.New()
	patrols, err := s.st.ListPatrols()
	if err != nil {
		return
	}
	for _, p := range patrols {
		if !p.Enabled {
			continue
		}
		id := p.ID
		if _, err := s.sched.AddFunc(p.Cron, func() { s.onRun(id) }); err != nil {
			continue
		}
	}
	s.sched.Start()
}

// --- CRUD ---

// List 列出全部流程。
func (s *Service) List() ([]*store.Patrol, error) { return s.st.ListPatrols() }

// Get 读取单个流程。
func (s *Service) Get(id string) (*store.Patrol, error) {
	p, err := s.st.GetPatrol(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrNotFound{ID: id}
	}
	return p, nil
}

// Create 创建流程（校验 YAML + cron）。
func (s *Service) Create(name, description, cronExpr, yamlText string, enabled bool) (*store.Patrol, error) {
	if _, err := ParseFlow(yamlText); err != nil {
		return nil, err
	}
	if err := ValidCron(cronExpr); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p := &store.Patrol{
		ID:          newPatrolID(),
		Name:        name,
		Description: description,
		Cron:        cronExpr,
		Enabled:     enabled,
		YAML:        yamlText,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.st.PutPatrol(p); err != nil {
		return nil, err
	}
	s.Reload()
	return p, nil
}

// Update 更新流程（name/description/cron/enabled/yaml）。
func (s *Service) Update(id string, name, description, cronExpr, yamlText string, enabled bool) (*store.Patrol, error) {
	p, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if yamlText != "" {
		if _, err := ParseFlow(yamlText); err != nil {
			return nil, err
		}
		p.YAML = yamlText
	}
	if cronExpr != "" {
		if err := ValidCron(cronExpr); err != nil {
			return nil, err
		}
		p.Cron = cronExpr
	}
	if name != "" {
		p.Name = name
	}
	p.Description = description
	p.Enabled = enabled
	p.UpdatedAt = time.Now().UTC()
	if err := s.st.PutPatrol(p); err != nil {
		return nil, err
	}
	s.Reload()
	return p, nil
}

// Delete 删除流程。
func (s *Service) Delete(id string) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	if err := s.st.DeletePatrol(id); err != nil {
		return err
	}
	s.Reload()
	return nil
}

// Runs 列出流程执行记录（最新在前）。
func (s *Service) Runs(patrolID string, limit int) ([]*store.PatrolRun, error) {
	return s.st.ListPatrolRuns(patrolID, limit)
}

// Reports 列出流程报告（最新在前）。
func (s *Service) Reports(patrolID string, limit int) ([]*store.Report, error) {
	return s.st.ListReports(patrolID, limit)
}

// RunByID 读取单个执行记录。
func (s *Service) RunByID(id string) (*store.PatrolRun, error) {
	r, err := s.st.GetPatrolRun(id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound{ID: id}
	}
	return r, nil
}

// --- 执行 ---

// Run 立即执行一次巡检。
func (s *Service) Run(ctx context.Context, patrolID string) (*store.PatrolRun, error) {
	p, err := s.Get(patrolID)
	if err != nil {
		return nil, err
	}
	flow, err := ParseFlow(p.YAML)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	seq, err := s.st.NextSeq("patrolrun")
	if err != nil {
		return nil, err
	}
	run := &store.PatrolRun{
		ID:        fmt.Sprintf("run-%d", seq),
		PatrolID:  patrolID,
		Seq:       seq,
		StartedAt: now,
		Status:    store.RunRunning,
	}
	if err := s.st.SavePatrolRun(run); err != nil {
		return nil, err
	}

	// 检查阶段（失败不中断，记录错误）
	anomalies := s.runChecks(ctx, flow)
	run.Anomalies = anomalies

	// 报告阶段（AI 可用时生成，否则保底文本）
	report := s.buildReport(p, flow, anomalies)
	if report != "" {
		rep := &store.Report{
			ID:          run.ID,
			PatrolRunID: run.ID,
			PatrolID:    patrolID,
			Summary:     report,
			Model:       flow.Report.Model,
			CreatedAt:   time.Now().UTC(),
		}
		if err := s.st.SaveReport(rep); err != nil {
			run.Error = "save report: " + err.Error()
		} else {
			run.ReportID = rep.ID
		}
	}

	finished := time.Now().UTC()
	run.FinishedAt = &finished
	run.Status = store.RunSuccess
	if err := s.st.SavePatrolRun(run); err != nil {
		return nil, err
	}
	return run, nil
}

// runChecks 逐个执行检查项，收集异常。
func (s *Service) runChecks(ctx context.Context, flow *Flow) []store.Anomaly {
	var out []store.Anomaly
	for _, c := range flow.Checks {
		a := s.runCheck(ctx, c)
		out = append(out, a)
	}
	return out
}

func (s *Service) runCheck(ctx context.Context, c Check) store.Anomaly {
	base := store.Anomaly{
		Check:   c.Type + "/" + c.Service,
		Cluster: c.Cluster,
		Service: c.Service,
	}
	cli, err := s.clusters.WorkerClient(c.Cluster)
	if err != nil {
		base.OK = false
		base.Message = "集群不可用: " + err.Error()
		return base
	}
	switch c.Type {
	case "resource":
		return s.checkResource(ctx, cli, base, c)
	case "health":
		return s.checkHealth(ctx, cli, base, c)
	default:
		base.OK = false
		base.Message = "未知检查类型 " + c.Type
		return base
	}
}

// checkResource 用节点 stats 检查服务容器资源阈值。
func (s *Service) checkResource(ctx context.Context, cli *workerproxy.Client, base store.Anomaly, c Check) store.Anomaly {
	stats, err := cli.NodeStats(ctx)
	if err != nil {
		base.OK = false
		base.Message = "获取资源统计失败: " + err.Error()
		return base
	}
	found := false
	for _, ct := range stats.Containers {
		if ct.Service != c.Service {
			continue
		}
		found = true
		var issues []string
		if c.CPUThreshold > 0 && ct.CPUPercent > c.CPUThreshold {
			issues = append(issues, fmt.Sprintf("CPU %.1f%% > %.0f%%", ct.CPUPercent, c.CPUThreshold))
		}
		if c.MemThreshold > 0 && ct.MemPercent > c.MemThreshold {
			issues = append(issues, fmt.Sprintf("内存 %.1f%% > %.0f%%", ct.MemPercent, c.MemThreshold))
		}
		if len(issues) > 0 {
			base.OK = false
			base.Message = strings.Join(issues, "; ")
			base.Data = fmt.Sprintf("cpu=%.1f%% mem=%.1f%%", ct.CPUPercent, ct.MemPercent)
		} else {
			base.OK = true
			base.Message = fmt.Sprintf("资源正常 cpu=%.1f%% mem=%.1f%%", ct.CPUPercent, ct.MemPercent)
		}
	}
	if !found {
		base.OK = false
		base.Message = fmt.Sprintf("未找到服务 %q 的容器", c.Service)
	}
	return base
}

// checkHealth 检查服务健康（running/desired/healthy）。
func (s *Service) checkHealth(ctx context.Context, cli *workerproxy.Client, base store.Anomaly, c Check) store.Anomaly {
	d, err := cli.GetWorkload(ctx, c.Service)
	if err != nil {
		base.OK = false
		base.Message = "获取服务详情失败: " + err.Error()
		return base
	}
	minRep := c.MinReplicas
	if minRep <= 0 {
		minRep = 1
	}
	if d.Running < uint64(minRep) {
		base.OK = false
		base.Message = fmt.Sprintf("运行副本 %d 少于 %d", d.Running, minRep)
		base.Data = fmt.Sprintf("running=%d desired=%d healthy=%d", d.Running, d.Desired, d.Healthy)
		return base
	}
	if d.Desired > 0 && d.Running != d.Desired {
		base.OK = false
		base.Message = fmt.Sprintf("副本未收敛 %d/%d", d.Running, d.Desired)
		base.Data = fmt.Sprintf("healthy=%d", d.Healthy)
		return base
	}
	base.OK = true
	base.Message = fmt.Sprintf("服务健康 %d/%d", d.Running, d.Desired)
	return base
}

// buildReport 生成巡检报告：AI 摘要（可用时）或保底文本。
func (s *Service) buildReport(p *store.Patrol, flow *Flow, anomalies []store.Anomaly) string {
	okCount, failCount := 0, 0
	for _, a := range anomalies {
		if a.OK {
			okCount++
		} else {
			failCount++
		}
	}
	prompt := fmt.Sprintf("巡检流程：%s\n检查项：%d 项，正常 %d，异常 %d\n\n检查明细：\n",
		p.Name, len(anomalies), okCount, failCount)
	for _, a := range anomalies {
		status := "正常"
		if !a.OK {
			status = "异常"
		}
		prompt += fmt.Sprintf("- [%s] %s（%s/%s）：%s\n", status, a.Check, a.Cluster, a.Service, a.Message)
	}
	if flow.Report.Prompt != "" {
		prompt += "\n附加要求：" + flow.Report.Prompt + "\n"
	}

	if s.ainx == nil {
		return prompt // 无 AI 时返回结构化摘要
	}
	report, err := s.ainx.Summarize(flow.Report.Model, prompt)
	if err != nil {
		return prompt + "\n（AI 报告生成失败：" + err.Error() + "）"
	}
	return report
}

// newPatrolID 生成流程 id。
func newPatrolID() string {
	return fmt.Sprintf("pt-%d", time.Now().UnixNano())
}
