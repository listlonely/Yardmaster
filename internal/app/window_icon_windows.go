//go:build windows && amd64

// 原生窗口图标补设（2026-11-09 用户反馈：标题栏左上角为系统默认图标）：
// wails v3 窗口创建只发 WM_SETICON ICON_BIG（大图标，任务栏/Alt-Tab 用），
// 而窗口类的 IconSm 注册为系统默认 IDI_APPLICATION（application_windows.go
// init）——标题栏小图标解析顺序 WM_GETICON(ICON_SMALL) → 类 IconSm →
// 大图标缩放，类 IconSm 非零直接命中默认图标。本文件从 exe 资源（编号 3
// 的图标组，genwinres 按 wails v3 约定同编号嵌入）按系统小图标尺寸加载
// 并补发 ICON_SMALL，同时以标准尺寸重发 ICON_BIG。仅 window_wails.go
// 壳层在 RuntimeReady 主线程回调中调用。
package app

import (
	"log/slog"
	"syscall"
)

// w32 常量：WM_SETICON 的 wParam 取值、LoadImageW 的类型与系统指标索引。
const (
	wmSetIcon  = 0x0080
	iconSmall  = 0
	iconBig    = 1
	imageIcon  = 1
	smCxIcon   = 11
	smCyIcon   = 12
	smCxSmIcon = 49
	smCySmIcon = 50
)

// appIconResID 是 exe 内图标组的资源编号（wails v3 约定：
// webview_window_windows.go "App icon ID is 3"，对话框同源）。
const appIconResID = 3

var (
	procLoadImage         = syscall.NewLazyDLL("user32.dll").NewProc("LoadImageW")
	procSendMessageIcon   = syscall.NewLazyDLL("user32.dll").NewProc("SendMessageW")
	procGetSystemMetrics  = syscall.NewLazyDLL("user32.dll").NewProc("GetSystemMetrics")
	procGetModuleHandle   = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

// setWindowIcons 从 exe 资源加载图标组并补发 WM_SETICON（小图标按
// SM_CXSMICON 系统尺寸必设；大图标按 SM_CXICON 重发，覆盖 wails 默认
// 尺寸）。hwnd 非法或加载失败静默返回——图标属展示增强，绝不影响
// 窗口与网关。
func setWindowIcons(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	hInst, _, _ := procGetModuleHandle.Call(0)
	cxSm, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cySm, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	cx, _, _ := procGetSystemMetrics.Call(smCxIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCyIcon)
	small := loadAppIcon(hInst, cxSm, cySm)
	big := loadAppIcon(hInst, cx, cy)
	slog.Info("窗口图标补设", "hwnd", hwnd, "small", small, "big", big)
	if small != 0 {
		procSendMessageIcon.Call(hwnd, wmSetIcon, iconSmall, small)
	}
	if big != 0 {
		procSendMessageIcon.Call(hwnd, wmSetIcon, iconBig, big)
	}
}

// loadAppIcon 按指定像素尺寸加载编号 appIconResID 的图标帧（近邻帧
// 缩放，LR_DEFAULTCOLOR）。
func loadAppIcon(hInst, cx, cy uintptr) uintptr {
	h, _, _ := procLoadImage.Call(hInst, uintptr(appIconResID), imageIcon, cx, cy, 0)
	return h
}
