// Workload operations over the Worker gRPC management API: deploy/update/
// scale/restart/remove, async operation polling, service detail, log streaming,
// and event/audit subscription. Method signatures match the previous HTTP
// implementation so upper layers are unchanged.
package workerproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"
	"google.golang.org/grpc/metadata"
)

// --- 操作（异步编排）---

// OperationStatus 编排操作状态。
type OperationStatus string

const (
	OpPending  OperationStatus = "pending"
	OpRunning  OperationStatus = "running"
	OpHealthy  OperationStatus = "healthy"
	OpDone     OperationStatus = "done"
	OpFailed   OperationStatus = "failed"
	OpPartial  OperationStatus = "partial"
	OpCanceled OperationStatus = "canceled"
)

// Operation 对应 Worker GetOperation 响应。
type Operation struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Service    string          `json:"service"`
	Status     OperationStatus `json:"status"`
	StartedAt  string          `json:"startedAt"`
	FinishedAt *string         `json:"finishedAt,omitempty"`
	ServiceID  string          `json:"serviceId,omitempty"`
	Error      string          `json:"error,omitempty"`
	Steps      []string        `json:"steps,omitempty"`
	Replicas   uint64          `json:"replicas,omitempty"`
	Mode       string          `json:"mode,omitempty"`
}

func opFromPB(o *pb.Operation) Operation {
	if o == nil {
		return Operation{}
	}
	out := Operation{
		ID: o.GetId(), Type: o.GetType(), Service: o.GetService(),
		Status: OperationStatus(o.GetStatus()), StartedAt: o.GetStartedAt(),
		ServiceID: o.GetServiceId(), Error: o.GetError(),
		Steps: o.GetSteps(), Replicas: o.GetReplicas(), Mode: o.GetMode(),
	}
	if f := o.GetFinishedAt(); f != "" {
		out.FinishedAt = &f
	}
	return out
}

// Deploy 部署服务（configYAML 为 Worker 的 YAML 配置体）。
func (c *Client) Deploy(ctx context.Context, configYAML string) (Operation, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	op, err := c.stub.Deploy(cctx, &pb.DeployRequest{ConfigBody: []byte(configYAML), IsJson: false})
	if err != nil {
		return Operation{}, c.wrapErr(err)
	}
	return opFromPB(op), nil
}

// Update 更新服务配置。
func (c *Client) Update(ctx context.Context, name, configYAML string) (Operation, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	op, err := c.stub.Update(cctx, &pb.UpdateRequest{Name: name, ConfigBody: []byte(configYAML), IsJson: false})
	if err != nil {
		return Operation{}, c.wrapErr(err)
	}
	return opFromPB(op), nil
}

// Scale 调整副本数。
func (c *Client) Scale(ctx context.Context, name string, replicas uint64) (Operation, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	op, err := c.stub.Scale(cctx, &pb.ScaleRequest{Name: name, Replicas: replicas})
	if err != nil {
		return Operation{}, c.wrapErr(err)
	}
	return opFromPB(op), nil
}

// Restart 强制重启服务（ForceUpdate）。
func (c *Client) Restart(ctx context.Context, name string) (Operation, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	op, err := c.stub.Restart(cctx, &pb.RestartRequest{Name: name})
	if err != nil {
		return Operation{}, c.wrapErr(err)
	}
	return opFromPB(op), nil
}

// Remove 删除服务。
func (c *Client) Remove(ctx context.Context, name string) (Operation, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	op, err := c.stub.Remove(cctx, &pb.RemoveRequest{Name: name})
	if err != nil {
		return Operation{}, c.wrapErr(err)
	}
	return opFromPB(op), nil
}

// Operation 查询操作状态。
func (c *Client) Operation(ctx context.Context, id string) (Operation, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	op, err := c.stub.GetOperation(cctx, &pb.GetOperationRequest{Id: id})
	if err != nil {
		return Operation{}, c.wrapErr(err)
	}
	return opFromPB(op), nil
}

// --- 服务视图 ---

// Workload 前端友好的服务摘要（由 Docker 原生 Service 映射）。
type Workload struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image,omitempty"`
	Mode    string            `json:"mode,omitempty"`
	Replica string            `json:"replica,omitempty"`
	Running uint64            `json:"running,omitempty"`
	Desired uint64            `json:"desired,omitempty"`
	Ports   []PortMapping     `json:"ports,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// PortMapping 端口映射。
type PortMapping struct {
	PublishedPort uint32 `json:"publishedPort"`
	TargetPort    uint32 `json:"targetPort"`
	Protocol      string `json:"protocol,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

