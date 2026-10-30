// Package app 装配用户入口：CLI 子命令与网关 serve（M3-W6 起含托盘）。
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"fyne.io/systray"

	"yardmaster/internal/credential"
	"yardmaster/internal/dispatch"
	"yardmaster/internal/gateway"
	"yardmaster/internal/netproxy"
	"yardmaster/internal/otlp"
	"yardmaster/internal/store"
)

// Version 由 release 流程注入（-ldflags -X yardmaster/internal/app.Version=…），
// 开发态为 dev（开发文档 §8.2）。
var Version = "dev"

// ErrUsage 表示命令行用法错误（区别于运行时故障）。
var ErrUsage = errors.New("usage")

// Run 解析并执行一条子命令。无参数缺省 app（桌面应用模式：网关 +
// 托盘 + 应用窗口——「二进制直接运行即有界面」）；无头场景显式 serve。
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		args = []string{"app"}
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout)
	case "app": // 桌面应用模式（M5.5：零 cgo 应用窗口 + 托盘 + 进程内网关）
		return runApp(args[1:], stdout)
	case "console": // 显式打开网页控制台（serve/app 不再自动开网页）
		return runConsole(args[1:], stdout)
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
	case "update":
		return updateCheck(args[1:], stdout, stderr)
	case "tui":
		return runTUI(args[1:], stdout, stderr)
	case "run": // M5-W3：headless Agent 启动原语（stdin 任务 → stdout 透传）
		return runRun(args[1:], os.Stdin, stdout, stderr)
	case "dispatch": // M5-W5：任务→Agent 派发（agents/init/run）
		return runDispatch(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：app（缺省，桌面应用）、serve、console、version、provider、group、rules、keys、intents、trace、settings、agents、price、cost、update、tui、run、dispatch\n", args[0])
		return ErrUsage
	}
}

// serveSetup 是网关装配产物（serve 与 app 模式共用）。
type serveSetup struct {
	srv      *gateway.Server
	planner  *gateway.Planner
	paths    store.Paths
	settings store.Settings
	listen   string
}

// setupGateway 完成网关装配（配置读取、安全基线、Server 构造、派发挂载）。
func setupGateway() (*serveSetup, error) {
	paths, err := store.DefaultPaths()
	if err != nil {
		return nil, fmt.Errorf("定位数据目录: %w", err)
	}
	planner := loadPlanner(paths)
	settings, serr := store.LoadSettings(paths)
	if serr != nil && !errors.Is(serr, os.ErrNotExist) {
		return nil, fmt.Errorf("读取设置: %w", serr)
	}

	// 监听地址归一化 + 安全基线把关（F10）：非回环监听必须配置令牌，
	// 否则拒绝启动（宁可不起也不无鉴权暴露，M2-W5）。
	listen := settings.Listen
	if listen == "" {
		listen = gateway.DefaultAddr
	}
	if err := gateway.ValidateSecurity(listen, settings.Token); err != nil {
		return nil, err
	}

	srv := &gateway.Server{
		Addr:          listen,
		Planner:       planner,
		Version:       Version,
		GlobalProxy:   settings.GlobalProxy,
		Rules:         gateway.NewRuleEngine(paths.RulesPath()),
		Keys:          gateway.NewKeyStore(paths.KeysPath()),
		Traces:        gateway.NewTraceLog(paths.TraceDir()),
		AppDir:        paths.AppDir,
		ProvidersPath: paths.ProvidersPath(),
		Token:         settings.Token,
		Creds:         &credential.Store{Path: paths.CredentialsPath(), BackupDir: paths.BackupDir()},
		Intents: gateway.NewIntentChain(paths.IntentsPath(), settings.L1Classifier,
			l1TimeoutOrDefault(settings), newL1Caller(listen, settings.Token, settings.L1Classifier)),
	}

	// 派发引擎（M5-W4）：dispatch.json 损坏 → fail-closed 禁用派发并
	// 指引修复（网关其余功能不受影响）；不存在 → 无 agent 空引擎。
	dispEngine, derr := dispatch.LoadEngine(paths.DispatchPath())
	switch {
	case derr != nil:
		slog.Error("dispatch.json 不可用，派发功能已禁用", "err", derr)
	case len(dispEngine.Agents()) > 0:
		slog.Info("已加载派发配置", "agents", len(dispEngine.Agents()))
	}
	if derr == nil {
		// 会话持久化（M6-W2）：终态追加落盘 + 启动恢复最近环形进内存
		// ——控制台「派发」页历史跨重启可见。恢复的 running 会话标失败
		// （进程重启即被杀，不猜结果）。
		sstore := dispatch.NewSessionStore(paths.DispatchSessionsPath())
		dispEngine.AttachStore(sstore)
		dispEngine.RestoreSessions(sstore.Recent(dispatch.SessionRingCap))
		if n := len(dispEngine.Sessions()); n > 0 {
			slog.Info("已恢复派发会话历史", "count", n)
		}
		srv.AttachDispatch(dispEngine)
	}
	return &serveSetup{srv: srv, planner: planner, paths: paths, settings: settings, listen: listen}, nil
}

