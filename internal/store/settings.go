// 用户设置（settings.json）：全局代理、L1 分类器、监听与访问令牌。
package store

// Settings 是用户级设置。
type Settings struct {
	GlobalProxy string `json:"global_proxy,omitempty"` // 空=跟随系统代理；direct=显式直连（F3 继承链末端）

	// L1 分类器（PRD F8）：模型名为 provider/model 或 group/<id>；空=关闭
	// L1（仅 L0）。超时毫秒数 0=默认 300ms（W5 简化实现）。
	L1Classifier string `json:"l1_classifier,omitempty"`
	L1TimeoutMS  int    `json:"l1_timeout_ms,omitempty"`

	// 监听地址（M2-W5）：空=127.0.0.1:8317（F10 安全基线：默认仅回环）。
	// 绑定非回环（局域网共享）时 Token 必填（ValidateSecurity 强制）。
	Listen string `json:"listen,omitempty"`

	// 访问令牌（M2-W5）：非空时 /v1/* 与 /api/* 要求匹配（Authorization
	// Bearer、x-api-key 或 X-Gw-Token 均可）；仅本机使用可留空。
	// 任何输出必须脱敏（§4.4，与 api_key 同规）。
	Token string `json:"token,omitempty"`

	// Gemini CLI OAuth client secret（M3-W3）：Google installed-app 客户端
	// 的公开常量，但 GitHub secret scanning 拦截仓库内字面量，故经设置
	// 注入（取值见 Gemini CLI 自身源码）；空=gemini-cli 凭证无法刷新。
	GeminiClientSecret string `json:"gemini_client_secret,omitempty"`

	// OTLP traces 导出（M4-W5）：默认关。Enabled 且 Endpoint 非空才装配
	// 导出器（otlphttp JSON，OTel 兼容后端）；失败只降级不阻断主流程。
	OTel OTelSettings `json:"otel,omitempty"`

	// 美元→人民币汇率（M7-W1 增量）：>0 时成本报告/单价表显示 ≈¥ 换算
	// 列；0=仅美元。纯展示换算，不参与计费（计费恒以美元单价计算）。
	FxUsdCNY float64 `json:"fx_usd_cny,omitempty"`

	// 桌面气泡通知（M9-W4）：派发终态/上游账号休眠时弹系统通知。
	// 指针语义：nil（配置缺省/未提供）=开启——桌面壳默认有通知，显式
	// false 才关闭；指针而非 bool 是为管理 API PUT 的空值保留语义
	//（nil=未提供保留现值，与 otel 等字段的保留逻辑同规）。
	Notify *bool `json:"notify,omitempty"`
}

// NotifyEnabled 返回桌面气泡通知是否开启（nil=缺省开启）。
func (s Settings) NotifyEnabled() bool {
	return s.Notify == nil || *s.Notify
}

// OTelSettings 是 OTLP 导出配置。
type OTelSettings struct {
	Enabled  bool              `json:"enabled,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"` // 基础端点，自动追加 /v1/traces
	Headers  map[string]string `json:"headers,omitempty"`  // 附加头（如 Authorization）
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
