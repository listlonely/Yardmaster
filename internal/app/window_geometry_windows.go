//go:build windows && amd64

// 假最大化几何计算（window_wails.go 窗口状态恢复 M8-W3 用）。
// 2026-11-20 从 webview_sync_windows.go 迁出——窄条真根因定为前端 CSS
// 类名撞名（gateway/web/app.js is-win 留档取证链），WebView2 渲染面伺
// 服体系整体退役，仅此几何函数被窗口状态恢复继续使用。
package app

import "syscall"

var (
	procGetSysMetGeo = syscall.NewLazyDLL("user32.dll").NewProc("GetSystemMetrics")
	procGetDPIGeo    = syscall.NewLazyDLL("user32.dll").NewProc("GetDpiForSystem")
)

// SM_ 索引：主屏宽 / 工作区高（屏高-任务栏）/ 可调边框宽。
const (
	smCXScreenGeo  = 0
	smCYFullScrGeo = 17
	smCXFrameGeo   = 32
)

// fakeMaxRectLogical 计算模拟最大化的窗口几何（逻辑 DIP，与 wails
// options/Bounds 同语义）。复刻真实最大化 rect 形状（本机 100% DPI 实
// 测：x=-边框、y=0、宽=屏宽+2×边框、高=工作区高+边框——左右下三边
// 出屏悬挂，标题栏完整可见）。物理量取自 GetSystemMetrics（进程 DPI
// 感知上下文与 GetDpiForSystem 自洽：感知进程两者皆物理，非感知进程
// 两者皆已虚拟化为逻辑），逻辑=物理/DPI 缩放。
// 假设（本机成立）：任务栏贴底、主屏为唯一显示器；多显示器场景假最
// 大化固定落主屏（调用方留档此限制）。
func fakeMaxRectLogical() (x, y, w, h int) {
	cx, _, _ := procGetSysMetGeo.Call(smCXScreenGeo)
	cyFS, _, _ := procGetSysMetGeo.Call(smCYFullScrGeo)
	frame, _, _ := procGetSysMetGeo.Call(smCXFrameGeo)
	dpi, _, _ := procGetDPIGeo.Call()
	if dpi == 0 {
		dpi = 96
	}
	sc := float64(dpi) / 96
	lg := func(v uintptr) int { return int(float64(v)/sc + 0.5) }
	return -lg(frame), 0, lg(cx) + 2*lg(frame), lg(cyFS) + lg(frame)
}
