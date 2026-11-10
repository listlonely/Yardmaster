//go:build !windows

package app

import (
	"fmt"
	"syscall"
	"time"
)

// waitForPID 轮询等待 pid 退出（Unix 无跨进程句柄等待的通用原语；
// 100ms 轮询足够——接力等待只发生一次）。超时返回错误。
func waitForPID(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !pidAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 pid %d 退出超时（%s）", pid, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pidAlive 报告 pid 是否存活：signal 0 探测（ESRCH=不存在；EPERM=存在
// 但属其他用户）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
