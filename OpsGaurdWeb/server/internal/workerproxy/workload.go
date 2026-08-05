// Workload operations over the Worker API: deploy/update/scale/restart/
// remove, async operation polling, service detail, and SSE log streaming.
//
// Worker contract (see Worker/internal/orchestrator + nodeagent):
//
//	POST   /api/v1/services                     body=config(YAML/JSON) -> 202 Operation
//	POST   /api/v1/services/{name}              update config           -> 202 Operation
//	POST   /api/v1/services/{name}/scale        {"replicas":N}          -> 202 Operation
//	POST   /api/v1/services/{name}/restart      -> 202 Operation
//	DELETE /api/v1/services/{name}              -> 200 Operation
//	GET    /api/v1/operations/{id}              Operation (poll)
//	GET    /api/v1/services/{name}              ServiceDetail{service,tasks,running,desired,healthy}
//	GET    /api/v1/services                     []Service (Docker native)
//	GET    /api/v1/local/logs?service=X&follow&tail&since  SSE {"ts","stream","line"}
package workerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// --- 操作（异步编排） ---

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

// Operation 对应 Worker GET /api/v1/operations/{id}。
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

// Deploy 部署服务（config 为 Worker 的 YAML/JSON 配置体，Content-Type: yaml）。
func (c *Client) Deploy(ctx context.Context, configYAML string) (Operation, error) {
	var op Operation
	err := c.doWithHooks(ctx, http.MethodPost, "/api/v1/services", nil, []byte(configYAML), &op,
		func(r *http.Request) { r.Header.Set("Content-Type", "text/yaml") })
	return op, err
}

// Update 更新服务配置。
func (c *Client) Update(ctx context.Context, name, configYAML string) (Operation, error) {
	var op Operation
	err := c.doWithHooks(ctx, http.MethodPost, "/api/v1/services/"+name, nil, []byte(configYAML), &op,
		func(r *http.Request) { r.Header.Set("Content-Type", "text/yaml") })
	return op, err
}

// Scale 调整副本数。
func (c *Client) Scale(ctx context.Context, name string, replicas uint64) (Operation, error) {
	var op Operation
	body, _ := json.Marshal(map[string]uint64{"replicas": replicas})
	err := c.do(ctx, http.MethodPost, "/api/v1/services/"+name+"/scale", nil, body, &op)
	return op, err
}

// Restart 强制重启服务（ForceUpdate）。
func (c *Client) Restart(ctx context.Context, name string) (Operation, error) {
	var op Operation
	err := c.do(ctx, http.MethodPost, "/api/v1/services/"+name+"/restart", nil, nil, &op)
	return op, err
}

// Remove 删除服务。
func (c *Client) Remove(ctx context.Context, name string) (Operation, error) {
	var op Operation
	err := c.do(ctx, http.MethodDelete, "/api/v1/services/"+name, nil, nil, &op)
	return op, err
}

// Operation 查询操作状态。
func (c *Client) Operation(ctx context.Context, id string) (Operation, error) {
	var op Operation
	err := c.do(ctx, http.MethodGet, "/api/v1/operations/"+id, nil, nil, &op)
	return op, err
}

// --- 服务视图 ---

// Workload 前端友好的服务摘要（由 Docker 原生 Service 映射）。
type Workload struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Image   string         `json:"image,omitempty"`
	Mode    string         `json:"mode,omitempty"` // replicated | global
	Replica string         `json:"replica,omitempty"` // 如 3/3
	Running uint64         `json:"running,omitempty"`
	Desired uint64         `json:"desired,omitempty"`
	Ports   []PortMapping  `json:"ports,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// PortMapping 端口映射（发布端口 → 目标端口）。
type PortMapping struct {
	PublishedPort uint32 `json:"publishedPort"`
	TargetPort    uint32 `json:"targetPort"`
	Protocol      string `json:"protocol,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

