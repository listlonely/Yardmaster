// 非 Windows 平台插件 spawn 无需窗口抑制（无 conhost 概念；默认属性
// 即可）。
//
//go:build !windows

package plugin

import "syscall"

// spawnSysProcAttr 返回 nil（默认进程属性）。
func spawnSysProcAttr() *syscall.SysProcAttr { return nil }
