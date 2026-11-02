// Wails v3 原生窗口壳（M8-W2，桌面壳两步走第二步）：Windows app 模式
// 以 WebView2 原生窗口装载控制台（?app=1 纯 URL 直载，零 JS 桥——
// AllowSimpleEventEmit 不开、不注册 Service/bindings，窗口只是指向
// 本机回环的壳）。本文件是全仓库唯一 wails import 点（依赖边界铁律
// app.md §三，TestCorePackagesNoWailsDependency 断言）；关窗=隐藏到
// 托盘（W1 决策），退出走托盘；WebView2 运行时缺失回退浏览器窗口
// （app.md §五两道闸）。
//
//go:build windows

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"golang.org/x/sys/windows/registry"
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

	app := application.New(application.Options{Name: "Yardmaster"})
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
	win.OnWindowEvent(events.Windows.WindowClosing, func(*application.WindowEvent) {
		snapshot()
		win.Hide() // 关窗=隐藏到托盘；重开走托盘「打开窗口」
	})
	nativeWindow.Store(win)

	// Ctrl-C（ctx 取消）与托盘退出（onExit→stop()）统一经 ctx→Quit
	// 收敛到单一路径。
	go func() {
		<-ctx.Done()
		snapshot()
		app.Quit()
	}()

	runErr := app.Run()
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
