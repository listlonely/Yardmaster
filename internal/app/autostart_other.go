// 非 Windows 平台的开机自启动 no-op：设置项可存（settings.json 跨平台
// 同构），壳层不落地（Linux 桌面壳为浏览器回退方案，无 wails；若未来
// 需要，可扩展 XDG autostart ~/.config/autostart/*.desktop）。
//
//go:build !windows

package app

// applyAutostart 非 Windows 平台 no-op。
func applyAutostart(bool) {}

// autostartValueExists 非 Windows 无 Run 键可查（未落地壳层偏好的对称
// 探测桩）：恒 true=视为存在，看门狗周期校验永不补写。
func autostartValueExists() bool { return true }
