//go:build windows

package app

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// waitForPID 阻塞等待 pid 退出（WaitForSingleObject；超时返回错误）。
// 进程不存在视为已退出（OpenProcess 失败统一按已退出——误判只会让
// serve bind 失败走回滚，不放大风险）。注：x/sys/windows 本版本
// WaitForSingleObject 事件码返回 syscall.Errno，WAIT_OBJECT_0/
// WAIT_TIMEOUT 常量为 uint32 类型不可直接 switch——用数值常量。
func waitForPID(pid int, timeout time.Duration) error {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil // 已退出（或不可访问——按已退出处理）
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ms := uint32(timeout / time.Millisecond)
	if ms == 0 {
		ms = 1
	}
	switch ev, werr := windows.WaitForSingleObject(h, ms); ev {
	case 0: // WAIT_OBJECT_0：已退出
		return nil
	case 258: // WAIT_TIMEOUT
		return fmt.Errorf("等待 pid %d 退出超时（%s）", pid, timeout)
	default:
		return fmt.Errorf("等待 pid %d: %w（事件码 %d）", pid, werr, ev)
	}
}

// pidAlive 报告 pid 是否存活（锁残留复核用）：OpenProcess 成功且零超时
// 等待未 signaled=活。规避 GetExitCodeProcess 的 STILL_ACTIVE(259)
// 歧义（真实退出码恰为 259 时误判）。
func pidAlive(pid int) bool {
	const queryLimited = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|queryLimited, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == 258 // WAIT_TIMEOUT=尚未 signaled=存活
}
