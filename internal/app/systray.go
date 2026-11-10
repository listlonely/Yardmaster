// 系统托盘常驻（M3-W6，桌面壳两步走第一步）：纯 Go 托盘
// （fyne.io/systray：Windows Win32 syscall / Linux D-Bus SNI，无 cgo，
// 不动交叉编译铁律）。
//
// M9-W4 动态化（Magpie traymenu 同构——原生菜单非用户可配置，动态性
// 体现在版本行/升级入口/状态摘要的运行时改标题）：固定项=打开窗口/退出；
// 信息项=版本（禁用态）、「升级到 x.y.z」（有新版才显示，M11-W3 起
// 点击=应用内安装）、状态摘要（休眠/派发进行中，30s 刷新）。i18n 不做
// （M8 留档不采纳延续）。
//
// 图标（M11 后期换装）：tray_icon.ico（tools/genwinres 从 appicon.png
// 生成——jimeng 图第一排第三：黑底白色发光 S 形岔路箭头 + 白色圆角外
// 框保证深色任务栏可见）。**必须喂 ICO**：fyne systray Windows 端
// SetIcon 经 LoadImage(LR_LOADFROMFILE) 只认 .ico 文件，PNG 喂入静默
// 失败（GetLastError=0 假阴性）=托盘从未显示图标（2026-11-04 用户截图
// 实证并修复）。见 tools/genwinres 头注释。
package app

import (
	_ "embed"
	"log/slog"
	"os/exec"
	"runtime"
	"time"

	"fyne.io/systray"
)

//go:embed tray_icon.ico
var trayIconICO []byte

// trayIcon 返回托盘图标字节（多尺寸 ICO，16/20/24/32/48 帧）。
func trayIcon() []byte { return trayIconICO }

// releasesURL 是升级菜单项的落地页（OnUpdateInstall 未装配时的回退）。
const releasesURL = "https://github.com/listlonely/Yardmaster/releases"

// openBrowser 调系统默认浏览器打开控制台（exec 纯 Go，无 cgo）。
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// TrayOptions 是托盘菜单的动态装配项（nil 字段=隐藏对应信息项）。
type TrayOptions struct {
	OnOpen func() // 点击「打开窗口」
	OnExit func() // 点击「退出」（调用方负责停网关并退出进程）

	// Status 返回状态摘要行文本（30s 刷新；禁用态展示）。
	Status func() string
	// Update 查询新版本：返回「升级到 v0.10.0」或 ""（15s 首查 + 每
	// 1h；查询失败静默隐藏）。
	Update func() string
	// OnUpdateInstall 点击升级菜单项的应用内安装回调（M11-W3）：非
	// nil=走自替换引擎（下载→换侧→接力重启），nil=回退打开 releases
	// 页（无桌面壳装配的场景）。
	OnUpdateInstall func() error
}

// runTrayIsolated 托盘崩溃隔离（M8-W5，语义同插件纪律）：托盘在独立
// goroutine 运行，任何 panic 不带倒网关进程——recover 后网关继续运行
// （失去托盘入口，Ctrl-C/信号退出），stderr 留痕可观测。
func runTrayIsolated(opts TrayOptions) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("托盘崩溃（已隔离，网关继续运行；Ctrl-C 退出）", "panic", r)
		}
	}()
	runTray(opts)
}

// runTray 启动托盘（阻塞 systray.Run 内部循环，调用方以 goroutine 包装）。
func runTray(opts TrayOptions) {
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTitle("Yardmaster")
		systray.SetTooltip("Yardmaster 网关运行中")
		mOpen := systray.AddMenuItem("打开窗口", "打开 Yardmaster 应用窗口/控制台")
		systray.AddSeparator()
		verLabel := "v" + Version
		if Version == "dev" {
			verLabel = "dev（开发态）" // 开发构建无版本号，避免 "vdev"
		}
		mVer := systray.AddMenuItem(verLabel, "当前版本（发布页可升级）")
		mVer.Disable()
		mUpdate := systray.AddMenuItem("检查更新…", "查询 GitHub 最新发布")
		mUpdate.Hide()
		mStatus := systray.AddMenuItem("", "网关运行状态摘要")
		mStatus.Disable()
		if opts.Status == nil {
			mStatus.Hide()
		}
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "停止网关并退出")

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					if opts.OnOpen != nil {
						opts.OnOpen()
					}
				case <-mUpdate.ClickedCh:
					if opts.OnUpdateInstall == nil {
						_ = openBrowser(releasesURL)
						continue
					}
					// 应用内安装（M11-W3）：菜单转「更新中…」禁用态；
					// 失败恢复可重试，成功后网关重启托盘随之消失。
					mUpdate.SetTitle("更新中…")
					mUpdate.Disable()
					go func() {
						if err := opts.OnUpdateInstall(); err != nil {
							slog.Warn("托盘触发更新失败", "err", err)
							mUpdate.SetTitle("更新失败（点击重试，详情见日志）")
							mUpdate.Enable()
							return
						}
						mUpdate.SetTitle("已更新，正在重启…")
					}()
				case <-mQuit.ClickedCh:
					if opts.OnExit != nil {
						opts.OnExit()
					}
					return
				}
			}
		}()

		// 动态刷新：摘要 30s；升级查询 15s 首查 + 每 1h（失败静默）。
		go func() {
			checkUpdate := func() {
				if opts.Update == nil {
					return
				}
				if label := opts.Update(); label != "" {
					mUpdate.SetTitle(label)
					mUpdate.Show()
				} else {
					mUpdate.Hide()
				}
			}
			refreshStatus := func() {
				if opts.Status == nil {
					return
				}
				if s := opts.Status(); s != "" {
					mStatus.SetTitle(s)
				}
			}
			time.AfterFunc(15*time.Second, checkUpdate)
			tickStatus := time.NewTicker(30 * time.Second)
			defer tickStatus.Stop()
			tickUpdate := time.NewTicker(time.Hour)
			defer tickUpdate.Stop()
			for {
				select {
				case <-tickStatus.C:
					refreshStatus()
				case <-tickUpdate.C:
					checkUpdate()
				}
			}
		}()
	}, func() {})
}