// startHelpers 启动后台辅助（OTLP 导出、OAuth 刷新、余额轮询、用量落盘
// 节流）；返回收尾函数（OTLP 关闭）。ctx 结束时各自退出。
func (st *serveSetup) startHelpers(ctx context.Context) func() {
	var stops []func()
	if st.settings.OTel.Enabled && st.settings.OTel.Endpoint != "" {
		exp := otlp.NewExporter(st.settings.OTel.Endpoint, st.settings.OTel.Headers)
		stops = append(stops, exp.Close)
		st.srv.Traces.OnTrace = exp.Export
		slog.Info("otlp traces 导出已开启", "endpoint", st.settings.OTel.Endpoint)
	}

	// OAuth 到期前主动刷新（M3-W3）：每分钟扫描，提前 10 分钟续期。
	// gemini-cli 的 client secret 经 settings 注入（公开常量但不入仓库，
	// GitHub secret scanning 拦截字面量）。
	if st.settings.GeminiClientSecret != "" {
		st.srv.SetRefreshSpec("gemini-cli", credential.RefreshSpec{
			TokenURL:     "https://oauth2.googleapis.com/token",
			ClientID:     "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com",
			ClientSecret: st.settings.GeminiClientSecret,
		})
	}
	st.srv.StartOAuthRefresher(ctx)
	st.srv.StartQuotaPoller(ctx)

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := st.planner.FlushUsage(st.paths.ProvidersPath()); err != nil {
					slog.Warn("用量落盘失败（下次重试）", "err", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		for _, f := range stops {
			f()
		}
	}
}

// banner 打印启动摘要。
func (st *serveSetup) banner(stdout io.Writer) {
	consoleURL := "http://" + st.listen + "/"
	fmt.Fprintf(stdout, "yardmaster %s · 网关监听 http://%s · 数据目录 %s\n", Version, st.srv.Addr, st.paths.AppDir)
	if st.settings.GlobalProxy != "" {
		fmt.Fprintf(stdout, "  全局代理: %s\n", netproxy.Redact(st.settings.GlobalProxy))
	}
	fmt.Fprintf(stdout, "  端点: /v1/chat/completions · /v1/messages · /healthz · /api/state · /api/trace\n")
	fmt.Fprintf(stdout, "  控制台: %s（总览/Provider/路由/观测/派发；管理 API /api/config/* · /api/reload）\n", consoleURL)
	if st.settings.L1Classifier != "" {
		fmt.Fprintf(stdout, "  L1 分类: %s（超时 %s）\n", st.settings.L1Classifier, l1TimeoutOrDefault(st.settings))
	}
}

// runServe 无头启动网关（不打开任何界面——网页控制台经 `yardmaster
// console` 或 app 模式窗口显式打开）。-tray 启用托盘常驻。
func runServe(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	tray := fs.Bool("tray", false, "启用系统托盘常驻（打开控制台/退出）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := setupGateway()
	if err != nil {
		return err
	}
	defer st.srv.Traces.Close()
	defer st.startHelpers(ctx)()
	st.banner(stdout)
	if *tray {
		go runTray(func() { _ = openBrowser("http://" + st.listen + "/") }, func() {
			slog.Info("托盘请求退出")
			stop()
			systray.Quit()
		})
		fmt.Fprintln(stdout, "  托盘: 已启用（系统托盘区图标）")
	}
	if err := st.srv.Serve(ctx); err != nil {
		return err
	}
	if ferr := st.planner.FlushUsage(st.paths.ProvidersPath()); ferr != nil {
		slog.Warn("退出前用量落盘失败", "err", ferr)
	}
	return nil
}

// runApp 桌面应用模式（无参数缺省）：进程内网关 + 托盘常驻 + 应用窗口
// （浏览器 --app 模式：无地址栏/标签页的独立窗口，FLClash 式观感；
// exec 纯 Go 零 cgo，不动交叉编译矩阵）。窗口关闭不影响网关（托盘
// 菜单可重开）；退出走托盘。
func runApp(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	noTray := fs.Bool("no-tray", false, "不启动系统托盘（窗口关闭后 Ctrl-C 退出）")
	noWindow := fs.Bool("no-window", false, "不打开应用窗口（等同 serve -tray）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := setupGateway()
	if err != nil {
		return err
	}
	defer st.srv.Traces.Close()
	defer st.startHelpers(ctx)()
	st.banner(stdout)
	consoleURL := "http://" + st.listen + "/?app=1"
	if !*noTray {
		go runTray(func() { _ = openAppWindow(consoleURL) }, func() {
			slog.Info("托盘请求退出")
			stop()
			systray.Quit()
		})
		fmt.Fprintln(stdout, "  托盘: 已启用（系统托盘区图标；退出请走托盘菜单）")
	}
	if !*noWindow {
		if werr := openAppWindow(consoleURL); werr == nil {
			fmt.Fprintln(stdout, "  应用窗口: 已打开（关闭窗口后台仍运行；重开走托盘菜单）")
		} else {
			fmt.Fprintln(stdout, "  应用窗口: 打开失败（"+werr.Error()+"），可用 yardmaster console 在浏览器打开")
		}
	}
	if err := st.srv.Serve(ctx); err != nil {
		return err
	}
	if ferr := st.planner.FlushUsage(st.paths.ProvidersPath()); ferr != nil {
		slog.Warn("退出前用量落盘失败", "err", ferr)
	}
	return nil
}

// runConsole 显式在浏览器打开控制台（用户指令；serve/app 不再自动开网页）。
// -addr 指向已运行的网关（本机或远程，远程需带令牌场景由控制台 401 引导）。
func runConsole(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	addr := fs.String("addr", "", "网关地址（空=本机 127.0.0.1:8317，或 settings.listen）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	target := consoleTarget(*addr)
	fmt.Fprintf(stdout, "打开控制台: %s\n", target)
	return openBrowser(target)
}

// consoleTarget 归一控制台地址：显式 addr > settings.listen > 缺省回环；
// 无协议前缀时补 http:// 与尾斜杠。
func consoleTarget(addr string) string {
	target := addr
	if target == "" {
		if paths, err := store.DefaultPaths(); err == nil {
			if settings, serr := store.LoadSettings(paths); serr == nil && settings.Listen != "" {
				target = settings.Listen
			}
		}
	}
	if target == "" {
		target = gateway.DefaultAddr
	}
	if !strings.Contains(target, "://") {
		target = "http://" + target + "/"
	}
	return target
}

// newL1Caller 构造 L1 分类回调：经网关自身推断端点调用分类模型
// （享受故障转移与代理，F8），带内部标记头防止递归分类；token 非空时
// 携带访问令牌（M2-W5：本机回环 + token 态下自调用也须过闸）。
func newL1Caller(addr, token, model string) gateway.L1Caller {
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
		if token != "" {
			req.Header.Set("X-Gw-Token", token)
		}
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
