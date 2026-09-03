// Package patrol implements YAML-declared inspection flows (P5): flow CRUD,
// single-instance cron scheduling, and execution that runs checks against
// cluster Workers, collects anomalies, and generates an AI report through the
// embedded AiNexus gateway.
package patrol

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // 内嵌 IANA 时区库：cron 调度时区不依赖容器 /usr/share/zoneinfo

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexusrt"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
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
	Type    string `yaml:"type"` // resource | health | port | http | process | flow
	Cluster string `yaml:"cluster"`
	Service string `yaml:"service"` // resource/health 必填
	// resource 阈值
	CPUThreshold float64 `yaml:"cpu_threshold,omitempty"` // 百分比
	MemThreshold float64 `yaml:"mem_threshold,omitempty"`
	// health 期望副本（0=全部）
	MinReplicas int `yaml:"min_replicas,omitempty"`
	// port/http/process/flow 目标节点（node ID/hostname，空 = 全部 ready 节点）
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
	// port/http/flow 探测超时（如 3s，默认 3s）
	Timeout string `yaml:"timeout,omitempty"`
	// flow 检查（多步 HTTP 事务，如 登录→验证会话）
	Name  string            `yaml:"name,omitempty"`  // 事务名（告警标识用）
	Vars  map[string]string `yaml:"vars,omitempty"`  // 初始变量；值支持 ${secret:name} 引用管理端密钥
	Steps []FlowStepDef     `yaml:"steps,omitempty"` // 有序步骤，失败即终止
}

// FlowStepDef 多步事务探测的一个步骤（YAML 定义）。
type FlowStepDef struct {
	Name         string            `yaml:"name"`
	URL          string            `yaml:"url"`                     // 可引用 {{var}}
	Method       string            `yaml:"method,omitempty"`        // 默认 GET
	Headers      map[string]string `yaml:"headers,omitempty"`       // 值可引用 {{var}}
	Body         string            `yaml:"body,omitempty"`          // 可引用 {{var}}
	ExpectStatus []int             `yaml:"expect_status,omitempty"` // 空 = 任意 2xx
	ExpectBody   string            `yaml:"expect_body,omitempty"`   // body 正则
	Extract      map[string]string `yaml:"extract,omitempty"`       // var -> "$.json.path" 或 "re:正则"
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
		case "flow":
			if c.Name == "" {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d]: flow check requires name", i)}
			}
			if len(c.Steps) == 0 {
				return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d]: flow check requires steps", i)}
			}
			for j, st := range c.Steps {
				if st.Name == "" || st.URL == "" {
					return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].steps[%d]: name 和 url 必填", i, j)}
				}
			}
		default:
			return nil, ErrInvalidFlow{fmt.Sprintf("checks[%d].type must be resource|health|port|http|process|flow, got %q", i, c.Type)}
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
	notify   *notify.Service    // 报告投递 + 异常转告警通知（nil = 不投递）
	loc      *time.Location     // cron 调度时区（nil = time.Local）
	sched    *cron.Cron
	// onRun 调度触发的执行（供测试注入/替换）。
	onRun func(patrolID string)
}

// New 创建巡检服务。sched 为 nil 时自动创建（单实例调度器）。
// loc 为 cron 表达式解释时区：传 nil 回退 time.Local（容器时区）；
// 生产请显式传业务时区（如 Asia/Shanghai），避免容器 TZ 影响调度时刻。
func New(st *store.Store, clusters *cluster.Service, ainxRT *ainexusrt.Service, notifySvc *notify.Service, loc *time.Location) *Service {
	s := &Service{st: st, clusters: clusters, ainxRT: ainxRT, notify: notifySvc, loc: loc}
	// 5 字段标准 cron（与 ValidCron 的 ParseStandard 一致）
	s.sched = newScheduler(loc)
	s.onRun = func(id string) { _, _ = s.Run(context.Background(), id) }
	return s
}

// newScheduler 创建按指定时区解释 cron 的调度器（nil = 进程本地时区）。
func newScheduler(loc *time.Location) *cron.Cron {
	if loc == nil {
		loc = time.Local
	}
	return cron.New(cron.WithLocation(loc))
}

// Start 启动调度器并加载已启用的流程。
func (s *Service) Start() { s.Reload(); s.sched.Start() }

// Stop 停止调度器。
func (s *Service) Stop() { s.sched.Stop() }

