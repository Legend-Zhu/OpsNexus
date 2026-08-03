package orchestrator

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// Orchestrator drives service lifecycle: it translates configs, calls the
// Docker Engine API, and tracks convergence as Operations. Long-running
// convergence (readiness polling) happens in background goroutines.
type Orchestrator struct {
	cli          docker.Client
	store        *OperationStore
	log          *slog.Logger
	readyTimeout time.Duration
	mon          MonitorRegistrar // optional P2 hook; nil disables
}

// MonitorRegistrar is the P2 monitoring hook implemented by monitor.Manager.
// It is a separate interface so orchestrator does not import the monitor
// package (no import cycle: monitor imports docker+config only).
type MonitorRegistrar interface {
	Register(service string, cfg *config.Monitoring)
	Unregister(service string)
}

// New constructs an Orchestrator over the given Docker client and operation
// store. readyTimeout bounds how long convergence is polled (default 5m).
func New(cli docker.Client, store *OperationStore, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		cli:          cli,
		store:        store,
		log:          log,
		readyTimeout: 5 * time.Minute,
	}
}

// SetReadyTimeout overrides the readiness polling deadline (e.g. for tests).
func (o *Orchestrator) SetReadyTimeout(d time.Duration) {
	if d > 0 {
		o.readyTimeout = d
	}
}

// SetMonitor wires the P2 monitoring registrar (nil disables).
func (o *Orchestrator) SetMonitor(m MonitorRegistrar) {
	o.mon = m
}

