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
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/auth"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
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

	// 通知服务（P6：渠道/策略/记录）
	notifySvc := notify.New(st)
	h.SetNotifyService(notifySvc)

	// 告警规则服务（P6：管理 Worker monitoring config）
	h.SetAlertRuleService(alertrule.New(st, clusterSvc))

	// 认证服务（P6：本地用户 + SSO/OIDC 抽象）
	authSvc := auth.New(st, cfg.Auth.TokenSecret, parseDuration(cfg.Auth.TokenTTL, 24*time.Hour), oidcFromConfig(cfg.Auth.SSO))
	h.SetAuthService(authSvc)
	if cfg.Auth.TokenSecret != "" || cfg.Auth.SSO != nil {
		// 配置了密钥/SSO → 启用认证；未配置（内网 bootstrap）→ 不启用
		// 引导：无任何用户时播种默认 admin（admin/opsguard-admin，首登后应修改）
		users, _ := authSvc.ListUsers()
		if len(users) == 0 {
			if _, err := authSvc.CreateUser("admin", bootstrapAdminPassword(), "admin", true); err != nil {
				log.Warn("bootstrap admin failed", "err", err)
			} else {
				log.Info("bootstrapped default admin (admin)")
			}
		}
		public := []string{"/healthz", "/api/v1/ingest/events", "/api/v1/auth/login", "/ainexus"}
		h.SetAuthMiddleware(authSvc.Middleware(public))
		log.Info("auth enabled", "sso", cfg.Auth.SSO != nil)
	}

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

// parseDuration 解析时长字符串，失败回退默认值。
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// oidcFromConfig 提取 OIDC 配置（未配置返回 nil → 仅本地用户）。
func oidcFromConfig(sso *config.SSOConfig) *auth.OIDCConfig {
	if sso == nil || sso.OIDC == nil {
		return nil
	}
	return &auth.OIDCConfig{
		Issuer:       sso.OIDC.Issuer,
		ClientID:     sso.OIDC.ClientID,
		ClientSecret: sso.OIDC.ClientSecret,
		RedirectURL:  sso.OIDC.RedirectURL,
	}
}

// bootstrapAdminPassword 引导管理员默认密码（可经环境变量覆盖）。
func bootstrapAdminPassword() string {
	if p := os.Getenv("OPSGUARD_ADMIN_PASSWORD"); p != "" {
		return p
	}
	return "opsguard-admin"
}
