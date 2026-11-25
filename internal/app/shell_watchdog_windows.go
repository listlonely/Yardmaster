//go:build windows

// 壳层挂起探测的 Windows 实装（2026-11-25）：GetShellWindow 取壳层窗口
// （Explorer 的桌面/任务栏宿主），IsHungAppWindow 判定其是否超过系统
// 阈值（5 秒）未处理消息——正是「任务栏点不动」的病态。两者均为纯查询
// API（自身无消息泵依赖），可安全跑在任意 goroutine。syscall 先例同
// window_icon_windows.go。
package app

import "syscall"

var (
	procGetShellWindow  = syscall.NewLazyDLL("user32.dll").NewProc("GetShellWindow")
	procIsHungAppWindow = syscall.NewLazyDLL("user32.dll").NewProc("IsHungAppWindow")
)

// shellHungReport 单次探测：壳窗口存在且挂起=shellHung；健康=shellOK；
// 取不到壳窗口（Explorer 尚未启动/真崩溃退出中）=shellUnknown（不计
// 不清——真死场景由 systray TaskbarCreated 自愈，看门狗不抢活）。
func shellHungReport() shellHealth {
	hwnd, _, _ := procGetShellWindow.Call()
	if hwnd == 0 {
		return shellUnknown
	}
	hung, _, _ := procIsHungAppWindow.Call(hwnd)
	if hung != 0 {
		return shellHung
	}
	return shellOK
}
