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
		// 病根切换（2026-11-20 v21）：窄条病与 resize 时序无关（v19 创建
		// 尺寸即终态、零 post-attach resize 仍病），Chrome 子窗口 HWND 满
		// 宽而内容恒挤 ~470px 绝对宽度——windowed（HWND 子窗口）hosting
		// 的渲染 surface 缺陷家族。改走 IDCompositionVisual hosting：
		// wails 在 WebView2 环境创建前置入 COREWEBVIEW2_FORCED_HOSTING_
		// MODE=WINDOW_TO_VISUAL，上游文档明示 windowed hosting 在 DPI 上
		// 下文不匹配时卡死重光栅化（WebView2Feedback #5248/#4485），与本
		// 病吻合。须在 app.Run() 前设置——New 即满足。副作用：Chrome_
		// WidgetWin_* 子窗口不再存在，伺服几何比对恒空转至超时（仅一条
		// WARN 日志，无害；若 hosting 切换治愈则伺服体系整体退役）。
		Windows: application.WindowsOptions{
			UseVisualHosting: true,
			// 诊断临时（v34，窄条根治后删除）：WebView2 Fixed Version
			// 154.0.4258.62 降级实验（155 已证病，封死版本因素链尾）。
			WebviewBrowserPath: `C:\Users\jasse\AppData\Local\Temp\wv2fv\extract154\Microsoft.WebView2.FixedVersionRuntime.154.0.4258.62.x64`,
			// 渲染参数保持最小（v26-v31 已证 GPU/ANGLE/DC/occlusion
			// flag 全部无效），仅保留 CDP 调试口供自动化测试取证。
			AdditionalBrowserArgs: []string{
				"--remote-debugging-port=9222",
				"--remote-allow-origins=*",
			},
		},
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
		MinWidth:  minWindowW,
		MinHeight: minWindowH,
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
	if state != nil && state.Max {
		// 防病路线（2026-11-20 v19）：窄条根因=wails 创建流程固定为
		// CreateWindowEx(还原尺寸)→setupChromium 按「当前」rect 附件
		// （v3.1.0 webview_window_windows.go L620）→StartState（L706）/
		// 延迟 Maximise 全部发生在附件之后——渲染 surface 永远先卡在还
		// 原尺寸（上游 #5544 「until a manual resize」；v11-v18 八种窗
		// 口级事后修复全无效实锤，StartState 即原始窄条入口）。唯一不
		// 败顺序=创建尺寸即终态尺寸：按主屏最大化几何直接创建正常态窗
		// 口，终身零 post-attach resize，竞态入口消失。视觉与真最大化
		// 无法区分；代价：IsMaximised 恒 false（状态记忆自洽回落为该
		// rect，行为不变）、多显示器时假最大化固定落主屏（留档）。
		// StartState=WindowMaximised 禁用——wails 内部 maximise() 是
		// post-attach resize，必踩竞态。
		fx, fy, fw, fh := fakeMaxRectLogical()
		opts.X, opts.Y, opts.Width, opts.Height = fx, fy, fw, fh
	} else if state != nil {
		// 还原态恢复：创建尺寸=终态尺寸，同样零 post-attach resize。
		opts.X, opts.Y, opts.Width, opts.Height = state.X, state.Y, state.W, state.H
	} else {
		opts.Width, opts.Height = 1200, 820
	}
	win := app.Window.NewWithOptions(opts)
	// 窗口渲染面同步保障（2026-11-20 v19 防病版）：根治改为「创建尺寸
	// 即终态尺寸」（见上方状态恢复块），post-attach resize 竞态入口消
	// 失。伺服降级为安全网：探针异常才修复（NavigationCompleted 的
	// Hide/Show hack 在 rect 恒定时无过期 rect 可用，理论上不再致病；
	// 一旦仍复现窄条，修复链按 zoomed 分流，详见 webview_sync_windows.go
	// 头注取证链）。结果留观测日志。
	// v35b 定案实验（2026-11-20）：伺服=窄条制造者实锤——minweb 最小
	// 复现（同 wails/WebView2/UseVisualHosting/假最大化几何）无伺服则
	// 完全正常；yardmaster 唯一差异是伺服链。探针误报 → 触发拖拽模拟
	// + Min/Restore「修复」→ 危险操作族自己打卡死 surface。本实验停用
	// 伺服验证治愈；根治后伺服体系整体退役。
	go func() {
		if true { // v35b：伺服停用
			slog.Info("伺服停用（v35b 实验层）")
			return
		}
		for i := 0; i < 100; i++ { // ~10s 上限（impl 在 app.Run() 才创建）
			if win.NativeWindow() != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		h := win.NativeWindow()
		if h == nil {
			return
		}
		repair := func(zoomed bool) {
			if win.NativeWindow() == nil {
				return
			}
			// 修复后不再 re-Maximise（v13/v14 实证：SC_SIZE|2 sizing 循
			// 环摆动生效、窗口 rect 真实跟随光标（wMin/wMax 摆幅 120px
			// 实锤），但 WM_SIZE 流不足以驱动 WebView2 重光栅化——渲染
			// surface 依旧卡窄条）。补刀 Hide/Show：按当前稳定 rect 重挂
			// 渲染树（wails NavigationCompleted #1077 hack 的正确时序版
			// ——此刻无导航竞态，Show 重挂即按新尺寸重建 surface）。
			// 窗口停在还原态（≈1696 宽）完全可用。
			webviewDragResize(uintptr(h))
			time.Sleep(1500 * time.Millisecond)
			blank := webviewHeaderLooksBlank(uintptr(h))
			if blank {
				// Minimize/Restore：走 wails WM_SIZE 处理内置的 #5544
				// 修复链——SIZE_RESTORED 分支会 resyncWebviewDPIAfterUnminimise
				// + chromium.Show()（v3.1.0 webview_window_windows.go，
				// 官方针对「devicePixelRatio 卡错显示器值直到手动 resize」
				// 的重同步路径）。SC_SIZE/Hide/Show 只更新 rect 不重建
				// surface（v13-15 实证全无效），此处直接重置光栅化上下文。
				// SW_RESTORE 恢复到最小化前的还原态 rect，不碰 rcNormalPosition。
				procShowWindowSync.Call(uintptr(h), 6) // SW_MINIMIZE
				time.Sleep(300 * time.Millisecond)
				procShowWindowSync.Call(uintptr(h), 9) // SW_RESTORE
				time.Sleep(1500 * time.Millisecond)
				blank = webviewHeaderLooksBlank(uintptr(h))
				slog.Info("修复复验：Min/Restore 后", "blank", blank)
			}
			slog.Info("修复复验：拖拽后", "zoomed", zoomed, "blank", blank)
		}
		if webviewSurfaceServo(uintptr(h), repair) {
			slog.Info("WebView2 渲染面伺服收敛")
		} else {
			slog.Warn("WebView2 渲染面伺服超时（窄条未确认收敛，手动拖拽窗口可即时恢复）")
		}
	}()
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
