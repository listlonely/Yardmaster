// 用户设置（settings.json）：W3 仅含全局代理；后续扩展自启、语言等。
package store

// Settings 是用户级设置。
type Settings struct {
	GlobalProxy string `json:"global_proxy,omitempty"` // 空=跟随系统代理；direct=显式直连（F3 继承链末端）
}

// LoadSettings 读取设置；文件不存在返回零值配置。
func LoadSettings(p Paths) (Settings, error) {
	var s Settings
	err := LoadJSON(p.SettingsPath(), &s)
	if err != nil {
		return Settings{}, err
	}
	return s, nil
}

// SaveSettings 原子写入设置。
func SaveSettings(p Paths, s Settings) error {
	return SaveJSON(p.SettingsPath(), s)
}
