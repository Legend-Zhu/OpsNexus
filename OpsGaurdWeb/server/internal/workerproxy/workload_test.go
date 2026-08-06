package workerproxy

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockOpsServer 一个带内存操作状态的 mock Worker，实现 ManagementServiceServer。
// 每个 RPC 对应旧 HTTP mock 的一个 handler；只实现测试需要的方法，其余走
// UnimplementedManagementServiceServer（返回 Unimplemented）。
type mockOpsServer struct {
	pb.UnimplementedManagementServiceServer

	op      atomic.Value // Operation (workerproxy layer)
	deploys atomic.Int32
	// updateErr 非空时 Update 返回该错误（模拟服务端校验拒绝）。
	updateErr error
}

func newMockOpsServer(t *testing.T) *mockOpsServer {
	t.Helper()
	m := &mockOpsServer{
		updateErr: status.Error(codes.InvalidArgument, "invalid config body"),
	}
	m.setOp(Operation{ID: "op1", Type: "deploy", Service: "web", Status: OpPending})
	return m
}

func (m *mockOpsServer) setOp(op Operation) { m.op.Store(op) }
func (m *mockOpsServer) opStatus() Operation {
	v, _ := m.op.Load().(Operation)
	return v
}

// toPBOp 把 workerproxy.Operation 映射回 pb.Operation（mock 用，与服务端序列化一致）。
func toPBOp(o Operation) *pb.Operation {
	return &pb.Operation{
		Id: o.ID, Type: o.Type, Service: o.Service, Status: string(o.Status),
		StartedAt: o.StartedAt, ServiceId: o.ServiceID, Error: o.Error,
		Steps: o.Steps, Replicas: o.Replicas, Mode: o.Mode,
	}
}

// --- orchestration RPCs ---

func (m *mockOpsServer) Deploy(_ context.Context, req *pb.DeployRequest) (*pb.Operation, error) {
	if req.GetIsJson() {
		return nil, status.Error(codes.InvalidArgument, "deploy expects yaml, got json")
	}
	if len(req.GetConfigBody()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "empty config body")
	}
	m.deploys.Add(1)
	m.setOp(Operation{ID: "op1", Type: "deploy", Service: "web", Status: OpRunning, StartedAt: "t0"})
	return toPBOp(m.opStatus()), nil
}

func (m *mockOpsServer) Update(_ context.Context, _ *pb.UpdateRequest) (*pb.Operation, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	m.setOp(Operation{ID: "op1", Type: "update", Service: "web", Status: OpHealthy})
	return toPBOp(m.opStatus()), nil
}

func (m *mockOpsServer) Scale(_ context.Context, req *pb.ScaleRequest) (*pb.Operation, error) {
	m.setOp(Operation{ID: "op2", Type: "scale", Service: req.GetName(), Status: OpHealthy, Replicas: req.GetReplicas()})
	return toPBOp(m.opStatus()), nil
}

func (m *mockOpsServer) Restart(_ context.Context, req *pb.RestartRequest) (*pb.Operation, error) {
	m.setOp(Operation{ID: "op3", Type: "restart", Service: req.GetName(), Status: OpDone})
	return toPBOp(m.opStatus()), nil
}

func (m *mockOpsServer) Remove(_ context.Context, req *pb.RemoveRequest) (*pb.Operation, error) {
	m.setOp(Operation{ID: "op4", Type: "remove", Service: req.GetName(), Status: OpDone})
	return toPBOp(m.opStatus()), nil
}

func (m *mockOpsServer) GetOperation(_ context.Context, _ *pb.GetOperationRequest) (*pb.Operation, error) {
	return toPBOp(m.opStatus()), nil
}

// --- workload read RPCs ---