// Reload 重载调度（CRUD 后调用）：清空并重新注册所有 enabled 流程。
func (s *Service) Reload() {
	ctx := s.sched.Stop() // 停止并等待已开始的作业结束（单实例）
	_ = ctx
	s.sched = newScheduler(s.loc)
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

	// 巡检异常转告警（新增/复发通知，恢复自动关闭；失败不阻断巡检）
	s.syncAlerts(ctx, p, run)

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
			// 报告投递到通知渠道（按全局设置；失败仅记入 run.Error）
			if derr := s.deliverReport(ctx, p, report, anomalies); derr != "" {
				run.Error = derr
			}
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
	case "flow":
		return s.checkFlow(ctx, cli, c)
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
			a.Node = n.Hostname
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
			a.Node = n.Hostname
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
			a.Node = n.Hostname
			a.Message = fmt.Sprintf("节点 %s 获取进程列表失败: %s", n.Hostname, err.Error())
			fails = append(fails, a)
			continue
		}
		if len(procs.Processes) < minCount {
			a := base
			a.Node = n.Hostname
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

// checkFlow 多步 HTTP 事务探测（典型：登录拿 token → 带 token 验证业务接口）。
// vars 中的 ${secret:name} 在执行时替换为管理端密钥值（凭据不落 YAML）。
func (s *Service) checkFlow(ctx context.Context, cli *workerproxy.Client, c Check) []store.Anomaly {
	base := store.Anomaly{Check: "flow/" + c.Name, Cluster: c.Cluster}
	vars, err := s.resolveSecretVars(c.Vars)
	if err != nil {
		base.Message = "解析密钥引用失败: " + err.Error()
		return []store.Anomaly{base}
	}
	targets, err := patrolTargetNodes(ctx, cli, c.Node)
	if err != nil {
		base.Message = err.Error()
		return []store.Anomaly{base}
	}
	steps := make([]workerproxy.FlowStep, 0, len(c.Steps))
	for _, sd := range c.Steps {
		steps = append(steps, workerproxy.FlowStep{
			Name: sd.Name, URL: sd.URL, Method: sd.Method, Headers: sd.Headers,
			Body: sd.Body, ExpectStatus: sd.ExpectStatus, ExpectBody: sd.ExpectBody, Extract: sd.Extract,
		})
	}
	req := workerproxy.FlowCheckRequest{Steps: steps, Vars: vars, Timeout: c.Timeout}
	var fails []store.Anomaly
	for _, n := range targets {
		res, err := cli.CheckFlow(ctx, n.ID, req)
		if err != nil {
			res.Error = err.Error()
		}
		if !res.OK {
			a := base
			a.Node = n.Hostname
			detail := res.Error
			for _, sr := range res.Steps {
				if !sr.OK {
					detail = fmt.Sprintf("步骤 %q 失败: %s", sr.Name, sr.Error)
					break
				}
			}
			a.Message = fmt.Sprintf("节点 %s 事务探测「%s」失败: %s", n.Hostname, c.Name, detail)
			a.Data = fmt.Sprintf("node=%s failed_step=%s", n.ID, res.FailedStep)
			fails = append(fails, a)
		}
	}
	if len(fails) > 0 {
		return fails
	}
	base.OK = true
	base.Message = fmt.Sprintf("%d 个节点事务探测「%s」均通过", len(targets), c.Name)
	return []store.Anomaly{base}
}

// secretRefRe 匹配 ${secret:name} 密钥引用。
var secretRefRe = regexp.MustCompile(`\$\{secret:([a-zA-Z0-9_.-]+)\}`)

// resolveSecretVars 把 vars 里的 ${secret:name} 替换为密钥值；引用不存在 → 报错
// （宁可探测失败，也不带空凭据发起请求）。
func (s *Service) resolveSecretVars(vars map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		for _, m := range secretRefRe.FindAllStringSubmatch(v, -1) {
			sec, err := s.st.GetSecret(m[1])
			if err != nil {
				return nil, err
			}
			if sec == nil {
				return nil, fmt.Errorf("secret %q 不存在（系统设置 → 密钥 维护）", m[1])
			}
			v = strings.ReplaceAll(v, m[0], sec.Value)
		}
		out[k] = v
	}
	return out, nil
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

// --- 闭环：报告投递 + 异常转告警 ---

// SettingReportDelivery 巡检报告投递配置的 settings key（系统设置「巡检报告」tab 维护）。
const SettingReportDelivery = "patrol-report"

// ReportDelivery 巡检报告投递配置。
type ReportDelivery struct {
	Mode       string   `json:"mode"`        // always | anomaly | off
	ChannelIDs []string `json:"channel_ids"` // notify 渠道
}

// ReportDeliveryConfig 读取投递配置（未设置返回 off 默认）。
func (s *Service) ReportDeliveryConfig() (ReportDelivery, error) {
	var cfg ReportDelivery
	found, err := s.st.GetSetting(SettingReportDelivery, &cfg)
	if err != nil || !found {
		return ReportDelivery{Mode: "off"}, err
	}
	return cfg, nil
}

// SaveReportDeliveryConfig 校验并保存投递配置（渠道必须存在）。
func (s *Service) SaveReportDeliveryConfig(cfg ReportDelivery) error {
	switch cfg.Mode {
	case "always", "anomaly", "off":
	default:
		return ErrInvalidFlow{"report delivery mode must be always|anomaly|off"}
	}
	if len(cfg.ChannelIDs) > 0 && s.notify != nil {
		channels, err := s.notify.ListChannels()
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		for _, ch := range channels {
			existing[ch.ID] = true
		}
		for _, id := range cfg.ChannelIDs {
			if !existing[id] {
				return ErrInvalidFlow{"notify channel not found: " + id}
			}
		}
	}
	return s.st.PutSetting(SettingReportDelivery, cfg)
}

// deliverReport 按全局设置把巡检报告投递到通知渠道。
// 返回错误描述（空 = 未投递或投递成功）；投递失败不阻断巡检，由调用方记入 run.Error。
func (s *Service) deliverReport(ctx context.Context, p *store.Patrol, report string, anomalies []store.Anomaly) string {
	if s.notify == nil {
		return ""
	}
	var cfg ReportDelivery
	found, err := s.st.GetSetting(SettingReportDelivery, &cfg)
	if err != nil {
		return "read report delivery setting: " + err.Error()
	}
	if !found || cfg.Mode == "" || cfg.Mode == "off" || len(cfg.ChannelIDs) == 0 {
		return ""
	}
	okCount, failCount := 0, 0
	for _, a := range anomalies {
		if a.OK {
			okCount++
		} else {
			failCount++
		}
	}
	if cfg.Mode == "anomaly" && failCount == 0 {
		return ""
	}
	title := fmt.Sprintf("巡检报告「%s」：正常 %d / 异常 %d", p.Name, okCount, failCount)
	// 富文本投递：飞书渠道渲染为 post 富文本（明细逐行排版），其余渠道纯文本
	if err := s.notify.SendRich(ctx, cfg.ChannelIDs, title, report); err != nil {
		return "deliver report: " + err.Error()
	}
	return ""
}

// syncAlerts 巡检异常转告警：每个失败检查项（按 集群/服务/检查项/节点 细分）
// upsert 一条确定键告警；新增或恢复后复发才通知（避免每次 cron 重复推送）。
// 与上一已完成 run 对比，本轮不再失败的检查项 → 对应告警自动 recovered。
func (s *Service) syncAlerts(ctx context.Context, p *store.Patrol, cur *store.PatrolRun) {
	curFail := map[string]store.Anomaly{}
	for _, a := range cur.Anomalies {
		if !a.OK {
			curFail[patrolAlertKey(a)] = a
		}
	}
	// 上一已完成 run 的失败集合（当前 run 仍是 running，跳过）
	prevFail := map[string]store.Anomaly{}
	if runs, err := s.st.ListPatrolRuns(p.ID, 5); err == nil {
		for _, r := range runs {
			if r.ID == cur.ID || r.Status == store.RunRunning {
				continue
			}
			for _, a := range r.Anomalies {
				if !a.OK {
					prevFail[patrolAlertKey(a)] = a
				}
			}
			break // 只对比最近一次
		}
	}

	for _, a := range curFail {
		now := time.Now().UTC()
		alert := &store.Alert{
			ID:      store.AlertIDWithKey(a.Cluster, a.Service, store.EventPatrolFailed, a.Check+"|"+a.Node),
			Cluster: a.Cluster,
			Service: a.Service,
			Type:    store.EventPatrolFailed,
			Level:   store.LevelWarn,
			Title:   fmt.Sprintf("巡检异常：%s", a.Check),
			Status:  store.AlertActive,
			Count:   1,
			FirstTS: now,
			LastTS:  now,
		}
		created, err := s.st.UpsertKeyedAlert(alert)
		if err != nil {
			continue
		}
		if created && s.notify != nil {
			subject := fmt.Sprintf("[%s] 巡检「%s」发现异常：%s — %s", a.Cluster, p.Name, a.Check, a.Message)
			// 通知按告警级别策略路由；upsert 可能合并了旧计数，取库里最新告警
			if latest, err := s.st.GetAlert(alert.ID); err == nil && latest != nil {
				alert = latest
			}
			_ = s.notify.NotifyAlert(ctx, alert, subject)
		}
	}
	for key, a := range prevFail {
		if _, still := curFail[key]; still {
			continue
		}
		id := store.AlertIDWithKey(a.Cluster, a.Service, store.EventPatrolFailed, a.Check+"|"+a.Node)
		recovered, err := s.st.SetAlertStatus(id, store.AlertRecovered, "patrol")
		if err == nil && recovered != nil && s.notify != nil {
			subject := fmt.Sprintf("[%s] 巡检「%s」异常已恢复：%s", a.Cluster, p.Name, a.Check)
			_ = s.notify.NotifyAlert(ctx, recovered, subject)
		}
	}
}

// patrolAlertKey 检查项的告警身份：集群/服务/检查项/节点（节点级检查同
// 一检查项在不同节点的失败是不同告警）。
func patrolAlertKey(a store.Anomaly) string {
	return a.Cluster + "|" + a.Service + "|" + a.Check + "|" + a.Node
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
	// 经运行时服务调用：flow 未指定模型时应用 patrol_report 场景绑定
	//（mlops 运营层），绑定不可路由时回退网关默认可用。
	report, err := s.ainxRT.Summarize(flow.Report.Model, prompt)
	if err != nil {
		return prompt + "\n（AI 报告生成失败：" + err.Error() + "）"
	}
	return report
}

// newPatrolID 生成流程 id。
func newPatrolID() string {
	return fmt.Sprintf("pt-%d", time.Now().UnixNano())
}
