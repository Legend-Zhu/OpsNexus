package patrol

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
)

// TestParseFlow 合法/非法 YAML。
func TestParseFlow(t *testing.T) {
	valid := `
name: nightly
checks:
  - type: resource
    cluster: dev
    service: web
    cpu_threshold: 85
  - type: health
    cluster: dev
    service: api
    min_replicas: 2
`
	f, err := ParseFlow(valid)
	if err != nil {
		t.Fatalf("valid flow rejected: %v", err)
	}
	if f.Name != "nightly" || len(f.Checks) != 2 || f.Checks[1].MinReplicas != 2 {
		t.Fatalf("unexpected flow: %+v", f)
	}

	for _, bad := range []string{"", "name: x", "name: x\nchecks: []",
		"name: x\nchecks:\n  - type: bogus\n    cluster: c\n    service: s"} {
		if _, err := ParseFlow(bad); err == nil {
			t.Fatalf("invalid flow accepted: %q", bad)
		}
	}
}

// TestValidCron 校验 cron 表达式。
func TestValidCron(t *testing.T) {
	if err := ValidCron("0 2 * * *"); err != nil {
		t.Fatalf("valid cron rejected: %v", err)
	}
	if err := ValidCron("bad cron"); err == nil {
		t.Fatal("invalid cron accepted")
	}
}

// patrolWorkerServer 是 patrol 测试用的 mock Worker，实现所有节点级/服务级检查
// RPC。语义与原 HTTP mock 一致：n1 一切正常，n2 全失败（端口不通/HTTP 503/
// 无 java 进程/flow 卡 login）；flow 还校验 secret 替换后的密码确实到达。
// 其余 RPC 走 UnimplementedManagementServiceServer。
type patrolWorkerServer struct {
	pb.UnimplementedManagementServiceServer
}

func (m *patrolWorkerServer) Self(context.Context, *pb.Empty) (*pb.SelfInfo, error) {
	return &pb.SelfInfo{
		NodeId: "n1", Hostname: "h1", Role: "manager",
		Leader: true, State: "ready", SwarmManager: true,
	}, nil
}

func (m *patrolWorkerServer) NodeStats(context.Context, *pb.Empty) (*pb.NodeStatsResponse, error) {
	return &pb.NodeStatsResponse{
		Node: "h1",
		Containers: []*pb.ContainerStat{{
			ContainerId: "c1", Service: "web", CpuPercent: 95.0, MemPercent: 88.0,
			MemUsage: 100, MemLimit: 200,
		}},
	}, nil
}

func (m *patrolWorkerServer) GetService(_ context.Context, req *pb.GetServiceRequest) (*pb.ServiceDetail, error) {
	// 旧 HTTP mock 返回 {service:{ID,Spec:{Name},ServiceStatus:{2,2}},...}
	// workerproxy 从 Docker 原生 ServiceStatus 取 running/desired；这里同样
	// 用 service_json(单个 dockerService) + running/desired/healthy 字段。
	svcJSON, _ := json.Marshal(map[string]any{
		"ID":   "s1",
		"Spec": map[string]any{"Name": req.GetName()},
		"ServiceStatus": map[string]any{"RunningTasks": 2, "DesiredTasks": 2},
	})
	return &pb.ServiceDetail{
		ServiceJson: svcJSON,
		TasksJson:   []byte("[]"),
		Running:     2,
		Desired:     2,
		Healthy:     2,
	}, nil
}

// 节点级检查 mock：n1 一切正常，n2 全失败。
func (m *patrolWorkerServer) ListNodes(context.Context, *pb.Empty) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: []*pb.Node{
		{Id: "n1", Hostname: "h1", State: "ready", Role: "manager"},
		{Id: "n2", Hostname: "h2", State: "ready", Role: "worker"},
	}}, nil
}