type dockerService struct {
	ID            string     `json:"ID"`
	Spec          dockerSpec `json:"Spec"`
	ServiceStatus *struct {
		RunningTasks   uint64 `json:"RunningTasks"`
		DesiredTasks   uint64 `json:"DesiredTasks"`
		CompletedTasks uint64 `json:"CompletedTasks"`
	} `json:"ServiceStatus,omitempty"`
	Endpoint struct {
		Ports []struct {
			Protocol      string `json:"Protocol"`
			TargetPort    uint32 `json:"TargetPort"`
			PublishedPort uint32 `json:"PublishedPort"`
			PublishMode   string `json:"PublishMode"`
		} `json:"Ports"`
	} `json:"Endpoint,omitempty"`
}

type dockerSpec struct {
	Name         string            `json:"Name"`
	Labels       map[string]string `json:"Labels,omitempty"`
	TaskTemplate struct {
		ContainerSpec struct {
			Image string `json:"Image"`
		} `json:"ContainerSpec"`
	} `json:"TaskTemplate"`
	Mode struct {
		Replicated *struct {
			Replicas uint64 `json:"Replicas"`
		} `json:"Replicated,omitempty"`
		Global *struct{} `json:"Global,omitempty"`
	} `json:"Mode"`
}

func mapWorkload(ds dockerService) Workload {
	w := Workload{
		ID:     ds.ID,
		Name:   ds.Spec.Name,
		Image:  ds.Spec.TaskTemplate.ContainerSpec.Image,
		Labels: ds.Spec.Labels,
	}
	if ds.Spec.Mode.Replicated != nil {
		w.Mode = "replicated"
		w.Desired = ds.Spec.Mode.Replicated.Replicas
	} else if ds.Spec.Mode.Global != nil {
		w.Mode = "global"
	}
	if ds.ServiceStatus != nil {
		w.Running = ds.ServiceStatus.RunningTasks
		if w.Mode == "replicated" {
			w.Desired = ds.ServiceStatus.DesiredTasks
		}
	}
	if w.Mode == "replicated" {
		w.Replica = fmt.Sprintf("%d/%d", w.Running, w.Desired)
	} else {
		w.Replica = fmt.Sprintf("%d running", w.Running)
	}
	for _, p := range ds.Endpoint.Ports {
		w.Ports = append(w.Ports, PortMapping{
			PublishedPort: p.PublishedPort,
			TargetPort:    p.TargetPort,
			Protocol:      p.Protocol,
			Mode:          p.PublishMode,
		})
	}
	return w
}

// ListWorkloads 列出集群服务（映射为前端友好视图）。
func (c *Client) ListWorkloads(ctx context.Context, label string) ([]Workload, error) {
	raw, err := c.Services(ctx, label)
	if err != nil {
		return nil, err
	}
	var svcs []dockerService
	if err := json.Unmarshal(raw, &svcs); err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	out := make([]Workload, 0, len(svcs))
	for _, ds := range svcs {
		out = append(out, mapWorkload(ds))
	}
	return out, nil
}

// WorkloadDetail 服务详情（摘要 + tasks + 健康计数）。
type WorkloadDetail struct {
	Workload
	Tasks   []TaskView `json:"tasks"`
	Healthy int        `json:"healthy"`
}

// TaskView 任务的轻量视图。
type TaskView struct {
	ID           string `json:"id"`
	Slot         int    `json:"slot,omitempty"`
	NodeID       string `json:"nodeId,omitempty"`
	State        string `json:"state"`
	DesiredState string `json:"desiredState"`
	Message      string `json:"message,omitempty"`
	Err          string `json:"err,omitempty"`
	ContainerID  string `json:"containerId,omitempty"`
	ExitCode     int    `json:"exitCode,omitempty"`
}

type dockerTask struct {
	ID           string `json:"ID"`
	Slot         int    `json:"Slot"`
	NodeID       string `json:"NodeID"`
	DesiredState string `json:"DesiredState"`
	Status       struct {
		State           string `json:"State"`
		Message         string `json:"Message"`
		Err             string `json:"Err"`
		ContainerStatus struct {
			ContainerID string `json:"ContainerID"`
			ExitCode    int    `json:"ExitCode"`
		} `json:"ContainerStatus"`
	} `json:"Status"`
}

