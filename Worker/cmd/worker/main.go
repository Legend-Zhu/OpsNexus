// Command worker is the OpsGaurd node agent. P1 adds the HTTP orchestration
// API (POST /api/v1/services etc.) over the Docker Swarm engine. Monitoring
// (P2) and MCP (P3) land in later phases.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/logging"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/mcp"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/nodeagent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/version"
)

func main() {
	var (
		addr       string
		cfgPath    string
		agentCfg   string
		logJSON    bool
		logLevel   string
		stdio      bool // MCP over stdio instead of HTTP
		tlsCert    string
		tlsKey     string
	)
	flag.StringVar(&addr, "addr", ":8080", "HTTP listen address for the orchestration API")
	flag.StringVar(&cfgPath, "config", "", "optional worker config (yaml/json) to validate at startup")
	flag.StringVar(&agentCfg, "agent-config", "", "path to the agent's own config (command policy, role)")
	flag.BoolVar(&logJSON, "log-json", false, "emit JSON logs instead of text")
	flag.StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.BoolVar(&stdio, "mcp-stdio", false, "serve MCP over stdio (local agents) instead of the HTTP API")
	flag.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file (PEM); enables HTTPS when set with -tls-key")
	flag.StringVar(&tlsKey, "tls-key", "", "TLS private key file (PEM)")
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
		api        *orchestrator.API
		mcpHandler *mcp.Handler
	)
	if isManager {
		store := orchestrator.NewOperationStore(1000)
		orch = orchestrator.New(cli, store, log)

		// P2 monitoring: wire the manager in; log/resource checks with
		// action=restart call back into the orchestrator.
		evStore := monitor.NewEventStore(10000)
		monMgr = monitor.NewManager(cli, evStore, log)
		monMgr.SetRestartFn(func(ctx context.Context, svc string) error {
			_, err := orch.Restart(ctx, svc)
			return err
		})
		orch.SetMonitor(monMgr)

		// Webhook event forwarding (agent config `webhooks` list).
		if len(agCfg.Webhooks) > 0 {
			pusher := monitor.NewWebhookPusher(agCfg.Webhooks, log)
			evStore.AddSink(pusher.Sink())
			log.Info("webhook forwarding enabled", "urls", len(agCfg.Webhooks))
		}

		api = orchestrator.NewAPI(orch)
		api.SetEvents(monMgr.Events)

		// P3 MCP: expose the same capabilities to LLM agents over Streamable HTTP.
		mcpHandler, err = mcp.New(orch, monMgr, cli, log)
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
		for pattern, handler := range api.Routes() {
			mux.HandleFunc(pattern, handler)
		}
		// MCP Streamable HTTP endpoint (manager only).
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

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
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

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigc
	log.Info("shutting down", "signal", sig.String())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown error", "err", err)
	}
	log.Info("stopped")
}