// dockerServiceJSON 返回与旧 HTTP mock 相同的 Docker 原生 service 对象（数组），
// 供 ListWorkloads 经 mapWorkload 解码。
func dockerServicesJSON() []byte {
	raw := []map[string]any{{
		"ID": "svc1",
		"Spec": map[string]any{
			"Name":         "web",
			"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "nginx:alpine"}},
			"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 3}},
		},
		"ServiceStatus": map[string]any{"RunningTasks": 2, "DesiredTasks": 3},
		"Endpoint": map[string]any{"Ports": []map[string]any{{
			"Protocol": "tcp", "TargetPort": 80, "PublishedPort": 8080, "PublishMode": "ingress",
		}}},
	}}
	b, _ := json.Marshal(raw)
	return b
}

func (m *mockOpsServer) ListServices(_ context.Context, _ *pb.ListServicesRequest) (*pb.ListServicesResponse, error) {
	return &pb.ListServicesResponse{ServicesJson: dockerServicesJSON()}, nil
}

func (m *mockOpsServer) GetService(_ context.Context, _ *pb.GetServiceRequest) (*pb.ServiceDetail, error) {
	// 旧 HTTP mock 把 {service, tasks, running, desired, healthy} 作为一个 JSON
	// 对象返回。gRPC 的 ServiceDetail 拆成两个字段：service_json 是单个
	// dockerService（mapWorkload 解码），tasks_json 是 []dockerTask。
	svcJSON, _ := json.Marshal(map[string]any{
		"ID": "svc1",
		"Spec": map[string]any{
			"Name":         "web",
			"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "nginx:alpine"}},
			"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 3}},
		},
		"ServiceStatus": map[string]any{"RunningTasks": 3, "DesiredTasks": 3},
	})
	tasksJSON, _ := json.Marshal([]map[string]any{{
		"ID": "task1", "Slot": 1, "NodeID": "n1", "DesiredState": "running",
		"Status": map[string]any{
			"State":           "running",
			"ContainerStatus": map[string]any{"ContainerID": "c1", "ExitCode": 0},
		},
	}})
	return &pb.ServiceDetail{
		ServiceJson: svcJSON,
		TasksJson:   tasksJSON,
		Running:     3,
		Desired:     3,
		Healthy:     3,
	}, nil
}

// --- streaming RPCs ---

func (m *mockOpsServer) StreamLogs(req *pb.StreamLogsRequest, stream grpc.ServerStreamingServer[pb.LogLine]) error {
	if req.GetService() != "web" {
		return status.Error(codes.InvalidArgument, "unknown service")
	}
	if err := stream.Send(&pb.LogLine{Ts: "t1", Stream: "stdout", Line: "hello"}); err != nil {
		return err
	}
	if err := stream.Send(&pb.LogLine{Ts: "t2", Stream: "stderr", Line: "warn"}); err != nil {
		return err
	}
	return nil
}

// --- tests ---

func TestDeployAndPoll(t *testing.T) {
	m := newMockOpsServer(t)
	cli := startBufconnServer(t, m, "")

	op, err := cli.Deploy(context.Background(), "name: web\nimage: nginx:alpine\nreplicas: 2\n")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if op.Status != OpRunning || op.Type != "deploy" {
		t.Fatalf("unexpected op: %+v", op)
	}
	if m.deploys.Load() != 1 {
		t.Fatalf("expected 1 deploy call, got %d", m.deploys.Load())
	}

	// 轮询（mock 已置 healthy）
	m.setOp(Operation{ID: "op1", Type: "deploy", Service: "web", Status: OpHealthy})
	polled, err := cli.Operation(context.Background(), "op1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if polled.Status != OpHealthy {
		t.Fatalf("expected healthy, got %s", polled.Status)
	}
}

func TestScaleRestartRemove(t *testing.T) {
	m := newMockOpsServer(t)
	cli := startBufconnServer(t, m, "")

	sc, err := cli.Scale(context.Background(), "web", 5)
	if err != nil || sc.Status != OpHealthy || sc.Replicas != 5 {
		t.Fatalf("scale: %+v err=%v", sc, err)
	}
	rs, err := cli.Restart(context.Background(), "web")
	if err != nil || rs.Status != OpDone {
		t.Fatalf("restart: %+v err=%v", rs, err)
	}
	rm, err := cli.Remove(context.Background(), "web")
	if err != nil || rm.Status != OpDone {
		t.Fatalf("remove: %+v err=%v", rm, err)
	}
}

