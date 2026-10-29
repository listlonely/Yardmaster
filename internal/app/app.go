// Package app 装配用户入口：CLI 子命令与网关 serve（托盘为 P1）。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"agenthub/internal/gateway"
	"agenthub/internal/netproxy"
	"agenthub/internal/provider"
	"agenthub/internal/store"
)

// Version 由 release 流程注入（-ldflags -X agenthub/internal/app.Version=…），
// 开发态为 dev（开发文档 §8.2）。
var Version = "dev"

// ErrUsage 表示命令行用法错误（区别于运行时故障）。
var ErrUsage = errors.New("usage")

// Run 解析并执行一条子命令。骨架阶段仅 serve 与 version。
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		return runServe(stdout)
	case "version":
		fmt.Fprintln(stdout, "agenthub "+Version)
		return nil
	case "provider":
		return runProvider(args[1:], stdout, stderr)
	case "settings":
		return runSettings(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：serve、version、provider、settings\n", args[0])
		return ErrUsage
	}
}

func runServe(stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	paths, err := store.DefaultPaths()
	if err != nil {
		return fmt.Errorf("定位数据目录: %w", err)
	}
	planner := loadPlanner(paths)
	settings, serr := store.LoadSettings(paths)
	if serr != nil && !errors.Is(serr, os.ErrNotExist) {
		return fmt.Errorf("读取设置: %w", serr)
	}

	srv := &gateway.Server{Addr: gateway.DefaultAddr, Planner: planner, Version: Version, GlobalProxy: settings.GlobalProxy}
	fmt.Fprintf(stdout, "agenthub %s · 网关监听 http://%s · 数据目录 %s\n", Version, srv.Addr, paths.AppDir)
	if settings.GlobalProxy != "" {
		fmt.Fprintf(stdout, "  全局代理: %s\n", netproxy.Redact(settings.GlobalProxy))
	}
	fmt.Fprintf(stdout, "  端点: /v1/chat/completions · /v1/messages · /healthz · /api/state\n")
	return srv.Serve(ctx)
}

// loadPlanner 从 providers.json 读取 provider 快照；文件不存在或损坏时
// 以空配置启动（「读不懂=未知」：损坏必须记日志并保留空配置运行，
// 绝不当作有配置——调用方报错行为一致）。
func loadPlanner(paths store.Paths) *gateway.Planner {
	logger := slog.Default()
	var cfg struct {
		Providers []provider.Provider `json:"providers"`
	}
	err := store.LoadJSON(paths.ProvidersPath(), &cfg)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Warn("providers.json 不存在，网关将以无 provider 状态启动", "path", paths.ProvidersPath())
	case err != nil:
		logger.Error("providers.json 解析失败，按未知处理并以空配置启动（保留原文件）", "path", paths.ProvidersPath(), "err", err)
	default:
		logger.Info("已加载 provider 配置", "count", len(cfg.Providers))
	}
	return gateway.NewPlanner(cfg.Providers)
}
