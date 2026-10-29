// Package app 装配用户入口：CLI 子命令与网关 serve（托盘为 P1）。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"agenthub/internal/gateway"
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
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：serve、version\n", args[0])
		return ErrUsage
	}
}

func runServe(stdout io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &gateway.Server{Addr: gateway.DefaultAddr}
	fmt.Fprintf(stdout, "agenthub %s · 网关监听 http://%s （骨架：仅 /healthz；双协议于 M1-W2 实现）\n", Version, srv.Addr)
	return srv.Serve(ctx)
}
