// Command server is the OpsGaurdWeb management-plane API server.
// It manages multiple clusters (via Worker agents) and embeds the
// AiNexus AI troubleshooting gateway in-process (vendored under
// internal/ainexus, no standalone service). Persistence is LevelDB
// (internal/store), P1 wires the cluster registry + Worker probing.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/patrol"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/router"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
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

	// 存储层（LevelDB）
	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		log.Error("store open failed", "path", cfg.Store.Path, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := api.NewHandlers()

	// 集群注册表服务（P1：CRUD + Worker 健康探测）
	clusterSvc := cluster.New(st)
	h.SetClusterService(clusterSvc)

	// 告警 ingest 服务（P3：Worker webhook → 事件落库 + 告警聚合）
	h.SetIngestService(ingest.New(st), cfg.Server.IngestToken)

	// 内嵌 AiNexus 网关（与管理端同进程，无独立服务/端口）
	var ainx *ainexusserver.Server
	if cfg.AINexus.Enabled {
		ainx = ainexusserver.New(&cfg.AINexus)
		if err := ainx.Initialize(context.Background()); err != nil {
			log.Error("ainexus embed init failed", "err", err)
			os.Exit(1)
		}
		h.AINexus = ainx
		defer ainx.Close()
	}

	// 智能巡检服务（P5：YAML 流程 + 单实例 cron 调度 + AI 报告）
	patrolSvc := patrol.New(st, clusterSvc, ainx)
	h.SetPatrolService(patrolSvc)
	patrolSvc.Start()
	defer patrolSvc.Stop()

	log.Info("server starting",
		"addr", cfg.Server.Addr,
		"store", cfg.Store.Path,
		"ainexus_embedded", cfg.AINexus.Enabled,
		"clusters", len(cfg.Clusters),
	)

	r := router.New(h)

	if err := r.Run(cfg.Server.Addr); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
