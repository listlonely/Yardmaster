//go:build windows

// 壳层挂起探测的 Windows 实装（2026-11-25）：GetShellWindow 取壳层窗口
// （Explorer 的桌面宿主），两路信号判「进程存活但窗口挂起」：
//
//  1. IsHungAppWindow 快速路径——但系统仅在目标线程队列有滞留消息时才
//     置 hung 标志。本机对照实验（NtSuspendProcess 全挂起 Explorer 14s）
//     实证：挂起期间 IsHungAppWindow 恒 FALSE（桌面窗口无人发消息、队
//     列空闲）——单靠它漏报（v0.2.16 现场验证失败的根因）。
//  2. WM_NULL 探针兜底（任务管理器「未响应」同款判定法）：向壳窗口
//     SendMessageTimeout(WM_NULL, SMTO_ABORTIFHUNG, 1.5s)——健康线程
//     毫秒级应答返回非零；挂起线程无法处理、超时返回零。实验数据：
//     健康 rc=1 / 挂起 8s rc=0 / 挂起 14s rc=0 / 恢复 rc=1，四态全对。
//
// 均为纯查询 API（自身无消息泵依赖），可安全跑在任意 goroutine；探针
// 最坏阻塞 1.5s（超时上限，SMTO_ABORTIFHUNG 对已标记窗口提前折返），
// 看门狗 15s tick 节奏可容忍。
package app

import (
	"syscall"
	"unsafe"
)

var (
	procGetShellWindow      = syscall.NewLazyDLL("user32.dll").NewProc("GetShellWindow")
	procIsHungAppWindow     = syscall.NewLazyDLL("user32.dll").NewProc("IsHungAppWindow")
	procSendMessageTimeoutW = syscall.NewLazyDLL("user32.dll").NewProc("SendMessageTimeoutW")
)

// 探针参数：SMTO_ABORTIFHUNG 标志、WM_NULL 消息号、探针超时毫秒数。
const (
	smtoAbortIfHung = 0x0002
	wmNull          = 0x0000
	probeTimeoutMs  = 1500
)

// shellHungReport 单次探测：壳窗口存在且（hung 标志置位 或 WM_NULL 探针
// 超时）=shellHung；探针正常应答=shellOK；取不到壳窗口（Explorer 尚未
// 启动/真崩溃退出中）=shellUnknown（不计不清——真死场景由 systray
// TaskbarCreated 自愈，看门狗不抢活）。
func shellHungReport() shellHealth {
	hwnd, _, _ := procGetShellWindow.Call()
	if hwnd == 0 {
		return shellUnknown
	}
	if hung, _, _ := procIsHungAppWindow.Call(hwnd); hung != 0 {
		return shellHung
	}
	var result uintptr
	ok, _, _ := procSendMessageTimeoutW.Call(hwnd, wmNull, 0, 0,
		smtoAbortIfHung, probeTimeoutMs, uintptr(unsafe.Pointer(&result)))
	if ok != 0 {
		return shellOK
	}
	return shellHung
}
