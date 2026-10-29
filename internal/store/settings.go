// 用户设置（settings.json）：W3 仅含全局代理；后续扩展自启、语言等。
package store

// Settings 是用户级设置。
type Settings struct {
	GlobalProxy string `json:"global_proxy,omitempty"` // 空=跟随系统代理；direct=显式直连（F3 继承链末端）

	// L1 分类器（PRD F8）：模型名为 provider/model 或 group/<id>；空=关闭
	// L1（仅 L0）。超时毫秒数 0=默认 300ms（W5 简化实现）。
	L1Classifier string `json:"l1_classifier,omitempty"`
	L1TimeoutMS  int    `json:"l1_timeout_ms,omitempty"`
}

// LoadSettings 读取设置；文件不存在返回 os.ErrNotExist（调用方按零值
// 配置处理），解析失败返回带路径的错误（调用方必须 fail-closed）。
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
