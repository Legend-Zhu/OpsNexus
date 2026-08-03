package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/legeosoft/ainexus/internal/config"
	"github.com/legeosoft/ainexus/internal/server"
)

var (
	version   = "1.0.0"
	buildTime = "unknown"
	gitCommit = "unknown"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to configuration file")
	showVersion := flag.Bool("version", false, "show version information")
	flag.Parse()

	if *showVersion {
		fmt.Printf("AiNexus v%s (commit: %s, built: %s)\n", version, gitCommit, buildTime)
		os.Exit(0)
	}

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 创建服务器
	srv := server.New(cfg)

	// 初始化
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Initialize(ctx); err != nil {
		log.Fatalf("Failed to initialize server: %v", err)
	}

	// 启动服务器
	if err := srv.Run(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
