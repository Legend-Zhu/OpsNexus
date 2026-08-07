// Command worker is the OpsGaurd node agent. P1 adds the HTTP orchestration
// API (POST /api/v1/services etc.) over the Docker Swarm engine. Monitoring
// (P2) and MCP (P3) land in later phases.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"os/signal"
	"syscall"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/authz"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/idpproxy"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/logging"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/mcp"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/nodeagent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/version"

	"google.golang.org/grpc"
)

func main() {
	var (
		addr     string
		grpcAddr string
		dataDir  string
		cfgPath  string
		agentCfg string
		logJSON  bool
		logLevel string
		stdio    bool // MCP over stdio instead of HTTP
		tlsCert  string
		tlsKey   string
		tlsCA    string
	)
	flag.StringVar(&addr, "addr", ":8080", "HTTP listen address for the orchestration API")
	flag.StringVar(&grpcAddr, "grpc-addr", ":9080", "gRPC listen address for the management API (server↔worker)")
	flag.StringVar(&dataDir, "data-dir", "/var/lib/opsguard", "directory for persistent state (event/audit SQLite queues)")
	flag.StringVar(&cfgPath, "config", "", "optional worker config (yaml/json) to validate at startup")
	flag.StringVar(&agentCfg, "agent-config", "", "path to the agent's own config (command policy, role)")
	flag.BoolVar(&logJSON, "log-json", false, "emit JSON logs instead of text")
	flag.StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.BoolVar(&stdio, "mcp-stdio", false, "serve MCP over stdio (local agents) instead of the HTTP API")
	flag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file (PEM); enables HTTPS when set with -tls-key")
	flag.StringVar(&tlsKey, "tls-key", "", "TLS private key file (PEM)")
	flag.StringVar(&tlsCA, "tls-ca", "", "CA file (PEM) to verify client certificates; enables mutual TLS (mTLS) when set with -tls-cert/-tls-key")
	flag.Parse()

	var level slog.Level
	switch logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	// Logs are redacted: attribute values under sensitive keys (env, auth,
	// secret, password, token, ...) never reach the output.
	var base slog.Handler
	if logJSON {
		base = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		base = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	log := slog.New(logging.NewRedactHandler(base))
	log.Info("worker starting", "version", version.Version, "addr", addr, "log_level", logLevel)

	if cfgPath != "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			log.Error("startup config invalid", "path", cfgPath, "err", err)
			os.Exit(1)
		}
		log.Info("startup config valid", "service", cfg.Service.Name, "image", cfg.Service.Image)
	}

	// Agent's own config: command-execution policy (host/container) and role.
	agCfg := agent.DefaultConfig()
	if agentCfg != "" {
		var err error
		agCfg, err = agent.Load(agentCfg)
		if err != nil {
			log.Error("agent config invalid", "path", agentCfg, "err", err)
			os.Exit(1)
		}
	}
	// Agent config can override the flag defaults for the gRPC listen address
	// and data dir (flags, when explicitly set on the command line, still win).
	// This lets deployments pin these in the mounted config file.
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if agCfg.Worker.GrpcListen != "" && !setFlags["grpc-addr"] {
		grpcAddr = agCfg.Worker.GrpcListen
	}
	if agCfg.Worker.DataDir != "" && !setFlags["data-dir"] {
		dataDir = agCfg.Worker.DataDir
	}
	log.Info("agent policy",
		"mode", agCfg.CommandPolicy.Mode,
		"allow_host_exec", agCfg.CommandPolicy.AllowHostExec,
		"allow_container_exec", agCfg.CommandPolicy.AllowContainerExec,
		"blacklist_entries", len(agCfg.CommandPolicy.Blacklist),
	)

	cli, err := docker.New()
	if err != nil {
		log.Error("docker client init failed", "err", err)
		os.Exit(1)
	}
	defer cli.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := cli.Ping(pingCtx); err != nil {
		log.Warn("docker daemon unreachable at startup (continuing)", "err", err)
	} else if v, vErr := cli.ServerVersion(pingCtx); vErr == nil {
		log.Info("connected to docker", "engine", v)
	}
	pingCancel()

	// Role: auto-derive from the swarm node type, or pin via agent config.
	role := agent.Role(agCfg.Worker.Role)
	if role == agent.RoleAuto {
		if n, nErr := cli.SelfNode(context.Background()); nErr == nil && n.Spec.Role == "manager" {
			role = agent.RoleManager
		} else {
			role = agent.RoleNode
		}
	}
	isManager := role == agent.RoleManager
	log.Info("worker role", "role", role)

	// Local node API: every worker (manager or node role) serves its node's
	// stats/exec/host endpoints so the manager can proxy cross-node calls.
	localAPI := nodeagent.New(cli, &agCfg.CommandPolicy, log)

	var (
		orch       *orchestrator.Orchestrator
		monMgr     *monitor.Manager
		mcpHandler *mcp.Handler
		evStore    *monitor.EventStore
		auditStore *audit.Store
	)
	if isManager {
		store := orchestrator.NewOperationStore(1000)
		orch = orchestrator.New(cli, store, log)
		// Cross-node proxying targets each worker's HTTP port; all workers
		// share the same listen port, so reuse this instance's -addr port.
		if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
			orch.SetWorkerPort(p)
		}
		// Forward a bearer token when proxying to other nodes' local HTTP API
		// (processes/stats/exec/logs-host). Every worker shares the same token
		// set, so any configured secret authenticates; pick the first one.
		if agCfg.Auth.Enabled && len(agCfg.Auth.Tokens) > 0 {
			for _, secret := range agCfg.Auth.Tokens {
				orch.SetAuthToken(secret)
				break
			}
		}
		// Leader write-forwarding targets the management gRPC port; every
		// worker shares the same gRPC port.
		if _, p, err := net.SplitHostPort(grpcAddr); err == nil && p != "" {
			orch.SetGRPCPort(p)
		}

		// Persistent state directory (event/audit SQLite queues). The manager
		// is the only role that runs the control plane, so it owns these.
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Error("data-dir create failed", "path", dataDir, "err", err)
			os.Exit(1)
		}

		// Audit log: record every lifecycle action + command execution. The
		// SQLite store is durable (survives restarts) and streamable over gRPC
		// (SubscribeAudit), replacing the former webhook push.
		auditStore, err = audit.NewStore(filepath.Join(dataDir, "audit.db"), log)
		if err != nil {
			log.Error("audit store open failed", "err", err)
			os.Exit(1)
		}
		defer auditStore.Close()
		orch.SetAudit(auditStore)

		// P2 monitoring: wire the manager in; log/resource checks with
		// action=restart call back into the orchestrator.
		evStore, err = monitor.NewEventStore(filepath.Join(dataDir, "events.db"), log)
		if err != nil {
			log.Error("event store open failed", "err", err)
			os.Exit(1)
		}
		defer evStore.Close()
		monMgr = monitor.NewManager(cli, evStore, log)
		monMgr.SetRestartFn(func(ctx context.Context, svc string) error {
			_, err := orch.Restart(ctx, svc)
			return err
		})
		orch.SetMonitor(monMgr)

		// P3 MCP: expose the same capabilities to LLM agents over Streamable HTTP.
		mcpHandler, err = mcp.NewWithAudit(orch, monMgr, cli, log, auditStore)
		if err != nil {
			log.Error("mcp init failed", "err", err)
			os.Exit(1)
		}
	}

	if stdio {
		if !isManager {
			log.Error("mcp-stdio requires the manager role")
			os.Exit(1)
		}
		// Local mode: MCP over stdin/stdout (protocol 2026-07-28). Logs must
		// not pollute stdout, which carries the JSON-RPC frames.
		log.Info("serving MCP over stdio")
		if err := mcpHandler.ServeStdio(context.Background()); err != nil {
			log.Error("mcp stdio error", "err", err)
			os.Exit(1)
		}
		return
	}

	mux := http.NewServeMux()
	if isManager {
		// MCP Streamable HTTP endpoint (manager only). The management API now
		// lives entirely on gRPC (:9080); HTTP serves only /mcp + /healthz +
		// the node-level local API.
		mux.Handle("/mcp", mcpHandler.HTTPHandler())
	}
	// Local node endpoints are served by every worker instance.
	for pattern, handler := range localAPI.Routes() {
		mux.HandleFunc(pattern, handler)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Auth: bearer-token middleware wraps the whole API (incl. /mcp and the
	// local node endpoints, which can execute host commands). OAuth 2.1
	// protected-resource metadata, /healthz and the /idp-proxy/ tunnel entry are
	// public for discovery and in-cluster IdP access.
	authzMW := authz.New(&agCfg.Auth)
	authzMW.PublicPaths = []string{
		"/.well-known/oauth-protected-resource",
		"/healthz",
		"/idp-proxy/", // reverse IdP tunnel: in-cluster services (r-nacos) reach IdP via here, no bearer
	}
	if agCfg.Auth.Enabled {
		log.Info("auth enabled", "tokens", len(agCfg.Auth.Tokens))
		mux.Handle("GET /.well-known/oauth-protected-resource", authz.MetadataHandler(authzMW))
	}

	// Reverse IdP tunnel: a manager-role worker owns the bidi Tunnel stream the
	// management server opens. The local /idp-proxy/ HTTP entry forwards
	// in-cluster IdP requests (r-nacos discovery/token/jwks/userinfo) over it,
	// avoiding a reverse firewall hole. Enabled only when a tunnel is reachable
	// (OPSGUARD_TUNNEL_BASE configured) on manager-role workers.
	var tunnelMgr *grpcapi.TunnelManager
	if isManager {
		tunnelBase := os.Getenv("OPSGUARD_TUNNEL_BASE") // e.g. http://10.60.171.232:8080
		if tunnelBase != "" {
			tunnelMgr = grpcapi.NewTunnelManager(log)
			mux.Handle("/idp-proxy/", idpproxy.Handler(idpproxy.Config{
				PublicIssuer: os.Getenv("OPSGUARD_IDP_PUBLIC_ISSUER"), // e.g. http://172.28.50.176:8080
				TunnelBase:   tunnelBase,
			}, tunnelMgrAdapter{tunnelMgr}, log))
			log.Info("idp reverse tunnel enabled", "tunnel_base", tunnelBase,
				"public_issuer", os.Getenv("OPSGUARD_IDP_PUBLIC_ISSUER"))
		}
	}

	// Management gRPC server (server↔worker API). Only the manager role serves
	// it; node-role workers expose only the local HTTP API. The gRPC port runs
	// alongside HTTP (:8080 for /mcp + /healthz + local node API; :9080 for the
	// management service). Subscribe* streams let the server pull events back
	// over a server-initiated connection (honoring the one-way network policy).
	var (
		grpcSrv *grpc.Server
		mgmt    *grpcapi.Server
	)
	if isManager {
		grpcSrv = grpc.NewServer(
			grpc.ChainUnaryInterceptor(authzMW.GRPCUnaryInterceptor()),
			grpc.ChainStreamInterceptor(authzMW.GRPCStreamInterceptor()),
		)
		mgmt = grpcapi.New(orch, evStore, auditStore, localAPI, tunnelMgr, log)
		mgmt.Register(grpcSrv)
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      authzMW.Wrap(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// Mutual TLS: when a CA is supplied, require and verify client certs.
	if tlsCert != "" && tlsKey != "" && tlsCA != "" {
		caPEM, err := os.ReadFile(tlsCA)
		if err != nil {
			log.Error("read tls-ca", "err", err)
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			log.Error("no valid certs in tls-ca", "path", tlsCA)
			os.Exit(1)
		}
		srv.TLSConfig = &tls.Config{ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
		log.Info("mutual TLS enabled (client certs required)", "ca", tlsCA)
	}

	go func() {
		if tlsCert != "" && tlsKey != "" {
			log.Info("http server listening (TLS)", "addr", addr)
			if err := srv.ListenAndServeTLS(tlsCert, tlsKey); err != nil && err != http.ErrServerClosed {
				log.Error("http server error", "err", err)
				os.Exit(1)
			}
			return
		}
		log.Info("http server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server error", "err", err)
			os.Exit(1)
		}
	}()

	// gRPC listener (manager only). Runs alongside HTTP.
	if grpcSrv != nil {
		lis, err := net.Listen("tcp", grpcAddr)
		if err != nil {
			log.Error("grpc listen failed", "addr", grpcAddr, "err", err)
			os.Exit(1)
		}
		go func() {
			log.Info("grpc server listening", "addr", grpcAddr)
			if err := grpcSrv.Serve(lis); err != nil {
				log.Error("grpc server error", "err", err)
			}
		}()
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigc
	log.Info("shutting down", "signal", sig.String())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown error", "err", err)
	}
	if grpcSrv != nil {
		grpcSrv.GracefulStop()
	}
	if mgmt != nil {
		_ = mgmt.Close()
	}
	log.Info("stopped")
}

// tunnelMgrAdapter adapts *grpcapi.TunnelManager to idpproxy.TunnelSender by
// delegating to its RoundTripHTTP method. It lets the idpproxy handler stay
// decoupled from the grpcapi/pb types.
type tunnelMgrAdapter struct{ m *grpcapi.TunnelManager }

func (a tunnelMgrAdapter) Available() bool {
	if a.m == nil {
		return false
	}
	return a.m.Available()
}

func (a tunnelMgrAdapter) RoundTrip(method, path string, headers http.Header, body []byte, timeout time.Duration) (int, http.Header, []byte, error) {
	if a.m == nil {
		return 0, nil, nil, fmt.Errorf("idp tunnel disabled")
	}
	return a.m.RoundTripHTTP(method, path, headers, body, timeout)
}
