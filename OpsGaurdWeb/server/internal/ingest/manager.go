// SubscriberManager owns one event Subscriber per registered cluster. It wires
// cluster lifecycle (add/remove) to subscription start/stop so new clusters
// begin draining events and removed clusters stop. One manager per server
// process; started from cmd/server/main.go alongside patrol.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// WorkerClientBuilder returns a fresh event subscriber handle for a cluster
// (the workerproxy.Client implements this via SubscribeEvents). The returned
// closeFn releases the underlying gRPC stream/client resources.
type WorkerClientBuilder func(ctx context.Context, cluster string, afterSeq int64) (sub EventSubscriber, closeFn func(), err error)

// Manager starts/stops per-cluster event subscribers.
type Manager struct {
	build WorkerClientBuilder
	svc   *Service
	st    *store.Store
	log   *slog.Logger

	mu      sync.Mutex
	subs    map[string]context.CancelFunc // cluster -> cancel
	wg      sync.WaitGroup
	rootCtx context.Context
}

// NewManager creates a subscriber manager. Call Start once, then Add/Remove as
// clusters come and go, and Stop on shutdown.
func NewManager(build WorkerClientBuilder, svc *Service, st *store.Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{build: build, svc: svc, st: st, log: log, subs: map[string]context.CancelFunc{}}
}

// Start seeds subscribers for all currently-registered clusters. It launches a
// background context whose lifetime is tied to Stop.
func (m *Manager) Start(ctx context.Context) error {
	m.rootCtx = ctx
	clusters, err := m.st.ListClusters()
	if err != nil {
		return err
	}
	for _, c := range clusters {
		m.Add(c.Name)
	}
	return nil
}

// Add starts a subscriber for a cluster (no-op if already running). Idempotent
// — safe to call on every cluster list refresh.
func (m *Manager) Add(cluster string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[cluster]; ok {
		return
	}
	if m.rootCtx == nil {
		return // not started yet
	}
	ctx, cancel := context.WithCancel(m.rootCtx)
	m.subs[cluster] = cancel

	// The factory adapts the WorkerClientBuilder (which needs a cursor + ctx
	// per stream open) into the Subscriber's ClusterClientFactory (which opens
	// a stream per reconnect attempt). Each attempt loads the latest cursor.
	factory := func(cluster string) (EventSubscriber, func(), error) {
		cursor, err := m.st.GetCursor(cluster)
		if err != nil {
			cursor = 0
		}
		return m.build(ctx, cluster, cursor)
	}

	sub := NewSubscriber(cluster, factory, m.svc, m.st, m.log)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		sub.Run(ctx)
		m.log.Info("event subscriber stopped", "cluster", cluster)
	}()
	m.log.Info("event subscriber started", "cluster", cluster)
}

// Remove stops a cluster's subscriber and deletes its cursor.
func (m *Manager) Remove(cluster string) {
	m.mu.Lock()
	cancel, ok := m.subs[cluster]
	if ok {
		delete(m.subs, cluster)
	}
	m.mu.Unlock()
	if ok {
		cancel()
	}
	_ = m.st.DeleteCursor(cluster)
	m.log.Info("event subscriber removed", "cluster", cluster)
}

// Stop cancels all subscribers and waits for them to drain.
func (m *Manager) Stop() {
	m.mu.Lock()
	for _, cancel := range m.subs {
		cancel()
	}
	m.subs = map[string]context.CancelFunc{}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		m.log.Warn("subscriber manager stop timed out")
	}
}
