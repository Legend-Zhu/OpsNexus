package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// portCheck probes TCP connectivity to a service's published port on the node
// IPs running its tasks (host mode) or any node (ingress mode via routing
// mesh). It reports port_down after `retries` consecutive failures.
type portCheck struct {
	service  string
	port     string
	protocol string
	interval time.Duration
	timeout  time.Duration
	retries  int

	cli   docker.Client
	store *EventStore
	log   *slog.Logger
}

func newPortCheck(service string, pc config.PortCheck, cli docker.Client, store *EventStore, log *slog.Logger) (*portCheck, error) {
	interval, err := time.ParseDuration(pc.Interval)
	if err != nil {
		return nil, fmt.Errorf("portCheck interval: %w", err)
	}
	timeout, err := time.ParseDuration(pc.Timeout)
	if err != nil {
		return nil, fmt.Errorf("portCheck timeout: %w", err)
	}
	if pc.Retries <= 0 {
		pc.Retries = 2
	}
	if pc.Protocol == "" {
		pc.Protocol = "tcp"
	}
	return &portCheck{
		service:  service,
		port:     pc.Port,
		protocol: pc.Protocol,
		interval: interval,
		timeout:  timeout,
		retries:  pc.Retries,
		cli:      cli,
		store:    store,
		log:      log,
	}, nil
}

func (p *portCheck) run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, detail := p.probe(ctx)
			if ok {
				fails = 0
				continue
			}
			fails++
			if fails >= p.retries {
				ev := Event{
					Service: p.service,
					Type:    EventPortDown,
					Level:   LevelError,
					Msg:     fmt.Sprintf("port %s/%s unreachable", p.protocol, p.port),
					Detail:  detail,
				}
				p.store.Add(ev)
				p.log.Error("port check failed", "service", p.service, "detail", detail)
				fails = 0 // re-arm: emit again after retries consecutive failures
			}
		}
	}
}

// probe dials the port on every node currently running a task of the service.
// In ingress mode the routing mesh makes any node answer; probing task nodes
// covers both modes. Returns (reachable, detail).
func (p *portCheck) probe(ctx context.Context) (bool, string) {
	addrs, err := serviceNodeAddrs(ctx, p.cli, p.service)
	if err != nil {
		return false, err.Error()
	}
	if len(addrs) == 0 {
		return false, "no nodes running tasks"
	}
	allOK := true
	for _, a := range addrs {
		addr := net.JoinHostPort(a, p.port)
		conn, err := net.DialTimeout(p.protocol, addr, p.timeout)
		if err != nil {
			allOK = false
			return false, fmt.Sprintf("dial %s: %v", addr, err)
		}
		conn.Close()
	}
	return allOK, ""
}

// serviceNodeAddrs resolves the node IPs of tasks in a running state for a
// service (used by portcheck and httpcheck to pick a probe target). The
// routing mesh (ingress mode) makes any node answer; in host mode the task
// nodes are the only ones with the port open.
func serviceNodeAddrs(ctx context.Context, cli docker.Client, service string) ([]string, error) {
	svc, err := cli.GetService(ctx, service)
	if err != nil {
		return nil, err
	}
	tasks, err := cli.ServiceTasks(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	nodeIDs := map[string]struct{}{}
	for _, t := range tasks {
		if t.Status.State == "running" && t.NodeID != "" {
			nodeIDs[t.NodeID] = struct{}{}
		}
	}
	nodes, err := cli.ListNodes(ctx, nil)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, n := range nodes {
		if _, ok := nodeIDs[n.ID]; ok && n.Status.Addr != "" {
			addrs = append(addrs, n.Status.Addr)
		}
	}
	return addrs, nil
}
