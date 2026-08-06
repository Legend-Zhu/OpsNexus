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
	"net"
	"os"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexusrt"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/alertrule"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/api"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/auth"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/notify"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/patrol"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/registry"
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

	// 告警 ingest 服务（P3：事件落库 + 告警聚合）。事件经 gRPC SubscribeEvents
	// 流从各 Worker 拉取（ingest.Manager 管理每集群一个订阅 goroutine），
	// 替代了原 Worker→server webhook 推送（单向网络策略下不可用）。
	ingestSvc := ingest.New(st)

	// 内嵌 AiNexus 网关（与管理端同进程，无独立服务/端口）。配置可在
	// 页面「系统设置 → AI 排查网关」在线修改并热重载（无需重启）；首次保存
	// 后以 LevelDB 中的运行时配置为准，之前回退 config.yaml 的 ainexus 块。
	ainexusRT := ainexusrt.New(st, clusterSvc, &cfg.AINexus)
	if err := ainexusRT.Init(context.Background()); err != nil {
		log.Error("ainexus embed init failed", "err", err)
		os.Exit(1)
	}
	h.SetAINexusRT(ainexusRT)

	// 通知服务（P6：渠道/策略/记录）
	notifySvc := notify.New(st)
	h.SetNotifyService(notifySvc)

	// 内嵌镜像仓库(OCI /v2 + 页面传包构建;构建 push 走本机 loopback)
	if cfg.Registry.Enabled {
		regSvc, err := registry.NewService(cfg.Registry, serverPort(cfg.Server.Addr))
		if err != nil {
			log.Error("registry init failed", "err", err)
			os.Exit(1)
		}
		h.SetRegistryService(regSvc)
		// 构建 push 账号登录本机 registry(loopback 免 HTTPS);失败仅告警,
		// 不阻断启动(docker 未装时构建接口会明确 503)
		if err := regSvc.DockerLogin(context.Background()); err != nil {
			log.Warn("registry builder docker login failed", "err", err)
		}
		log.Info("embedded registry enabled", "storage", cfg.Registry.Storage,
			"hostname", cfg.Registry.Hostname, "auth", len(cfg.Registry.Users) > 0)
	}

	// 智能巡检服务（P5：YAML 流程 + 单实例 cron 调度 + AI 报告 +
	// 报告渠道投递/异常转告警闭环）
	patrolSvc := patrol.New(st, clusterSvc, ainexusRT, notifySvc)
	h.SetPatrolService(patrolSvc)
	patrolSvc.Start()
	defer patrolSvc.Stop()

	// 事件订阅管理器（P3：每集群一个 gRPC SubscribeEvents goroutine，拉取
	// Worker 监控事件 → ingestSvc 落库 + 告警聚合；断线带游标重连）。
	// builder 适配 cluster.Service.WorkerClient → SubscribeEvents 流。
	ingestMgr := ingest.NewManager(func(ctx context.Context, clusterName string, afterSeq int64) (ingest.EventSubscriber, func(), error) {
		cli, err := clusterSvc.WorkerClient(clusterName)
		if err != nil {
			return nil, func() {}, err
		}
		sub, err := cli.SubscribeEvents(ctx, afterSeq)
		if err != nil {
			cli.Close()
			return nil, func() {}, err
		}
		return sub, func() { cli.Close() }, nil
	}, ingestSvc, st, log)
	if err := ingestMgr.Start(context.Background()); err != nil {
		log.Error("ingest manager start failed", "err", err)
	}
	defer ingestMgr.Stop()

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
		public := []string{"/healthz", "/api/v1/auth/login", "/api/v1/auth/sso", "/api/v1/auth/callback", "/ainexus"}
		h.SetAuthMiddleware(authSvc.Middleware(public))
		log.Info("auth enabled", "sso", cfg.Auth.SSO != nil)
	}

	log.Info("server starting",
		"addr", cfg.Server.Addr,
		"store", cfg.Store.Path,
		"ainexus_embedded", ainexusRT.Server() != nil,
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

// serverPort 从监听地址提取端口（":8090" → "8090";"0.0.0.0:8090" → "8090"）。
func serverPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "8090"
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
		FrontendURL:  sso.OIDC.FrontendURL,
		Scopes:       sso.OIDC.Scopes,
		DefaultRole:  sso.OIDC.DefaultRole,
	}
}

// bootstrapAdminPassword 引导管理员默认密码（可经环境变量覆盖）。
func bootstrapAdminPassword() string {
	if p := os.Getenv("OPSGUARD_ADMIN_PASSWORD"); p != "" {
		return p
	}
	return "opsguard-admin"
}
