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

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexusrt"
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
	Type    string `yaml:"type"` // resource | health | port | http | process
	Cluster string `yaml:"cluster"`
	Service string `yaml:"service"` // resource/health 必填
	// resource 阈值
	CPUThreshold float64 `yaml:"cpu_threshold,omitempty"` // 百分比
	MemThreshold float64 `yaml:"mem_threshold,omitempty"`
	// health 期望副本（0=全部）
	MinReplicas int `yaml:"min_replicas,omitempty"`
	// port/http/process 目标节点（node ID/hostname，空 = 全部 ready 节点）
	Node string `yaml:"node,omitempty"`
	// port 检查（宿主机中间件端口，如 MySQL 3306）
	Host string `yaml:"host,omitempty"` // 目标主机/IP（探宿主机服务用节点 IP，勿用 127.0.0.1）
	Port int    `yaml:"port,omitempty"`
	// http 检查
	URL            string `yaml:"url,omitempty"`
	Method         string `yaml:"method,omitempty"`          // 默认 GET
	ExpectedStatus []int  `yaml:"expected_status,omitempty"` // 空 = 任意 2xx
	ExpectedBody   string `yaml:"expected_body,omitempty"`   // body 正则
	// process 检查（宿主机进程发现，如 java / redis-server）
	Filter   string `yaml:"filter,omitempty"`    // 名称/cmdline 子串（大小写不敏感）
	MinCount int    `yaml:"min_count,omitempty"` // 每节点最少匹配数（默认 1，少于即异常）
	// port/http 探测超时（如 3s，默认 3s）
	Timeout string `yaml:"timeout,omitempty"`
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
			if c.Service == "" {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].service is required for %s", i, c.Type)}
			}
		case "port":
			if c.Host == "" || c.Port <= 0 {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d]: port check requires host and port", i)}
			}
		case "http":
			if c.URL == "" {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d]: http check requires url", i)}
			}
		case "process":
			if c.Filter == "" {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d]: process check requires filter", i)}
			}
		default:
			return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].type must be resource|health|port|http|process, got %q", i, c.Type)}
		}
		if c.Cluster == "" {
			return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].cluster is required", i)}
		}
		if c.Timeout != "" {
			if _, err := time.ParseDuration(c.Timeout); err != nil {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].timeout invalid: %v", i, err)}
			}
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
	st       *store.Store
	clusters *cluster.Service
	ainxRT   *ainexusrt.Service // 内嵌 AiNexus 运行时（Server() 为空 = 无报告生成）
	sched    *cron.Cron
	// onRun 调度触发的执行（供测试注入/替换）。
	onRun func(patrolID string)
}

