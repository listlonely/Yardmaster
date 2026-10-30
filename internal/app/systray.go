// 系统托盘常驻（M3-W6，桌面壳两步走第一步）：纯 Go 托盘
// （fyne.io/systray：Windows Win32 syscall / Linux D-Bus SNI，无 cgo，
// 不动交叉编译铁律）。菜单：打开控制台 / 退出。真原生窗口（Wails v3）
// 排 M4 的独立 gui 构建变体，网关核心不引入 webview 依赖。
package app

import (
	"bytes"
	"image"
	"image/draw"
	"image/color"
	"image/png"
	"os/exec"
	"runtime"

	"fyne.io/systray"
)

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

// runTray 启动托盘（阻塞 systray.Run 内部循环，调用方以 goroutine 包装）。
// onExit 在用户点击退出时回调（调用方负责停止网关并退出进程）。
func runTray(url string, onExit func()) {
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTitle("Yardmaster")
		systray.SetTooltip("Yardmaster 网关运行中 · " + url)
		mOpen := systray.AddMenuItem("打开控制台", "在浏览器打开管理控制台")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "停止网关并退出")
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					_ = openBrowser(url)
				case <-mQuit.ClickedCh:
					onExit()
					return
				}
			}
		}()
	}, func() {})
}
