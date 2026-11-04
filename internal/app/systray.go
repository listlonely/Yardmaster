// 系统托盘常驻（M3-W6，桌面壳两步走第一步）：纯 Go 托盘
// （fyne.io/systray：Windows Win32 syscall / Linux D-Bus SNI，无 cgo，
// 不动交叉编译铁律）。
//
// M9-W4 动态化（Magpie traymenu 同构——原生菜单非用户可配置，动态性
// 体现在版本行/升级入口/状态摘要的运行时改标题）：固定项=打开窗口/退出；
// 信息项=版本（禁用态）、「升级到 x.y.z」（有新版才显示，点击开
// releases 页）、状态摘要（休眠/派发进行中，30s 刷新）。i18n 不做
// （M8 留档不采纳延续）。
package app

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"os/exec"
	"runtime"
	"time"

	"fyne.io/systray"
)

// releasesURL 是升级菜单项的落地页。
const releasesURL = "https://github.com/listlonely/Yardmaster/releases"

// trayIcon 代码生成 32x32 PNG 图标（深蓝底 + 白色轨岔斜线，调车长意象），
// 无需外部资源文件。
func trayIcon() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	bg := color.RGBA{R: 0x1e, G: 0x24, B: 0x30, A: 0xff}
	track := color.RGBA{R: 0xe0, G: 0xe7, B: 0xff, A: 0xff}
	accent := color.RGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	// 两条平行横轨（y=20 / y=24）
	for x := 4; x < 28; x++ {
		img.Set(x, 20, track)
		img.Set(x, 24, track)
	}
	// 岔轨斜线（从右上引向 x=14 的横轨）
	for i := 0; i < 12; i++ {
		img.Set(16+i, 19-i, accent)
		img.Set(17+i, 19-i, accent)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

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