func TestListWorkloadsMapping(t *testing.T) {
	m := newMockOpsServer(t)
	cli := startBufconnServer(t, m, "")

	list, err := cli.ListWorkloads(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(list))
	}
	w := list[0]
	if w.Name != "web" || w.Image != "nginx:alpine" || w.Mode != "replicated" {
		t.Fatalf("unexpected workload: %+v", w)
	}
	if w.Replica != "2/3" || w.Running != 2 || w.Desired != 3 {
		t.Fatalf("replica fields: %+v", w)
	}
	if len(w.Ports) != 1 || w.Ports[0].PublishedPort != 8080 || w.Ports[0].TargetPort != 80 {
		t.Fatalf("ports: %+v", w.Ports)
	}
}

func TestGetWorkloadDetail(t *testing.T) {
	m := newMockOpsServer(t)
	cli := startBufconnServer(t, m, "")

	d, err := cli.GetWorkload(context.Background(), "web")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.Healthy != 3 || len(d.Tasks) != 1 || d.Tasks[0].State != "running" {
		t.Fatalf("unexpected detail: %+v", d)
	}
}

// TestStreamLogs mock 返回 gRPC 服务端流式日志，校验逐行解码。
func TestStreamLogs(t *testing.T) {
	m := newMockOpsServer(t)
	cli := startBufconnServer(t, m, "")

	var got []LogLine
	err := cli.StreamLogs(context.Background(), "web", false, 0, "", func(ll LogLine) bool {
		got = append(got, ll)
		return true
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(got) != 2 || got[0].Line != "hello" || got[1].Stream != "stderr" {
		t.Fatalf("unexpected lines: %+v", got)
	}
}

// TestStreamLogsEarlyExit handler 返回 false 时提前断开。
// mock 会发送 100 行，但 handler 在第 3 行返回 false 后客户端应停止 Recv。
func TestStreamLogsEarlyExit(t *testing.T) {
	srv := &streamLogsCountServer{}
	cli := startBufconnServer(t, srv, "")

	count := 0
	err := cli.StreamLogs(context.Background(), "web", false, 0, "", func(ll LogLine) bool {
		count++
		return count < 3
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected early exit at 3, got %d", count)
	}
}

// TestDeployWrongContentType 校验服务端拒绝请求时客户端得到 *ErrUnreachable。
// 旧版用 HTTP Content-Type 校验返回 400；gRPC 下没有 Content-Type，改为服务端
// Update RPC 返回 InvalidArgument（等价的“请求格式被拒”语义），客户端应把它包装成
// *ErrUnreachable，底层 gRPC code 为 InvalidArgument。
func TestDeployWrongContentType(t *testing.T) {
	m := newMockOpsServer(t)
	cli := startBufconnServer(t, m, "")

	_, err := cli.Update(context.Background(), "web", "name: web")
	if err == nil {
		t.Fatal("expected error for rejected update")
	}
	ue, ok := err.(*ErrUnreachable)
	if !ok {
		t.Fatalf("expected *ErrUnreachable, got %T", err)
	}
	if status.Code(ue.Err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", ue.Err)
	}
}

// streamLogsCountServer 发送 100 行日志，用于 TestStreamLogsEarlyExit。
type streamLogsCountServer struct {
	pb.UnimplementedManagementServiceServer
}

func (m *streamLogsCountServer) StreamLogs(req *pb.StreamLogsRequest, stream grpc.ServerStreamingServer[pb.LogLine]) error {
	for i := 0; i < 100; i++ {
		if err := stream.Send(&pb.LogLine{Ts: "t", Stream: "stdout", Line: "l"}); err != nil {
			return err
		}
	}
	return nil
}