// SelfInfo describes the local node and its swarm role. Used by the HTTP API
// and MCP to tell agents where this Worker sits in the cluster (HA awareness).
type SelfInfo struct {
	NodeID       string `json:"nodeId"`
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`       // manager | worker
	Leader       bool   `json:"leader"`     // manager-only: swarm Raft leader
	State        string `json:"state"`      // node Status.State
	SwarmManager bool   `json:"swarmManager"` // this daemon runs swarm control plane
	Addr         string `json:"addr,omitempty"`
}

// Self returns the local node's identity and swarm role.
func (o *Orchestrator) Self(ctx context.Context) (SelfInfo, error) {
	info, err := o.cli.Info(ctx)
	if err != nil {
		return SelfInfo{}, err
	}
	si := SelfInfo{
		NodeID:       info.Swarm.NodeID,
		SwarmManager: info.Swarm.ControlAvailable,
	}
	if info.Swarm.NodeID != "" {
		node, err := o.cli.SelfNode(ctx)
		if err == nil {
			si.Hostname = node.Description.Hostname
			si.Role = node.Spec.Role
			si.State = node.Status.State
			si.Addr = node.Status.Addr
			if node.ManagerStatus != nil {
				si.Leader = node.ManagerStatus.Leader
			}
		}
	}
	return si, nil
}

// ensureSwarmManager guards swarm control-plane operations: only a daemon with
// ControlAvailable (a manager) can create/update/scale services. Worker nodes
// return a clear error instead of the engine's cryptic one.
func (o *Orchestrator) ensureSwarmManager(ctx context.Context) error {
	info, err := o.cli.Info(ctx)
	if err != nil {
		return fmt.Errorf("engine info: %w", err)
	}
	if !info.Swarm.ControlAvailable {
		return fmt.Errorf("this node is not a swarm manager (state %q); control-plane operations require a manager", info.Swarm.LocalNodeState)
	}
	return nil
}

// Deploy translates a config into a service spec, pulls the image if needed,
// creates the service, and starts background readiness polling. Returns a
// pending Operation.
func (o *Orchestrator) Deploy(ctx context.Context, cfg *config.Config) (*Operation, error) {
	if err := o.ensureSwarmManager(ctx); err != nil {
		return nil, err
	}
	spec, err := Translate(cfg)
	if err != nil {
		return nil, fmt.Errorf("translate: %w", err)
	}
	auth, err := o.resolveRegistryAuth(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("registry auth: %w", err)
	}
	if err := o.pullIfNeeded(ctx, cfg, auth); err != nil {
		return nil, fmt.Errorf("image pull: %w", err)
	}
	id, err := o.cli.ServiceCreate(ctx, spec, auth)
	if err != nil {
		return nil, fmt.Errorf("service create: %w", err)
	}
	op := newOperation(OpCreate, cfg.Service.Name)
	op.ServiceID = id
	op.Replicas = replicasOf(cfg)
	op.Mode = cfg.Service.Mode
	op.AppendStep(fmt.Sprintf("service created (id=%s, replicas=%d)", id, op.Replicas))
	o.store.Put(op)
	go o.pollReadiness(op, id, configHasHealth(cfg))
	o.registerMonitor(cfg)
	return op, nil
}

// Update replaces a service's spec with a new config and polls convergence.
func (o *Orchestrator) Update(ctx context.Context, name string, cfg *config.Config) (*Operation, error) {
	if err := o.ensureSwarmManager(ctx); err != nil {
		return nil, err
	}
	spec, err := Translate(cfg)
	if err != nil {
		return nil, fmt.Errorf("translate: %w", err)
	}
	auth, err := o.resolveRegistryAuth(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("registry auth: %w", err)
	}
	if err := o.pullIfNeeded(ctx, cfg, auth); err != nil {
		return nil, fmt.Errorf("image pull: %w", err)
	}
	if err := o.cli.ServiceUpdate(ctx, name, spec, auth); err != nil {
		return nil, fmt.Errorf("service update: %w", err)
	}
	svc, err := o.cli.GetService(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("service inspect after update: %w", err)
	}
	op := newOperation(OpUpdate, name)
	op.ServiceID = svc.ID
	op.Replicas = replicasOf(cfg)
	op.Mode = cfg.Service.Mode
	op.AppendStep(fmt.Sprintf("service updated (id=%s)", svc.ID))
	o.store.Put(op)
	go o.pollReadiness(op, svc.ID, configHasHealth(cfg))
	o.registerMonitor(cfg)
	return op, nil
}

// Scale adjusts the replica count and polls convergence.
func (o *Orchestrator) Scale(ctx context.Context, name string, replicas uint64) (*Operation, error) {
	if err := o.ensureSwarmManager(ctx); err != nil {
		return nil, err
	}
	if err := o.cli.ServiceScale(ctx, name, replicas); err != nil {
		return nil, fmt.Errorf("service scale: %w", err)
	}
	svc, err := o.cli.GetService(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("service inspect after scale: %w", err)
	}
	op := newOperation(OpScale, name)
	op.ServiceID = svc.ID
	op.Replicas = replicas
	op.Mode = modeString(svc.Spec.Mode)
	op.AppendStep(fmt.Sprintf("scaled to %d replicas", replicas))
	o.store.Put(op)
	go o.pollReadiness(op, svc.ID, serviceHasHealth(svc))
	return op, nil
}

// Restart forces swarm to re-create the service's tasks (docker service
// update --force equivalent) and polls convergence.
func (o *Orchestrator) Restart(ctx context.Context, name string) (*Operation, error) {
	if err := o.ensureSwarmManager(ctx); err != nil {
		return nil, err
	}
	if err := o.cli.ServiceRestart(ctx, name); err != nil {
		return nil, fmt.Errorf("service restart: %w", err)
	}
	svc, err := o.cli.GetService(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("service inspect after restart: %w", err)
	}
	op := newOperation(OpRestart, name)
	op.ServiceID = svc.ID
	op.AppendStep(fmt.Sprintf("force-updated (id=%s)", svc.ID))
	o.store.Put(op)
	go o.pollReadiness(op, svc.ID, serviceHasHealth(svc))
	return op, nil
}

// Remove deletes a service. Returns a done Operation (no convergence to poll).
func (o *Orchestrator) Remove(ctx context.Context, name string) (*Operation, error) {
	if err := o.ensureSwarmManager(ctx); err != nil {
		return nil, err
	}
	if err := o.cli.ServiceRemove(ctx, name); err != nil {
		return nil, fmt.Errorf("service remove: %w", err)
	}
	op := newOperation(OpRemove, name)
	op.AppendStep("service removed")
	op.SetStatus(OpStatusDone, "")
	o.store.Put(op)
	if o.mon != nil {
		o.mon.Unregister(name)
	}
	return op, nil
}

// ListServices returns services, optionally filtered by label.
func (o *Orchestrator) ListServices(ctx context.Context, label string) ([]docker.Service, error) {
	f := docker.Filter{}
	if label != "" {
		f["label"] = []string{label}
	}
	return o.cli.ListServices(ctx, f)
}

// ServiceDetail is the GET /services/{name} response: service + tasks + health.
type ServiceDetail struct {
	Service docker.Service  `json:"service"`
	Tasks   []docker.Task   `json:"tasks"`
	Running int             `json:"running"`
	Desired int             `json:"desired"`
	Healthy int             `json:"healthy"`
}

// Inspect returns a service detail with task counts and health.
func (o *Orchestrator) Inspect(ctx context.Context, name string) (ServiceDetail, error) {
	svc, err := o.cli.GetService(ctx, name)
	if err != nil {
		return ServiceDetail{}, err
	}
	tasks, err := o.cli.ServiceTasks(ctx, svc.ID)
	if err != nil {
		return ServiceDetail{}, err
	}
	d := ServiceDetail{Service: svc, Tasks: tasks}
	for _, t := range tasks {
		if t.DesiredState == "running" {
			d.Desired++
			if t.Status.State == "running" {
				d.Running++
				if !serviceHasHealth(svc) {
					d.Healthy++
				} else if cid := t.Status.ContainerStatus.ContainerID; cid != "" {
					if ci, err := o.cli.ContainerInspect(ctx, cid); err == nil && ci.State.Health != nil && ci.State.Health.Status == "healthy" {
						d.Healthy++
					}
				}
			}
		}
	}
	return d, nil
}

// GetOperation returns an operation snapshot.
func (o *Orchestrator) GetOperation(id string) (Operation, bool) {
	return o.store.Get(id)
}

// ---- readiness polling ----

type readyState struct {
	running, desired, healthy int
	taskErrors               []string
}

func (o *Orchestrator) computeReady(ctx context.Context, serviceID string, hasHealth bool) (readyState, error) {
	tasks, err := o.cli.ServiceTasks(ctx, serviceID)
	if err != nil {
		return readyState{}, err
	}
	rs := readyState{}
	for _, t := range tasks {
		if t.DesiredState == "running" {
			rs.desired++
			if t.Status.State == "running" {
				rs.running++
				if hasHealth {
					if cid := t.Status.ContainerStatus.ContainerID; cid != "" {
						if ci, err := o.cli.ContainerInspect(ctx, cid); err == nil && ci.State.Health != nil && ci.State.Health.Status == "healthy" {
							rs.healthy++
						}
					}
				} else {
					rs.healthy++
				}
			}
		}
		if t.Status.Err != "" {
			rs.taskErrors = append(rs.taskErrors, fmt.Sprintf("task %s state=%s err=%s", t.ID, t.Status.State, t.Status.Err))
		}
	}
	return rs, nil
}

// pollReadiness polls the service until all replicas are running (and healthy
// if a healthcheck is configured), the deadline expires, or the context is
// cancelled. It mutates op's status/steps.
func (o *Orchestrator) pollReadiness(op *Operation, serviceID string, hasHealth bool) {
	ctx, cancel := context.WithTimeout(context.Background(), o.readyTimeout)
	defer cancel()

	backoff := time.Second
	for {
		rs, err := o.computeReady(ctx, serviceID, hasHealth)
		if err != nil {
			op.AppendStep(fmt.Sprintf("readiness check error: %v", err))
		} else {
			op.AppendStep(fmt.Sprintf("running=%d desired=%d healthy=%d", rs.running, rs.desired, rs.healthy))
			for _, e := range rs.taskErrors {
				op.AppendStep(e)
			}
			if rs.desired > 0 && rs.running == rs.desired && rs.healthy == rs.running {
				op.SetStatus(OpStatusHealthy, "")
				return
			}
		}

		select {
		case <-ctx.Done():
			if rs.running > 0 {
				op.SetStatus(OpStatusPartial, fmt.Sprintf("convergence timeout: %d/%d running", rs.running, rs.desired))
			} else {
				op.SetStatus(OpStatusFailed, "convergence timeout: no replicas running")
			}
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff = backoff * 3 / 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// ---- helpers ----

func replicasOf(cfg *config.Config) uint64 {
	if cfg.Service.Replicas != nil {
		return *cfg.Service.Replicas
	}
	return 1
}

// resolveRegistryAuth computes the X-Registry-Auth header value (base64 of a
// ~/.docker/config.json) from the config. Inline takes precedence; otherwise a
// swarm secret's Data (itself base64) is decoded and re-encoded.
func (o *Orchestrator) resolveRegistryAuth(ctx context.Context, cfg *config.Config) (string, error) {
	ra := cfg.Service.RegistryAuth
	if ra == nil {
		return "", nil
	}
	if ra.Inline != "" {
		return ra.Inline, nil
	}
	if ra.SecretRef != "" {
		sec, err := o.cli.GetSecret(ctx, ra.SecretRef)
		if err != nil {
			return "", fmt.Errorf("secret %q: %w", ra.SecretRef, err)
		}
		data, err := base64.StdEncoding.DecodeString(sec.Spec.Data)
		if err != nil {
			return "", fmt.Errorf("secret %q is not valid base64: %w", ra.SecretRef, err)
		}
		return base64.StdEncoding.EncodeToString(data), nil
	}
	return "", nil
}

// pullIfNeeded pulls the image explicitly when the policy is "always" or when
// private registry auth is configured (so the manager has the image before
// tasks start; also surfaces pull errors synchronously).
func (o *Orchestrator) pullIfNeeded(ctx context.Context, cfg *config.Config, auth string) error {
	if cfg.Service.ImagePullPolicy != "always" && auth == "" {
		return nil
	}
	o.log.Info("pulling image", "image", cfg.Service.Image)
	return o.cli.ImagePull(ctx, cfg.Service.Image, auth)
}

// registerMonitor wires the config's monitoring block into the P2 monitor.
func (o *Orchestrator) registerMonitor(cfg *config.Config) {
	if o.mon != nil {
		o.mon.Register(cfg.Service.Name, &cfg.Monitoring)
	}
}

func configHasHealth(cfg *config.Config) bool {
	hc := cfg.Service.Healthcheck
	return hc != nil && len(hc.Test) > 0
}

func serviceHasHealth(svc docker.Service) bool {
	hc := svc.Spec.TaskTemplate.ContainerSpec.Healthcheck
	return hc != nil && len(hc.Test) > 0
}

// modeString reduces a ServiceMode struct to "replicated" | "global" | "".
func modeString(m docker.ServiceMode) string {
	switch {
	case m.Replicated != nil:
		return "replicated"
	case m.Global != nil:
		return "global"
	}
	return ""
}
