package alertrule

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
)

// alertWorkerServer 是 alertrule 测试用的 mock Worker。实现:
//   - Self: 返回 manager（兼容任何潜在探测）
//   - GetService: 返回单个 docker 原生 service（nginx:alpine, 2/2 副本）
//   - Update: 记录被调用次数，返回 healthy 操作
//
// 其余 RPC 走 UnimplementedManagementServiceServer。
type alertWorkerServer struct {
	pb.UnimplementedManagementServiceServer
	updated atomic.Int32
}

func (m *alertWorkerServer) Self(context.Context, *pb.Empty) (*pb.SelfInfo, error) {
	return &pb.SelfInfo{
		NodeId: "n1", Hostname: "h1", Role: "manager",
		Leader: true, State: "ready", SwarmManager: true,
	}, nil
}

func (m *alertWorkerServer) GetService(_ context.Context, req *pb.GetServiceRequest) (*pb.ServiceDetail, error) {
	// 旧 HTTP mock 把 {service, tasks, running, desired, healthy} 作为一个 JSON
	// 对象返回。gRPC 的 ServiceDetail 拆成 service_json（单个 dockerService，
	// mapWorkload 解码）+ tasks_json（[]dockerTask）+ running/desired/healthy。
	svcJSON, _ := json.Marshal(map[string]any{
		"ID": "s1",
		"Spec": map[string]any{
			"Name":         req.GetName(),
			"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "nginx:alpine"}},
			"Mode":         map[string]any{"Replicated": map[string]any{"Replicas": 2}},
		},
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

func (m *alertWorkerServer) Update(_ context.Context, req *pb.UpdateRequest) (*pb.Operation, error) {
	// 旧 handler 校验收到的 config 含 monitoring 块；这里只记录被调用，
	// 返回 healthy 操作（Apply 仅在 op.Status=="failed" 时报错）。
	m.updated.Add(1)
	return &pb.Operation{
		Id: "op1", Type: "update", Service: req.GetName(), Status: "healthy",
	}, nil
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

func newTestRule(t *testing.T) (*Service, *alertWorkerServer) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := &alertWorkerServer{}
	url := startGRPCWorker(t, srv)

	cs := cluster.New(st)
	if err := st.PutCluster(&store.Cluster{Name: "dev", WorkerURL: url, Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	return New(st, cs), srv
}

// TestUpsertValidate 校验：阈值范围/必填字段。
func TestUpsertValidate(t *testing.T) {
	svc, _ := newTestRule(t)
	// 非法阈值
	_, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 150}},
	}})
	if err == nil {
		t.Fatal("threshold >100 should be rejected")
	}
	// 非法 metric
	_, err = svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "disk", Threshold: 80}},
	}})
	if err == nil {
		t.Fatal("unknown metric should be rejected")
	}
	// 合法
	r, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 85}},
	}})
	if err != nil || r == nil || r.UpdatedAt.IsZero() {
		t.Fatalf("upsert: %v %+v", err, r)
	}
}

// TestApplyPushToWorker 规则下发：合并 monitoring → Worker Update 被调用。
func TestApplyPushToWorker(t *testing.T) {
	svc, _ := newTestRule(t)
	r := &store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 85}},
	}}
	if err := svc.Apply(context.Background(), r); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 本地副本持久化
	got, err := svc.Get("dev", "web")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Monitoring.ResourceThresholds[0].Threshold != 85 {
		t.Fatalf("monitoring not persisted: %+v", got)
	}
}

// TestDeleteNotFound 删除不存在 → ErrNotFound。
func TestDeleteNotFound(t *testing.T) {
	svc, _ := newTestRule(t)
	if err := svc.Delete("dev", "nope"); err == nil {
		t.Fatal("delete missing should fail")
	}
}
