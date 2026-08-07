package monitor

import (
	"context"
	"log/slog"
	"sync"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// Manager owns all active monitor jobs, one per service. It implements
// orchestrator.MonitorRegistrar so Deploy/Update/Remove wire monitoring in and
// out automatically.
type Manager struct {
	cli   docker.Client
	store *EventStore
	log   *slog.Logger

	// restart triggers a service restart (logCheck action=restart). Set by the
	// orchestrator wiring; may be nil.
	restartFn func(ctx context.Context, service string) error

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	active  map[string]struct{}
}

// NewManager creates a monitor manager.
func NewManager(cli docker.Client, store *EventStore, log *slog.Logger) *Manager {
	return &Manager{
		cli:     cli,
		store:   store,
		log:     log,
		cancels: map[string]context.CancelFunc{},
		active:  map[string]struct{}{},
	}
}

// SetRestartFn wires the restart callback (called when a check's action is
// "restart").
func (m *Manager) SetRestartFn(fn func(ctx context.Context, service string) error) {
	m.restartFn = fn
}

// Register starts (or reconfigures) the monitor jobs for a service from its
// monitoring config.
func (m *Manager) Register(service string, mc *config.Monitoring) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// stop existing jobs for this service
	if cancel, ok := m.cancels[service]; ok {
		cancel()
		delete(m.cancels, service)
	}

	if mc == nil || !mc.Enabled {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[service] = cancel
	m.active[service] = struct{}{}

	start := func(run func(ctx context.Context)) {
		go run(ctx)
	}

	for _, pc := range mc.PortChecks {
		pc := pc
		if chk, err := newPortCheck(service, pc, m.cli, m.store, m.log); err == nil {
			start(chk.run)
		}
	}
	for _, hc := range mc.HTTPChecks {
		hc := hc
		if chk, err := newHTTPCheck(service, hc, m.cli, m.store, m.log); err == nil {
			start(chk.run)
		}
	}
	for _, lc := range mc.LogChecks {
		lc := lc
		chk := newLogCheck(service, lc, m.cli, m.store, m.log)
		start(chk.run)
	}
	if len(mc.ResourceThresholds) > 0 {
		if chk, err := newResCheck(service, mc.ResourceThresholds, m.cli, m.store, m.log); err == nil {
			start(chk.run)
		}
	}

	m.log.Info("monitoring registered",
		"service", service,
		"ports", len(mc.PortChecks),
		"http", len(mc.HTTPChecks),
		"logs", len(mc.LogChecks),
		"resources", len(mc.ResourceThresholds),
	)
}

// Unregister stops all monitor jobs for a service.
func (m *Manager) Unregister(service string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.cancels[service]; ok {
		cancel()
		delete(m.cancels, service)
	}
	delete(m.active, service)
	m.log.Info("monitoring unregistered", "service", service)
}

// Events returns events from the store, filtered. after > 0 时仅返回
// seq < after 的更早一页（倒序分页游标）。
func (m *Manager) Events(service string, typ EventType, after int64, limit int) []Event {
	return m.store.List(service, typ, after, limit)
}
