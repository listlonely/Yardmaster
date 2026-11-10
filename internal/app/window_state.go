// 窗口状态记忆（M8-W3）：尺寸/位置/最大化持久化到数据目录
// window-state.json。壳层状态不进 settings.json——它是查看端设备偏好
// 而非网关配置（局域网多客户端共享语义错误），也不需要走配置写闸；
// 读不懂=缺省（fail-soft），写失败仅日志不阻塞。保存时机：关窗隐藏前
// 与退出前（移动后强杀不保存的场景如实留档）。
//
//go:build windows

package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// windowState 持久化形态（Bounds 为逻辑坐标，DPI 无关）。
type windowState struct {
	X   int  `json:"x"`
	Y   int  `json:"y"`
	W   int  `json:"w"`
	H   int  `json:"h"`
	Max bool `json:"max"`
}

func loadWindowState(dir string) *windowState {
	b, err := os.ReadFile(filepath.Join(dir, "window-state.json"))
	if err != nil {
		return nil
	}
	var s windowState
	if json.Unmarshal(b, &s) != nil || s.W < 400 || s.H < 300 || s.W > 20000 || s.H > 20000 {
		return nil // 读不懂=缺省（fail-soft）
	}
	return &s
}

func saveWindowState(dir string, s *windowState) {
	if s == nil || s.W <= 0 || s.H <= 0 {
		return
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "window-state.json"), b, 0o600); err != nil {
		slog.Warn("窗口状态落盘失败", "err", err)
	}
}

// systemPrefersLight 读系统应用深浅色偏好（AppsUseLightTheme，0=暗）；
// 读不懂=亮（缺省）。用于 WebView2 窗口初始背景色联动（防主题不符闪色）。
func systemPrefersLight() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	_ = k.Close()
	if err != nil {
		return true
	}
	return v != 0
}