func (m *patrolWorkerServer) CheckPort(_ context.Context, req *pb.CheckPortRequest) (*pb.PortCheckResult, error) {
	ok := req.GetNodeId() == "n1"
	out := &pb.PortCheckResult{
		Node: req.GetNodeId(), Host: req.GetHost(), Port: portToStr(req.GetPort()),
		Ok: ok, LatencyMs: 1,
	}
	if !ok {
		out.Error = "dial tcp: connection refused"
	}
	return out, nil
}

func (m *patrolWorkerServer) CheckHTTP(_ context.Context, req *pb.CheckHTTPRequest) (*pb.HTTPCheckResult, error) {
	ok := req.GetNodeId() == "n1"
	out := &pb.HTTPCheckResult{
		Node: req.GetNodeId(), Url: "http://x/healthz", Ok: ok,
		Status: 200, LatencyMs: 1,
	}
	if !ok {
		out.Status = 503
		out.Error = "status 503 not 2xx"
	}
	return out, nil
}

func (m *patrolWorkerServer) NodeProcesses(_ context.Context, req *pb.NodeProcessesRequest) (*pb.ProcessesResponse, error) {
	var procs []*pb.ProcessInfo
	// 原 mock: 仅 n1 + filter==java 返回 2 个 java 进程
	if req.GetNodeId() == "n1" && req.GetFilter() == "java" {
		procs = append(procs,
			&pb.ProcessInfo{Pid: 100, Name: "java", Cmdline: "java -jar app.jar"},
			&pb.ProcessInfo{Pid: 101, Name: "java", Cmdline: "java -jar worker.jar"},
		)
	}
	return &pb.ProcessesResponse{
		Node: req.GetNodeId(), Total: int32(len(procs)), Processes: procs,
	}, nil
}

// CheckFlow mock:校验 secret 替换后的密码确实到达;n2 卡在 login 步。
func (m *patrolWorkerServer) CheckFlow(_ context.Context, req *pb.CheckFlowRequest) (*pb.FlowCheckResult, error) {
	out := &pb.FlowCheckResult{Node: req.GetNodeId(), LatencyMs: 3}
	switch {
	case req.GetNodeId() != "n1":
		out.Ok = false
		out.FailedStep = "login"
		out.Steps = []*pb.FlowStepResult{{Name: "login", Ok: false, Error: "status 401 not 2xx"}}
	case req.GetVars()["pass"] != "s3cret-from-store":
		// secret 未被替换/替换错 → 视为探测失败
		out.Ok = false
		out.FailedStep = "login"
		out.Steps = []*pb.FlowStepResult{{Name: "login", Ok: false, Error: "bad credentials"}}
	default:
		out.Ok = true
		out.Steps = []*pb.FlowStepResult{
			{Name: "login", Ok: true, Status: 200, Extracted: []string{"token"}},
			{Name: "verify", Ok: true, Status: 200},
		}
	}
	return out, nil
}

// portToStr 把端口号转成字符串（旧 HTTP mock 用 query string，原样是字符串）。
func portToStr(p int32) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(int(p))
}

// startGRPCWorker 在随机 TCP 端口上启动一个真实 gRPC 服务器，返回其
// http://127.0.0.1:<port> URL（workerproxy.New 经 grpcTarget 拨号会剥掉 scheme）。
// cleanup 注册到 t.Cleanup。
func startGRPCWorker(t *testing.T, srv pb.ManagementServiceServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterManagementServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.GracefulStop() })
	return "http://" + lis.Addr().String()
}

func mockWorker(t *testing.T) string {
	t.Helper()
	return startGRPCWorker(t, &patrolWorkerServer{})
}