// GetWorkload 获取服务详情。
func (c *Client) GetWorkload(ctx context.Context, name string) (WorkloadDetail, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.GetService(cctx, &pb.GetServiceRequest{Name: name})
	if err != nil {
		return WorkloadDetail{}, c.wrapErr(err)
	}
	var svc dockerService
	if err := json.Unmarshal(resp.GetServiceJson(), &svc); err != nil {
		return WorkloadDetail{}, fmt.Errorf("decode service: %w", err)
	}
	var tasks []dockerTask
	_ = json.Unmarshal(resp.GetTasksJson(), &tasks)

	d := WorkloadDetail{Workload: mapWorkload(svc), Healthy: int(resp.GetHealthy())}
	if r := resp.GetRunning(); r > 0 || resp.GetDesired() > 0 {
		d.Running = uint64(r)
		d.Desired = uint64(resp.GetDesired())
		d.Replica = fmt.Sprintf("%d/%d", r, resp.GetDesired())
	}
	for _, t := range tasks {
		d.Tasks = append(d.Tasks, TaskView{
			ID: t.ID, Slot: t.Slot, NodeID: t.NodeID,
			State: t.Status.State, DesiredState: t.DesiredState,
			Message: t.Status.Message, Err: t.Status.Err,
			ContainerID: t.Status.ContainerStatus.ContainerID,
			ExitCode:    t.Status.ContainerStatus.ExitCode,
		})
	}
	return d, nil
}

// --- 流式 ---

// LogLine 一行日志。
type LogLine struct {
	TS     string `json:"ts"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// StreamLogs 流式拉取服务日志（gRPC server-streaming）。
// handler 返回 false 时停止；ctx 取消或流结束即返回。
func (c *Client) StreamLogs(ctx context.Context, service string, follow bool, tail int, since string, handler func(LogLine) bool) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Attach bearer token to the stream context (server-streaming RPCs read
	// metadata from the context passed to the open call; unary RPCs use callCtx,
	// streams do not — without this an auth-enabled worker rejects with 401).
	if c.token != "" {
		cctx = metadata.AppendToOutgoingContext(cctx, "authorization", "Bearer "+c.token)
	}
	stream, err := c.stub.StreamLogs(cctx, &pb.StreamLogsRequest{
		Service: service, Follow: follow, Tail: int32(tail), Since: since,
	})
	if err != nil {
		return c.wrapErr(err)
	}
	for {
		ll, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return c.wrapErr(err)
		}
		if !handler(LogLine{TS: ll.GetTs(), Stream: ll.GetStream(), Line: ll.GetLine()}) {
			return nil
		}
	}
}

// --- 事件/审计订阅（双向流，供 ingest.Subscriber 使用）---

// EventSubscription wraps a SubscribeEvents bidirectional stream. The caller
// starts at AfterSeq, receives MonitorEvent messages, and sends Acks.
type EventSubscription struct {
	stream pb.ManagementService_SubscribeEventsClient
}

// Recv blocks for the next event from the Worker.
func (s *EventSubscription) Recv() (*pb.MonitorEvent, error) {
	return s.stream.Recv()
}

// Ack acknowledges receipt of seq so the Worker can GC persisted events.
func (s *EventSubscription) Ack(seq int64) error {
	return s.stream.Send(&pb.SubscribeRequest{AckSeq: seq})
}

// SubscribeEvents opens a bidirectional event subscription starting after
// afterSeq (0 = from the beginning). The caller owns Recv/Ack; the stream
// stays open until Recv returns io.EOF or an error.
func (c *Client) SubscribeEvents(ctx context.Context, afterSeq int64) (*EventSubscription, error) {
	// Attach bearer token to the stream context (bidi streams read metadata from
	// the context passed to the open call; unary RPCs use callCtx, streams do not).
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	stream, err := c.stub.SubscribeEvents(ctx)
	if err != nil {
		return nil, c.wrapErr(err)
	}
	// Send the initial cursor request.
	if err := stream.Send(&pb.SubscribeRequest{AfterSeq: afterSeq}); err != nil {
		return nil, c.wrapErr(err)
	}
	return &EventSubscription{stream: stream}, nil
}

// AuditSubscription wraps a SubscribeAudit bidirectional stream.
type AuditSubscription struct {
	stream pb.ManagementService_SubscribeAuditClient
}

func (s *AuditSubscription) Recv() (*pb.AuditEntry, error) {
	return s.stream.Recv()
}

func (s *AuditSubscription) Ack(seq int64) error {
	return s.stream.Send(&pb.SubscribeRequest{AckSeq: seq})
}

// SubscribeAudit opens a bidirectional audit subscription starting after afterSeq.
func (c *Client) SubscribeAudit(ctx context.Context, afterSeq int64) (*AuditSubscription, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	stream, err := c.stub.SubscribeAudit(ctx)
	if err != nil {
		return nil, c.wrapErr(err)
	}
	if err := stream.Send(&pb.SubscribeRequest{AfterSeq: afterSeq}); err != nil {
		return nil, c.wrapErr(err)
	}
	return &AuditSubscription{stream: stream}, nil
}
