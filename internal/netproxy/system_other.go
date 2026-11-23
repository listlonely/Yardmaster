//go:build !windows

// 非 Windows：无 WinINET 系统代理概念（Linux/macOS 桌面代理多走
// 环境变量，默认 Transport 的 ProxyFromEnvironment 已兜底）。
package netproxy

// SystemProxy 非 Windows 恒返回未启用（调用方回退直连）。
func SystemProxy() (Ref, bool) { return "", false }