func newTestPatrol(t *testing.T) (*Service, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	url := mockWorker(t)
	cs := cluster.New(st)
	// 直接落库一个在线集群
	if err := st.PutCluster(&store.Cluster{Name: "dev", WorkerURL: url, Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	return New(st, cs, nil, nil), url
}

// TestCreateAndRun 创建流程 → 立即执行 → 异常采集 + 保底报告。
func TestCreateAndRun(t *testing.T) {
	svc, _ := newTestPatrol(t)

	p, err := svc.Create("nightly", "desc", "0 2 * * *", `
name: nightly
checks:
  - type: resource
    cluster: dev
    service: web
    cpu_threshold: 80
  - type: health
    cluster: dev
    service: web
    min_replicas: 1
`, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" || !p.Enabled {
		t.Fatalf("unexpected patrol: %+v", p)
	}

	run, err := svc.Run(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Status != store.RunSuccess || run.Anomalies == nil || len(run.Anomalies) != 2 {
		t.Fatalf("unexpected run: %+v", run)
	}
	// resource 检查：CPU 95% > 80% → 异常
	if run.Anomalies[0].OK {
		t.Fatalf("resource check should fail: %+v", run.Anomalies[0])
	}
	// health 检查：2/2 正常
	if !run.Anomalies[1].OK {
		t.Fatalf("health check should pass: %+v", run.Anomalies[1])
	}
	// 保底报告（无 AI）
	rep, err := svc.st.GetReport(run.ID)
	if err != nil || rep == nil {
		t.Fatalf("report missing: %v", err)
	}
	if rep.Summary == "" || !contains(rep.Summary, "nightly") {
		t.Fatalf("unexpected report: %q", rep.Summary)
	}
}

// TestRunValidation 非法 YAML 创建被拒。
func TestRunValidation(t *testing.T) {
	svc, _ := newTestPatrol(t)
	if _, err := svc.Create("bad", "", "0 2 * * *", "name: x", false); err == nil {
		t.Fatal("invalid flow should be rejected")
	}
	if _, err := svc.Create("bad2", "", "not a cron", "name: x\nchecks:\n  - type: resource\n    cluster: c\n    service: s", false); err == nil {
		t.Fatal("invalid cron should be rejected")
	}
}

// TestNodeLevelChecks port/http/process 检查：node 空 = 全部 ready 节点，
// 每个失败节点一条异常（n2 全失败，n1 全通过）。
func TestNodeLevelChecks(t *testing.T) {
	svc, _ := newTestPatrol(t)

	p, err := svc.Create("node-checks", "", "0 2 * * *", `
name: node-checks
checks:
  - type: port
    cluster: dev
    host: 10.0.0.1
    port: 3306
  - type: http
    cluster: dev
    url: http://10.0.0.1:8080/healthz
  - type: process
    cluster: dev
    filter: java
  - type: port
    cluster: dev
    node: h1
    host: 10.0.0.1
    port: 6379
`, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	run, err := svc.Run(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// port(全部节点)/http/process 各产出 n2 一条失败；port(node=h1) 通过
	if len(run.Anomalies) != 4 {
		t.Fatalf("anomalies len=%d, want 4: %+v", len(run.Anomalies), run.Anomalies)
	}
	for i, a := range run.Anomalies[:3] {
		if a.OK || !contains(a.Message, "h2") {
			t.Fatalf("anomalies[%d] 应为 h2 的失败: %+v", i, a)
		}
	}
	last := run.Anomalies[3]
	if !last.OK || !contains(last.Message, "6379") {
		t.Fatalf("单节点 port 检查应通过: %+v", last)
	}
}

// TestParseFlowNodeChecks 新检查类型的字段校验。
func TestParseFlowNodeChecks(t *testing.T) {
	valid := `
name: x
checks:
  - type: port
    cluster: dev
    host: 10.0.0.1
    port: 3306
    timeout: 5s
  - type: http
    cluster: dev
    url: http://a/healthz
    expected_status: [200, 204]
  - type: process
    cluster: dev
    filter: java
    min_count: 2
`
	f, err := ParseFlow(valid)
	if err != nil {
		t.Fatalf("valid flow rejected: %v", err)
	}
	if f.Checks[2].MinCount != 2 || f.Checks[1].ExpectedStatus[1] != 204 {
		t.Fatalf("unexpected flow: %+v", f.Checks)
	}

	for _, bad := range []string{
		"name: x\nchecks:\n  - type: port\n    cluster: c",                                             // 缺 host/port
		"name: x\nchecks:\n  - type: http\n    cluster: c",                                             // 缺 url
		"name: x\nchecks:\n  - type: process\n    cluster: c",                                          // 缺 filter
		"name: x\nchecks:\n  - type: resource\n    cluster: c",                                         // resource 缺 service
		"name: x\nchecks:\n  - type: port\n    cluster: c\n    host: h\n    port: 1\n    timeout: bad", // timeout 非法
	} {
		if _, err := ParseFlow(bad); err == nil {
			t.Fatalf("invalid flow accepted: %q", bad)
		}
	}
}

// TestClosedLoop 巡检闭环：异常转告警（新建→持续→恢复）+ 报告按设置投递。
func TestClosedLoop(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// 渠道接收端（webhook）：同时收告警通知（带 alert 字段）与报告（带 content 字段）。
	// 这是 notify 包的 webhook 接收端（普通 HTTP POST），不是 Worker，保留 httptest。
	var captured []map[string]any
	capSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		captured = append(captured, m)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(capSrv.Close)

	// 可切换成败的单节点 Worker mock（gRPC）：failPort 控制端口探测成败。
	failPort := true
	wSrv := &closedLoopWorkerServer{failFunc: func() bool { return failPort }}
	workerURL := startGRPCWorker(t, wSrv)

	cs := cluster.New(st)
	if err := st.PutCluster(&store.Cluster{Name: "dev", WorkerURL: workerURL, Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	notifySvc := notify.New(st)
	ch, err := notifySvc.CreateChannel(store.ChannelWebhook, "hook", map[string]any{"url": capSrv.URL}, false, "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	// warn 级策略（巡检异常告警的通知路由）+ 报告投递设置（仅有异常时）
	if err := notifySvc.UpsertPolicy(&store.NotifyPolicy{Level: "warn", ChannelIDs: []string{ch.ID}}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if err := st.PutSetting(SettingReportDelivery, ReportDelivery{Mode: "anomaly", ChannelIDs: []string{ch.ID}}); err != nil {
		t.Fatalf("setting: %v", err)
	}

	svc := New(st, cs, nil, notifySvc)
	p, err := svc.Create("pl", "", "0 2 * * *", `
name: pl
checks:
  - type: port
    cluster: dev
    host: 10.0.0.1
    port: 3306
`, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Run 1（端口不通）：产生 patrol_failed 告警 + 告警通知 + 报告投递
	if _, err := svc.Run(context.Background(), p.ID); err != nil {
		t.Fatalf("run1: %v", err)
	}
	alertID := store.AlertIDWithKey("dev", "", store.EventPatrolFailed, "port/10.0.0.1:3306|h1")
	a, err := st.GetAlert(alertID)
	if err != nil || a == nil || a.Status != store.AlertActive {
		t.Fatalf("alert after run1: %+v err=%v", a, err)
	}
	var gotAlertNotify, gotReport bool
	for _, m := range captured {
		if _, ok := m["alert"]; ok {
			gotAlertNotify = true
		}
		if _, ok := m["content"]; ok {
			gotReport = true
		}
	}
	if !gotAlertNotify || !gotReport {
		t.Fatalf("run1 captured=%v, want alert notify + report", captured)
	}

	// Run 2（持续不通）：告警计数累加，不重复通知
	captured = nil
	if _, err := svc.Run(context.Background(), p.ID); err != nil {
		t.Fatalf("run2: %v", err)
	}
	a, _ = st.GetAlert(alertID)
	if a.Count != 2 || a.Status != store.AlertActive {
		t.Fatalf("alert after run2: %+v", a)
	}
	for _, m := range captured {
		if _, ok := m["alert"]; ok {
			t.Fatal("run2 should not re-notify an already-active alert")
		}
	}

	// Run 3（恢复）：告警自动 recovered；无异常不投递报告
	failPort = false
	captured = nil
	if _, err := svc.Run(context.Background(), p.ID); err != nil {
		t.Fatalf("run3: %v", err)
	}
	a, _ = st.GetAlert(alertID)
	if a.Status != store.AlertRecovered {
		t.Fatalf("alert after recovery: %+v", a)
	}
	for _, m := range captured {
		if _, ok := m["content"]; ok {
			t.Fatal("run3 (all ok) should not deliver report in anomaly mode")
		}
	}
}

// TestFlowCheckWithSecret flow 检查:多步事务 + ${secret:} 引用替换(mock 校验
// 替换后的密码确实到达 Worker);n1 通过,n2 卡 login 步产出异常。
func TestFlowCheckWithSecret(t *testing.T) {
	svc, _ := newTestPatrol(t)
	if err := svc.st.PutSecret(&store.Secret{Name: "patrol-login", Value: "s3cret-from-store"}); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	p, err := svc.Create("flow-check", "", "0 2 * * *", `
name: flow-check
checks:
  - type: flow
    cluster: dev
    name: 登录可用性
    vars:
      user: monitor-bot
      pass: "${secret:patrol-login}"
    steps:
      - name: login
        method: POST
        url: http://10.0.0.1/api/login
        body: '{"username":"{{user}}","password":"{{pass}}"}'
        extract: { token: "$.data.token" }
      - name: verify
        url: http://10.0.0.1/api/me
        headers: { Authorization: "Bearer {{token}}" }
`, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	run, err := svc.Run(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// n2 失败一条,消息含失败步骤;n1 通过(secret 已正确替换并到达)
	if len(run.Anomalies) != 1 {
		t.Fatalf("anomalies=%+v, want 1", run.Anomalies)
	}
	a := run.Anomalies[0]
	if a.OK || !contains(a.Message, "h2") || !contains(a.Message, "login") {
		t.Fatalf("unexpected anomaly: %+v", a)
	}
	if a.Node != "h2" {
		t.Fatalf("anomaly.Node=%q", a.Node)
	}

	// secret 不存在 → 检查失败并明确提示
	p2, _ := svc.Create("flow-bad-secret", "", "0 2 * * *", `
name: x
checks:
  - type: flow
    cluster: dev
    name: t
    vars: { pass: "${secret:no-such}" }
    steps:
      - { name: s1, url: "http://x/" }
`, true)
	run2, err := svc.Run(context.Background(), p2.ID)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if run2.Anomalies[0].OK || !contains(run2.Anomalies[0].Message, "no-such") {
		t.Fatalf("missing secret should fail clearly: %+v", run2.Anomalies[0])
	}
}

// TestDeleteAndRuns 删除流程 + 执行记录列表。
func TestDeleteAndRuns(t *testing.T) {
	svc, _ := newTestPatrol(t)
	p, err := svc.Create("x", "", "0 2 * * *", "name: x\nchecks:\n  - type: health\n    cluster: dev\n    service: web", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Run(context.Background(), p.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	runs, err := svc.Runs(p.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v len=%d", err, len(runs))
	}
	if err := svc.Delete(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(p.ID); err == nil {
		t.Fatal("should be not found after delete")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// closedLoopWorkerServer 是 TestClosedLoop 专用的可切换成败的单节点 Worker mock。
// failFunc 返回 true 时端口探测失败（连接拒绝），false 时通过。
type closedLoopWorkerServer struct {
	pb.UnimplementedManagementServiceServer
	failFunc func() bool
}

func (m *closedLoopWorkerServer) ListNodes(context.Context, *pb.Empty) (*pb.ListNodesResponse, error) {
	return &pb.ListNodesResponse{Nodes: []*pb.Node{
		{Id: "n1", Hostname: "h1", State: "ready", Role: "manager"},
	}}, nil
}

func (m *closedLoopWorkerServer) CheckPort(_ context.Context, req *pb.CheckPortRequest) (*pb.PortCheckResult, error) {
	fail := m.failFunc()
	out := &pb.PortCheckResult{
		Node: "h1", Host: req.GetHost(), Port: portToStr(req.GetPort()),
		Ok: !fail, LatencyMs: 1,
	}
	if fail {
		out.Error = "connection refused"
	}
	return out, nil
}
