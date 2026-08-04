// Command server is the OpsGaurdWeb management-plane API server.
// It manages multiple clusters (via Worker agents) and embeds the
// AiNexus AI troubleshooting gateway in-process (vendored under
// internal/ainexus, no standalone service). Skeleton: routes are
// registered, handlers return placeholders.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
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

	h := api.NewHandlers()

	// 内嵌 AiNexus 网关（与管理端同进程，无独立服务/端口）
	if cfg.AINexus.Enabled {
		ainx := ainexusserver.New(&cfg.AINexus)
		if err := ainx.Initialize(context.Background()); err != nil {
			log.Error("ainexus embed init failed", "err", err)
			os.Exit(1)
		}
		h.AINexus = ainx
		defer ainx.Close()
	}

	log.Info("server starting",
		"addr", cfg.Server.Addr,
		"ainexus_embedded", cfg.AINexus.Enabled,
		"clusters", len(cfg.Clusters),
	)

	r := router.New(h)

	if err := r.Run(cfg.Server.Addr); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
