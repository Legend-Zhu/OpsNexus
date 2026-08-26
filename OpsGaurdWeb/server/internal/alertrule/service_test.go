package alertrule

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// alertWorkerServer 是 alertrule 测试用的 mock Worker。实现:
//   - Self: 返回 manager（兼容任何潜在探测）
//   - GetService: 返回单个 docker 原生 service（nginx:alpine, 2/2 副本）；
//     notFound=true 时对所有服务返回 codes.NotFound（模拟服务不存在）
//   - Update: 记录被调用次数与最近一次 config，返回 healthy 操作
//
// 其余 RPC 走 UnimplementedManagementServiceServer。
type alertWorkerServer struct {
	pb.UnimplementedManagementServiceServer
	updated    atomic.Int32
	lastConfig atomic.Pointer[string]
	notFound   atomic.Bool
}

func (m *alertWorkerServer) Self(context.Context, *pb.Empty) (*pb.SelfInfo, error) {
	return &pb.SelfInfo{
		NodeId: "n1", Hostname: "h1", Role: "manager",
		Leader: true, State: "ready", SwarmManager: true,
	}, nil
}

func (m *alertWorkerServer) GetService(_ context.Context, req *pb.GetServiceRequest) (*pb.ServiceDetail, error) {
	if m.notFound.Load() {
		return nil, status.Error(codes.NotFound, "service "+req.GetName()+" not found")
	}
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
	// 记录被调用次数与最近一次 config 体（测试断言监控禁用块用）。
	m.updated.Add(1)
	cfg := string(req.GetConfigBody())
	m.lastConfig.Store(&cfg)
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

func newTestRule(t *testing.T) (*Service, *alertWorkerServer, *store.Store) {
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
	return New(st, cs), srv, st
}

// TestUpsertValidate 校验：阈值范围/必填字段。
func TestUpsertValidate(t *testing.T) {
	svc, _, _ := newTestRule(t)
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
	svc, _, _ := newTestRule(t)
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
	svc, _, _ := newTestRule(t)
	if _, err := svc.Delete(context.Background(), "dev", "nope", false); err == nil {
		t.Fatal("delete missing should fail")
	} else {
		var nf ErrNotFound
		if !errors.As(err, &nf) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
}

// TestDeleteStopsSwarmMonitoring 删除已下发的 swarm 规则：
// 向 Worker 下发 enabled: false 的禁用配置后删除规则。
func TestDeleteStopsSwarmMonitoring(t *testing.T) {
	svc, srv, _ := newTestRule(t)
	r := &store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		Enabled:            true,
		ResourceThresholds: []store.ResourceThreshold{{Metric: "cpu", Threshold: 85}},
	}}
	if err := svc.Apply(context.Background(), r); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if srv.updated.Load() != 1 {
		t.Fatalf("apply should call Update once, got %d", srv.updated.Load())
	}

	res, err := svc.Delete(context.Background(), "dev", "web", false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !res.Stopped || res.StopErr != "" {
		t.Fatalf("want stopped without error, got %+v", res)
	}
	// 第二次 Update：禁用配置
	if srv.updated.Load() != 2 {
		t.Fatalf("delete should push disabled config, Update calls = %d", srv.updated.Load())
	}
	last := srv.lastConfig.Load()
	if last == nil {
		t.Fatal("no Update config captured")
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(*last), &cfg); err != nil {
		t.Fatalf("unmarshal pushed config: %v", err)
	}
	mon, _ := cfg["monitoring"].(map[string]any)
	if mon == nil {
		t.Fatalf("pushed config has no monitoring block: %s", *last)
	}
	if enabled, _ := mon["enabled"].(bool); enabled {
		t.Fatalf("pushed config should disable monitoring: %s", *last)
	}
	// 规则记录已删
	got, err := svc.Get("dev", "web")
	if err != nil || got != nil {
		t.Fatalf("rule should be deleted, got %+v err=%v", got, err)
	}
}

// TestDeleteClearsInventoryMonitoring 删除纳管规则：清除清单条目的监控声明。
func TestDeleteClearsInventoryMonitoring(t *testing.T) {
	svc, srv, st := newTestRule(t)
	srv.notFound.Store(true) // 非 swarm 服务

	// 集群清单中带启用监控的条目
	rec, err := st.GetCluster("dev")
	if err != nil || rec == nil {
		t.Fatalf("get cluster: %v", err)
	}
	rec.Inventory = &store.InventoryConfig{Items: []store.InventoryItem{{
		Name: "hostapp", Type: "host-service", Ref: "10.0.0.1",
		Monitoring: &store.Monitoring{Enabled: true, PortChecks: []store.PortCheck{{Port: "8080"}}},
	}}}
	if err := st.PutCluster(rec); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	if _, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "hostapp", Monitoring: store.Monitoring{
		Enabled:    true,
		PortChecks: []store.PortCheck{{Port: "8080"}},
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	res, err := svc.Delete(context.Background(), "dev", "hostapp", false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !res.Stopped {
		t.Fatalf("want stopped, got %+v", res)
	}
	if srv.updated.Load() != 0 {
		t.Fatalf("inventory path should not call Worker Update, got %d", srv.updated.Load())
	}
	// 清单条目监控声明已清除，其余字段不变
	rec, err = st.GetCluster("dev")
	if err != nil || rec == nil || rec.Inventory == nil || len(rec.Inventory.Items) != 1 {
		t.Fatalf("inventory lost: %v %+v", err, rec)
	}
	item := rec.Inventory.Items[0]
	if item.Monitoring != nil {
		t.Fatalf("item monitoring should be cleared, got %+v", item.Monitoring)
	}
	if item.Name != "hostapp" || item.Ref != "10.0.0.1" {
		t.Fatalf("item fields should be preserved: %+v", item)
	}
	// 规则记录已删
	got, err := svc.Get("dev", "hostapp")
	if err != nil || got != nil {
		t.Fatalf("rule should be deleted, got %+v err=%v", got, err)
	}
}

// TestDeleteFailFastAndForce Worker 不可达：默认删除失败且规则保留；
// force 强删成功并如实报告监控未停止。
func TestDeleteFailFastAndForce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// 无 Worker 监听：端口 1 连接被拒（快速失败，区别于超时）
	if err := st.PutCluster(&store.Cluster{Name: "dev", WorkerURL: "http://127.0.0.1:1", Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	svc := New(st, cluster.New(st))
	if _, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		Enabled: true, PortChecks: []store.PortCheck{{Port: "80"}},
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// 非 force：失败且规则保留
	_, err = svc.Delete(context.Background(), "dev", "web", false)
	if err == nil {
		t.Fatal("delete should fail when worker unreachable")
	}
	var sf ErrStopFailed
	if !errors.As(err, &sf) {
		t.Fatalf("want ErrStopFailed, got %v", err)
	}
	got, _ := svc.Get("dev", "web")
	if got == nil {
		t.Fatal("rule should be kept on stop failure")
	}

	// force：强删，报告未停止
	res, err := svc.Delete(context.Background(), "dev", "web", true)
	if err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if res.Stopped || res.StopErr == "" {
		t.Fatalf("want stopped=false with stopError, got %+v", res)
	}
	got, _ = svc.Get("dev", "web")
	if got != nil {
		t.Fatal("rule should be deleted with force")
	}
}

// TestRemoveClusterPurgesRules 集群删除回调：清理该集群全部规则，其他集群不受影响。
func TestRemoveClusterPurgesRules(t *testing.T) {
	svc, _, st := newTestRule(t)
	if err := st.PutCluster(&store.Cluster{Name: "prod", WorkerURL: "http://127.0.0.1:1", Status: store.ClusterOnline}); err != nil {
		t.Fatalf("put cluster: %v", err)
	}
	if _, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{Enabled: true}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "db", Monitoring: store.Monitoring{Enabled: true}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := svc.Upsert(&store.AlertRule{Cluster: "prod", Service: "web", Monitoring: store.Monitoring{Enabled: true}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := svc.RemoveCluster("dev"); err != nil {
		t.Fatalf("remove cluster rules: %v", err)
	}
	devRules, err := svc.List("dev")
	if err != nil || len(devRules) != 0 {
		t.Fatalf("dev rules should be purged, got %+v err=%v", devRules, err)
	}
	prodRules, err := svc.List("prod")
	if err != nil || len(prodRules) != 1 || prodRules[0].Service != "web" {
		t.Fatalf("prod rules should be intact, got %+v err=%v", prodRules, err)
	}
}

// TestDeleteClusterGone 集群已删除：无生效点，直接删规则。
func TestDeleteClusterGone(t *testing.T) {
	svc, _, st := newTestRule(t)
	if _, err := svc.Upsert(&store.AlertRule{Cluster: "dev", Service: "web", Monitoring: store.Monitoring{
		Enabled: true, PortChecks: []store.PortCheck{{Port: "80"}},
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.DeleteCluster("dev"); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
	res, err := svc.Delete(context.Background(), "dev", "web", false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !res.Stopped {
		t.Fatalf("want stopped=true for gone cluster, got %+v", res)
	}
}
