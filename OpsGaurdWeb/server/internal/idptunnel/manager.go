// Package idptunnel owns the per-cluster reverse IdP tunnels. For each
// registered cluster it opens (and reopens on disconnect) the server-initiated
// Tunnel bidi stream to that cluster's Worker, proxying the Worker's forwarded
// IdP HTTP requests to this management server's local IdP.
//
// This mirrors ingest.Manager's lifecycle (Start seeds all clusters; Add/Remove
// follow cluster registry changes; Stop drains) but is simpler — there is no
// persistent cursor; a dropped tunnel simply loses in-flight frames (callers
// time out and retry) and the stream reopens cleanly.
package idptunnel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// Manager opens one reverse IdP tunnel per registered cluster.
type Manager struct {
	clusterSvc *cluster.Service
	st         *store.Store
	localBase  string // loopback base of the management server's IdP, e.g. http://127.0.0.1:8080
	log        *slog.Logger

	mu      sync.Mutex
	tunnels map[string]context.CancelFunc // cluster -> cancel
	wg      sync.WaitGroup
	rootCtx context.Context
}

// NewManager creates an IdP tunnel manager. localBase is the loopback base the
// management server uses to reach its own IdP (same process/port).
func NewManager(clusterSvc *cluster.Service, st *store.Store, localBase string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		clusterSvc: clusterSvc,
		st:         st,
		localBase:  localBase,
		log:        log,
		tunnels:    map[string]context.CancelFunc{},
	}
}

// Start opens tunnels for all currently-registered clusters.
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

// Add opens a tunnel for a cluster (no-op if already running).
func (m *Manager) Add(cluster string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tunnels[cluster]; ok {
		return
	}
	if m.rootCtx == nil {
		return
	}
	ctx, cancel := context.WithCancel(m.rootCtx)
	m.tunnels[cluster] = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runWithReconnect(ctx, cluster)
		m.log.Info("idp tunnel stopped", "cluster", cluster)
	}()
	m.log.Info("idp tunnel started", "cluster", cluster)
}

// Remove closes a cluster's tunnel.
func (m *Manager) Remove(cluster string) {
	m.mu.Lock()
	cancel, ok := m.tunnels[cluster]
	if ok {
		delete(m.tunnels, cluster)
	}
	m.mu.Unlock()
	if ok {
		cancel()
	}
	m.log.Info("idp tunnel removed", "cluster", cluster)
}

// Stop cancels all tunnels and waits for them to drain.
func (m *Manager) Stop() {
	m.mu.Lock()
	for _, cancel := range m.tunnels {
		cancel()
	}
	m.tunnels = map[string]context.CancelFunc{}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		m.log.Warn("idp tunnel manager stop timed out")
	}
}

// runWithReconnect opens the tunnel stream and reopens it with exponential
// backoff when it ends (transport error, Worker restart, leader change). A
// tunnel that cannot be opened (e.g. Worker unreachable) simply keeps retrying
// rather than killing the manager.
func (m *Manager) runWithReconnect(ctx context.Context, cluster string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		cli, err := m.clusterSvc.WorkerClient(cluster)
		if err != nil {
			m.log.Warn("idp tunnel: worker client unavailable, retrying", "cluster", cluster, "err", err)
		} else {
			err = cli.ServeTunnel(ctx, m.localBase, m.log)
			cli.Close()
		}
		if ctx.Err() != nil {
			return
		}
		m.log.Info("idp tunnel stream ended, reconnecting", "cluster", cluster, "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
