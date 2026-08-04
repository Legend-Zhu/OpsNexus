// Command server is the OpsGaurdWeb management-plane API server.
// It manages multiple clusters (via Worker agents) and integrates the
// AiNexus AI troubleshooting service. Skeleton: routes are registered,
// handlers return placeholders.
package main

import (
	"flag"
	"log/slog"
	"os"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/router"
)

func main() {
	var (
		cfgPath string
		addr    string
	)
	flag.StringVar(&cfgPath, "config", "configs/config.yaml", "path to server config")
	flag.StringVar(&addr, "addr", "", "listen address (overrides config)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("config load failed", "path", cfgPath, "err", err)
		os.Exit(1)
	}
	if addr != "" {
		cfg.Server.Addr = addr
	}
	log.Info("server starting",
		"addr", cfg.Server.Addr,
		"ainexus_enabled", cfg.AINexus.Enabled,
		"ainexus", cfg.AINexus.BaseURL,
		"clusters", len(cfg.Clusters),
	)

	h := api.NewHandlers()
	r := router.New(h)

	if err := r.Run(cfg.Server.Addr); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