// New 创建巡检服务。sched 为 nil 时自动创建（单实例调度器）。
func New(st *store.Store, clusters *cluster.Service, ainxRT *ainexusrt.Service) *Service {
	s := &Service{st: st, clusters: clusters, ainxRT: ainxRT}
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

// runChecks 逐个执行检查项，收集异常（节点级检查一个检查项可产生多条）。
func (s *Service) runChecks(ctx context.Context, flow *Flow) []store.Anomaly {
	var out []store.Anomaly
	for _, c := range flow.Checks {
		out = append(out, s.runCheck(ctx, c)...)
	}
	return out
}

func (s *Service) runCheck(ctx context.Context, c Check) []store.Anomaly {
	base := store.Anomaly{
		Check:   c.Type + "/" + c.Service,
		Cluster: c.Cluster,
		Service: c.Service,
	}
	cli, err := s.clusters.WorkerClient(c.Cluster)
	if err != nil {
		base.OK = false
		base.Message = "集群不可用: " + err.Error()
		return []store.Anomaly{base}
	}
	switch c.Type {
	case "resource":
		return []store.Anomaly{s.checkResource(ctx, cli, base, c)}
	case "health":
		return []store.Anomaly{s.checkHealth(ctx, cli, base, c)}
	case "port":
		return s.checkPort(ctx, cli, c)
	case "http":
		return s.checkHTTP(ctx, cli, c)
	case "process":
		return s.checkProcess(ctx, cli, c)
	default:
		base.OK = false
		base.Message = "未知检查类型 " + c.Type
		return []store.Anomaly{base}
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

// --- 节点级检查（port/http/process）：node 空 = 全部 ready 节点，
// 每个失败节点一条异常；全部通过时聚合为一条正常。 ---

// checkPort 从各节点探测任意 host:port（宿主机中间件端口，如 MySQL 3306）。
func (s *Service) checkPort(ctx context.Context, cli *workerproxy.Client, c Check) []store.Anomaly {
	base := store.Anomaly{Check: fmt.Sprintf("port/%s:%d", c.Host, c.Port), Cluster: c.Cluster}
	targets, err := patrolTargetNodes(ctx, cli, c.Node)
	if err != nil {
		base.Message = err.Error()
		return []store.Anomaly{base}
	}
	var fails []store.Anomaly
	for _, n := range targets {
		res, err := cli.CheckPort(ctx, n.ID, c.Host, c.Port, c.Timeout)
		if err != nil {
			res.Error = err.Error()
		}
		if !res.OK {
			a := base
			a.Message = fmt.Sprintf("节点 %s 探测 %s:%d 不通: %s", n.Hostname, c.Host, c.Port, res.Error)
			a.Data = fmt.Sprintf("node=%s latency=%dms", n.ID, res.LatencyMS)
			fails = append(fails, a)
		}
	}
	if len(fails) > 0 {
		return fails
	}
	base.OK = true
	base.Message = fmt.Sprintf("%d 个节点探测 %s:%d 均连通", len(targets), c.Host, c.Port)
	return []store.Anomaly{base}
}

// checkHTTP 从各节点探测任意 URL（状态码 + body 正则）。
func (s *Service) checkHTTP(ctx context.Context, cli *workerproxy.Client, c Check) []store.Anomaly {
	base := store.Anomaly{Check: "http/" + c.URL, Cluster: c.Cluster}
	targets, err := patrolTargetNodes(ctx, cli, c.Node)
	if err != nil {
		base.Message = err.Error()
		return []store.Anomaly{base}
	}
	req := workerproxy.HTTPCheckRequest{
		URL:            c.URL,
		Method:         c.Method,
		ExpectedStatus: c.ExpectedStatus,
		ExpectedBody:   c.ExpectedBody,
		Timeout:        c.Timeout,
	}
	var fails []store.Anomaly
	for _, n := range targets {
		res, err := cli.CheckHTTP(ctx, n.ID, req)
		if err != nil {
			res.Error = err.Error()
		}
		if !res.OK {
			a := base
			a.Message = fmt.Sprintf("节点 %s 探测 %s 失败: %s", n.Hostname, c.URL, res.Error)
			a.Data = fmt.Sprintf("node=%s status=%d latency=%dms", n.ID, res.Status, res.LatencyMS)
			fails = append(fails, a)
		}
	}
	if len(fails) > 0 {
		return fails
	}
	base.OK = true
	base.Message = fmt.Sprintf("%d 个节点探测 %s 均正常", len(targets), c.URL)
	return []store.Anomaly{base}
}

// checkProcess 检查各节点宿主机进程（名称/cmdline 子串），匹配数少于 min_count 即异常。
func (s *Service) checkProcess(ctx context.Context, cli *workerproxy.Client, c Check) []store.Anomaly {
	minCount := c.MinCount
	if minCount <= 0 {
		minCount = 1
	}
	base := store.Anomaly{Check: "process/" + c.Filter, Cluster: c.Cluster}
	targets, err := patrolTargetNodes(ctx, cli, c.Node)
	if err != nil {
		base.Message = err.Error()
		return []store.Anomaly{base}
	}
	var fails []store.Anomaly
	for _, n := range targets {
		procs, err := cli.ListProcesses(ctx, n.ID, "", 0, c.Filter)
		if err != nil {
			a := base
			a.Message = fmt.Sprintf("节点 %s 获取进程列表失败: %s", n.Hostname, err.Error())
			fails = append(fails, a)
			continue
		}
		if len(procs.Processes) < minCount {
			a := base
			a.Message = fmt.Sprintf("节点 %s 匹配 %q 的进程 %d 个，少于期望 %d 个", n.Hostname, c.Filter, len(procs.Processes), minCount)
			fails = append(fails, a)
		}
	}
	if len(fails) > 0 {
		return fails
	}
	base.OK = true
	base.Message = fmt.Sprintf("%d 个节点均存在匹配 %q 的进程（≥%d 个）", len(targets), c.Filter, minCount)
	return []store.Anomaly{base}
}

// patrolTargetNodes 解析检查目标节点：node 空 = 全部 ready 节点，否则按 ID/hostname 匹配单个。
func patrolTargetNodes(ctx context.Context, cli *workerproxy.Client, node string) ([]workerproxy.Node, error) {
	nodes, err := cli.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取节点列表失败: %w", err)
	}
	if node == "" {
		var out []workerproxy.Node
		for _, n := range nodes {
			if n.State == "ready" {
				out = append(out, n)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("集群无 ready 节点")
		}
		return out, nil
	}
	for _, n := range nodes {
		if n.ID == node || n.Hostname == node {
			return []workerproxy.Node{n}, nil
		}
	}
	return nil, fmt.Errorf("目标节点 %q 不存在", node)
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

	if s.ainxRT == nil {
		return prompt // 无 AI 时返回结构化摘要
	}
	srv := s.ainxRT.Server()
	if srv == nil {
		return prompt
	}
	report, err := srv.Summarize(flow.Report.Model, prompt)
	if err != nil {
		return prompt + "\n（AI 报告生成失败：" + err.Error() + "）"
	}
	return report
}

// newPatrolID 生成流程 id。
func newPatrolID() string {
	return fmt.Sprintf("pt-%d", time.Now().UnixNano())
}