// dockerService Docker 原生 Service（最小子集，用于映射）。
type dockerService struct {
	ID            string        `json:"ID"`
	Spec          dockerSpec    `json:"Spec"`
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
		w.Desired = 0 // global 无固定副本数
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
	var raw []dockerService
	err := c.do(ctx, http.MethodGet, "/api/v1/services", map[string]string{"label": label}, nil, &raw)
	if err != nil {
		return nil, err
	}
	out := make([]Workload, 0, len(raw))
	for _, ds := range raw {
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

// GetWorkload 获取服务详情（GET /api/v1/services/{name}）。
func (c *Client) GetWorkload(ctx context.Context, name string) (WorkloadDetail, error) {
	var raw struct {
		Service dockerService   `json:"service"`
		Tasks   []dockerTask    `json:"tasks"`
		Running int             `json:"running"`
		Desired int             `json:"desired"`
		Healthy int             `json:"healthy"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/services/"+name, nil, nil, &raw)
	if err != nil {
		return WorkloadDetail{}, err
	}
	d := WorkloadDetail{Workload: mapWorkload(raw.Service), Healthy: raw.Healthy}
	// 权威 running/desired 来自 Worker 顶层字段（真机 Inspect 的 service 无
	// ServiceStatus）；仅在缺失时回退到 ServiceStatus 映射值。
	if raw.Running > 0 || raw.Desired > 0 {
		d.Running = uint64(raw.Running)
		d.Desired = uint64(raw.Desired)
		d.Replica = fmt.Sprintf("%d/%d", raw.Running, raw.Desired)
	}
	for _, t := range raw.Tasks {
		d.Tasks = append(d.Tasks, TaskView{
			ID:           t.ID,
			Slot:         t.Slot,
			NodeID:       t.NodeID,
			State:        t.Status.State,
			DesiredState: t.DesiredState,
			Message:      t.Status.Message,
			Err:          t.Status.Err,
			ContainerID:  t.Status.ContainerStatus.ContainerID,
			ExitCode:     t.Status.ContainerStatus.ExitCode,
		})
	}
	return d, nil
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

// LogLine 一行日志（Worker /local/logs SSE 事件体）。
type LogLine struct {
	TS     string `json:"ts"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// StreamLogs 流式拉取服务日志。事件体格式 data: {"ts","stream","line"}。
// handler 返回 false 时停止；ctx 取消或流结束即返回。
func (c *Client) StreamLogs(ctx context.Context, service string, follow bool, tail int, since string, handler func(LogLine) bool) error {
	q := map[string]string{
		"service": service,
		"follow":  strconv.FormatBool(follow),
	}
	if tail > 0 {
		q["tail"] = strconv.Itoa(tail)
	}
	if since != "" {
		q["since"] = since
	}

	url := c.baseURL + "/api/v1/local/logs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &ErrUnreachable{URL: url, Err: err}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	qq := req.URL.Query()
	for k, v := range q {
		if v != "" {
			qq.Set(k, v)
		}
	}
	req.URL.RawQuery = qq.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return &ErrUnreachable{URL: url, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: fmt.Errorf("%s", strings.TrimSpace(string(raw)))}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ll LogLine
		if err := json.Unmarshal([]byte(payload), &ll); err != nil {
			continue
		}
		if !handler(ll) {
			return nil
		}
	}
	return sc.Err()
}

// doWithHooks 发起请求并解码 JSON（支持请求体钩子，如覆盖 Content-Type）。
func (c *Client) doWithHooks(ctx context.Context, method, path string, query map[string]string, body []byte, v any, hook func(*http.Request)) error {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return &ErrUnreachable{URL: url, Err: err}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if hook != nil {
		hook(req)
	}
	q := req.URL.Query()
	for k, val := range query {
		if val != "" {
			q.Set(k, val)
		}
	}
	req.URL.RawQuery = q.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return &ErrUnreachable{URL: url, Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: fmt.Errorf("%s", truncate(string(raw), 512))}
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: fmt.Errorf("decode: %w", err)}
	}
	return nil
}
