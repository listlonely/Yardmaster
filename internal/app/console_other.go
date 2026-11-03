// 非 Windows 平台无控制台收起概念（黑窗问题仅存在于 Windows console
// subsystem 的 conhost）：no-op，runApp 调用点签名统一。
//
//go:build !windows

package app

func hideConsoleIfDetached() bool { return false }

func setupAppLog(dir string) { _ = dir }
