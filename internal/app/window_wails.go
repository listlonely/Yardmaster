// Wails v3 原生窗口壳（M8-W2，桌面壳两步走第二步）：Windows app 模式
// 以 WebView2 原生窗口装载控制台（?app=1 纯 URL 直载，零 JS 桥——
// AllowSimpleEventEmit 不开、不注册 Service/bindings，窗口只是指向
// 本机回环的壳）。本文件是全仓库唯一 wails import 点（依赖边界铁律
// app.md §三，TestCorePackagesNoWailsDependency 断言）；关窗=隐藏到
// 托盘（W1 决策），退出走托盘；WebView2 运行时缺失回退浏览器窗口
// （app.md §五两道闸）。
//
// 仅 windows/amd64 [2026-11-09-arch386]：wails v3 pkg/w32 的 combridge
// 在 init 经 syscall.NewCallback 注册 COM 虚表，回调参数按 64 位形状
// 编译——windows/386 启动即 panic（compileCallback: argument size is
// larger than uintptr）。386 构建排除本文件，nativeShell* 钩子保持
// nil → app 模式自动走浏览器窗口回退（与非支持平台同路径）。
//
//go:build windows && amd64

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows/registry"

	"yardmaster/internal/store"
)

// webview2ClientKey 是 WebView2 Evergreen 运行时的 EdgeUpdate 注册表
// 键（pv 值非空即视为在位）。机器级键在 64 位视图经 WOW6432Node，32
// 位视图与用户级各一份；任一命中即足（W1 实测本机为 HKLM
// WOW6432Node，156.0.4314.58）。
const webview2ClientKey = `Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

// webview2RuntimePresent 探测系统 WebView2 运行时（纯 Go registry，
// 不碰 wails 内部包）。
func webview2RuntimePresent() bool {
	for _, ref := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\` + webview2ClientKey},
		{registry.LOCAL_MACHINE, `SOFTWARE\` + webview2ClientKey},
		{registry.CURRENT_USER, `Software\` + webview2ClientKey},
	} {
		k, err := registry.OpenKey(ref.root, ref.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pv, _, err := k.GetStringValue("pv")
		_ = k.Close()
		if err == nil && pv != "" {
			return true
		}
	}
	return false
}

// nativeWindow 持有当前原生窗口引用（托盘「打开窗口」重开用）。窗口
// 方法内部经 InvokeSync 派发（同线程直调/跨线程投递，application.go
// dispatchOnMainThread），跨 goroutine 调用安全。
var nativeWindow atomic.Pointer[application.WebviewWindow]

func init() {
	nativeShellProbe = func() error {
		// 应急开关/回退演练钩子（W6）：置位即跳过原生窗口——模拟 WebView2
		// 缺失的完整回退路径，也为受控环境提供禁用出口（留痕可观测）。
		if os.Getenv("YARDMASTER_NO_NATIVE_WINDOW") != "" {
			return errors.New("YARDMASTER_NO_NATIVE_WINDOW 置位（跳过原生窗口）")
		}
		if !webview2RuntimePresent() {
			return errors.New("WebView2 运行时未安装")
		}
		return nil
	}
	nativeShellRun = runWailsShell
	nativeReopen = reopenNativeWindow
}

// reopenNativeWindow 托盘「打开窗口」：原生窗口在位则 Show+Focus；
// 不在位（未创建/壳已退出）回退浏览器窗口。
func reopenNativeWindow(consoleURL string) {
	if w := nativeWindow.Load(); w != nil {
		w.Show()
		w.Focus()
		return
	}
	_ = openAppWindow(consoleURL)
}

// snapshotWindowBounds 取当前窗口几何（逻辑坐标）与最大化态；窗口不在
// 位（未创建/已销毁，Bounds 返回零值）返回 nil 不落盘。
func snapshotWindowBounds(win *application.WebviewWindow) *windowState {
	if win == nil {
		return nil
	}
	b := win.Bounds()
	if b.Width <= 0 || b.Height <= 0 {
		return nil
	}
	return &windowState{X: b.X, Y: b.Y, W: b.Width, H: b.Height, Max: win.IsMaximised()}
}

// runWailsShell 原生窗口主循环：网关 serve 转后台 goroutine，wails 占
// 主线程（Win32 消息泵）——托盘已由调用方在独立 goroutine 启动（fyne
// systray 自有线程泵，两泵各占一线程，app.md §二#6）。返回时网关已
// 收尾（Serve 返回 + 用量落盘）。
func runWailsShell(st *serveSetup, ctx context.Context, stop context.CancelFunc, consoleURL string, stdout io.Writer) error {
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveUntilDone(st, ctx) }()

	app := application.New(application.Options{
		Name: "Yardmaster",
		// 单实例互斥（2026-11-10，兑现 §二「勿自建」盘点）：锁在 New 时
		// 获取，二次启动进程通知首实例后自动 os.Exit——不再出现第二个
		// 窗口/第二份托盘（二次启动早期托盘图标会短暂闪现 ~1s，锁在此
		// 才获取，留档）。首实例回调=前置既有窗口（托盘「打开窗口」同
		// 语义）；首实例为 serve/浏览器回退态时不持有锁（端口绑定天然
		// 防双网关），如实留档。
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "yardmaster-app",
			ExitCode: 0,
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				reopenNativeWindow(consoleURL)
			},
		},
	})
	// 切断平台 WindowClosing→Common 的默认映射：Common.WindowClosing
	// 的内部订阅者会无条件销毁窗口（NewWindow 内置），我们要的是
	// 「关窗=隐藏」。其余映射原样保留。
	mapping := events.DefaultWindowEventMapping()
	delete(mapping, events.Windows.WindowClosing)

	// 窗口状态记忆（M8-W3）：恢复上次尺寸/位置/最大化；初始背景色跟
	// 系统深浅色（防主题不符闪色；运行中切换由页面自身全覆盖背景承接）。
	state := loadWindowState(st.paths.AppDir)
	opts := application.WebviewWindowOptions{
		Name:      "console",
		Title:     "Yardmaster",
		MinWidth:  900,
		MinHeight: 600,
		URL:       consoleURL,
		Windows: application.WindowsWindow{
			EventMapping: mapping,
		},
	}
	if systemPrefersLight() {
		opts.BackgroundColour = application.RGBA{Red: 0xf6, Green: 0xf7, Blue: 0xf9, Alpha: 0xff}
	} else {
		opts.BackgroundColour = application.RGBA{Red: 0x14, Green: 0x18, Blue: 0x21, Alpha: 0xff}
	}
	if state != nil {
		opts.X, opts.Y, opts.Width, opts.Height = state.X, state.Y, state.W, state.H
		if state.Max {
			opts.StartState = application.WindowStateMaximised
		}
	} else {
		opts.Width, opts.Height = 1200, 820
	}
	win := app.Window.NewWithOptions(opts)
	snapshot := func() { saveWindowState(st.paths.AppDir, snapshotWindowBounds(win)) }
	// 关窗行为（settings.close_action，2026-11-10）：关窗事件时实时读盘
	//——设置保存后下次关窗即生效，无需重启。hide=隐藏到托盘（缺省，
	// W1 现状）；exit=直接退出（stop+Quit，与 Ctrl-C 同一收敛路径，托盘
	// 图标随进程退出消失）。读盘失败/未知值按 hide 处理（读不懂=缺省，
	// 绝不因配置异常把常驻网关悄悄退出）。
	win.OnWindowEvent(events.Windows.WindowClosing, func(*application.WindowEvent) {
		snapshot()
		if s, err := store.LoadSettings(st.paths); err == nil && s.CloseActionMode() == store.CloseExit {
			stop() // 触发 ctx 取消：网关收尾 + 下面的 Quit goroutine 兜底
			app.Quit()
			return
		}
		win.Hide() // 关窗=隐藏到托盘；重开走托盘「打开窗口」
	})
	// 标题栏图标补设（wails 只发 ICON_BIG，小图标仍为窗口类默认——
	// window_icon_windows.go 头注）。窗口 impl 在 app.Run() 处理
	// pendingRun 时才创建，且本壳直载控制台页无 wails runtime、
	// RuntimeReady 不触发——轮询句柄就绪后补发（SendMessage 跨线程
	// 安全，主线程 Run 已在泵消息）。窗口销毁/退出即轮询终止。
	go func() {
		for i := 0; i < 50; i++ { // ~10s 上限
			h := win.NativeWindow()
			if h != nil {
				setWindowIcons(uintptr(h))
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	nativeWindow.Store(win)

	// Ctrl-C（ctx 取消）与托盘退出（onExit→stop()）统一经 ctx→Quit
	// 收敛到单一路径。
	go func() {
		<-ctx.Done()
		snapshot()
		app.Quit()
	}()

	// 壳层 panic 隔离（M8-W5，语义同插件纪律）：wails 主循环 panic 转
	// 为 error，走与 runErr 相同的浏览器回退路径——壳崩溃不带倒网关进程。
	runErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("wails 主循环 panic: %v", r)
			}
		}()
		return app.Run()
	}()
	nativeWindow.Store(nil)
	if runErr != nil {
		// 原生壳启动后故障（如 WebView2 环境创建失败）：网关不陪葬，
		// 回退浏览器窗口继续服务（app.md §五第二道闸），阻塞至托盘
		// 退出/Ctrl-C。
		slog.Warn("原生窗口壳故障，回退浏览器窗口", "err", runErr)
		fmt.Fprintln(stdout, "  应用窗口: 原生窗口故障（"+runErr.Error()+"），已回退浏览器窗口")
		if oerr := openAppWindow(consoleURL); oerr != nil {
			slog.Warn("浏览器窗口回退失败", "err", oerr)
		}
		return <-serveDone
	}
	stop()
	return <-serveDone
}
