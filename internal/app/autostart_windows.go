// Windows 开机自启动（2026-11-11）：settings.autostart 偏好的进程外
// 落地——HKCU 注册表 Run 键（登录时拉起当前 exe；GUI 模式下即
// yardmaster-app.exe，与双击入口一致）。HKCU 无需管理员权限；键值带
// 引号防路径空格。写/删失败仅降级留痕（壳层纪律：不阻断网关启动），
// 下次设置变更或启动自愈会重试。
//
//go:build windows

package app

import (
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/sys/windows/registry"
)

// autostartValueName 是 Run 键的值名（控制面板/注册表编辑器里显示的
// 条目名，与产品名一致便于用户辨认）。
const autostartValueName = "Yardmaster"

// autostartRunKeyPath 是 HKCU 下的当前用户登录项路径。
const autostartRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// applyAutostart 按偏好写/删注册表 Run 键（幂等；enable=true 确保存在，
// false 确保不存在）。
func applyAutostart(enable bool) {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("开机自启动同步失败（取可执行文件路径）", "err", err)
		return
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		slog.Warn("开机自启动同步失败（打开注册表 Run 键）", "err", err)
		return
	}
	defer key.Close()
	if enable {
		if err := key.SetStringValue(autostartValueName, fmt.Sprintf(`"%s"`, exe)); err != nil {
			slog.Warn("开机自启动同步失败（写入 Run 键）", "err", err)
			return
		}
		slog.Info("开机自启动已开启", "exe", exe)
		return
	}
	if err := key.DeleteValue(autostartValueName); err != nil && err != registry.ErrNotExist {
		// 键不存在=目标状态，不算失败；其余写失败留痕。
		slog.Warn("开机自启动同步失败（删除 Run 键）", "err", err)
		return
	}
	slog.Info("开机自启动已关闭")
}
