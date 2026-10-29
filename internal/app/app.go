// Package app 装配用户入口：CLI 子命令与网关 serve（托盘为 P1）。
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"yardmaster/internal/gateway"
	"yardmaster/internal/netproxy"
	"yardmaster/internal/store"
)

// Version 由 release 流程注入（-ldflags -X yardmaster/internal/app.Version=…），
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
		fmt.Fprintln(stdout, "yardmaster "+Version)
		return nil
	case "provider":
		return runProvider(args[1:], stdout, stderr)
	case "group":
		return runGroup(args[1:], stdout, stderr)
	case "rules":
		return runRules(args[1:], stdout, stderr)
	case "keys":
		return runKeys(args[1:], stdout, stderr)
	case "intents":
		return runIntents(args[1:], stdout, stderr)
	case "trace":
		return runTrace(args[1:], stdout, stderr)
	case "price":
		return runPrice(args[1:], stdout, stderr)
	case "cost":
		return runCost(args[1:], stdout, stderr)
	case "settings":
		return runSettings(args[1:], stdout, stderr)
	case "agents":
		return runAgents(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：serve、version、provider、group、rules、keys、intents、trace、settings、agents、price、cost\n", args[0])
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

	srv := &gateway.Server{
		Addr:          gateway.DefaultAddr,
		Planner:       planner,
		Version:       Version,
		GlobalProxy:   settings.GlobalProxy,
		Rules:         gateway.NewRuleEngine(paths.RulesPath()),
		Keys:          gateway.NewKeyStore(paths.KeysPath()),
		Traces:        gateway.NewTraceLog(paths.TraceDir()),
		AppDir:        paths.AppDir,
		ProvidersPath: paths.ProvidersPath(),
		Intents: gateway.NewIntentChain(paths.IntentsPath(), settings.L1Classifier,
			l1TimeoutOrDefault(settings), newL1Caller(gateway.DefaultAddr, settings.L1Classifier)),
	}
	defer srv.Traces.Close()

	// 当日用量节流落盘（W6）：每分钟一次 + 退出前一次。
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := planner.FlushUsage(paths.ProvidersPath()); err != nil {
					slog.Warn("用量落盘失败（下次重试）", "err", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	fmt.Fprintf(stdout, "yardmaster %s · 网关监听 http://%s · 数据目录 %s\n", Version, srv.Addr, paths.AppDir)
	if settings.GlobalProxy != "" {
		fmt.Fprintf(stdout, "  全局代理: %s\n", netproxy.Redact(settings.GlobalProxy))
	}
	fmt.Fprintf(stdout, "  端点: /v1/chat/completions · /v1/messages · /healthz · /api/state · /api/trace\n")
	fmt.Fprintf(stdout, "  控制台: http://%s（总览/Provider/路由/观测；管理 API /api/config/* · /api/reload）\n", srv.Addr)
	if settings.L1Classifier != "" {
		fmt.Fprintf(stdout, "  L1 分类: %s（超时 %s）\n", settings.L1Classifier, l1TimeoutOrDefault(settings))
	}
	err = srv.Serve(ctx)
	if ferr := planner.FlushUsage(paths.ProvidersPath()); ferr != nil {
		slog.Warn("退出前用量落盘失败", "err", ferr)
	}
	return err
}

// newL1Caller 构造 L1 分类回调：经网关自身推断端点调用分类模型
// （享受故障转移与代理，F8），带内部标记头防止递归分类。
func newL1Caller(addr, model string) gateway.L1Caller {
	if model == "" {
		return nil
	}
	url := "http://" + addr + "/v1/chat/completions"
	client := &http.Client{} // 超时由 IntentChain 的 ctx 控制
	return func(ctx context.Context, prompt string) (string, error) {
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": 16,
		})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(gateway.L1SelfHeader, gateway.L1SelfValue)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("L1 调用 HTTP %d: %s", resp.StatusCode, netproxy.Snippet(b))
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return "", fmt.Errorf("解析 L1 响应: %w", err)
		}
		if len(out.Choices) == 0 {
			return "", errors.New("L1 响应无 choices")
		}
		return out.Choices[0].Message.Content, nil
	}
}

// loadPlanner 从 providers.json 读取 provider/组快照；文件不存在或损坏时
// 以空配置启动（「读不懂=未知」：损坏必须记日志并保留空配置运行，
// 绝不当作有配置——调用方报错行为一致）。
func loadPlanner(paths store.Paths) *gateway.Planner {
	logger := slog.Default()
	cfg, err := store.LoadProvidersFile(paths.ProvidersPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Warn("providers.json 不存在，网关将以无 provider 状态启动", "path", paths.ProvidersPath())
	case err != nil:
		logger.Error("providers.json 解析失败，按未知处理并以空配置启动（保留原文件）", "path", paths.ProvidersPath(), "err", err)
	default:
		logger.Info("已加载 provider 配置", "providers", len(cfg.Providers), "groups", len(cfg.Groups))
	}
	return gateway.NewPlanner(cfg.Providers, cfg.Groups)
}
