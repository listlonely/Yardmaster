// 厂商预设模板（M3-W4）：常见上游的 base_url / 协议开箱即用。
// 端点取自各厂商公开文档；GLM Coding Plan 的专用端点形状对齐 Magpie
// （chat 在 /api/coding/paas/v4、Anthropic 协议在 /api/anthropic）。
// 预检会拉取真实模型清单，此处不维护模型列表。
package provider

// Preset 是一个厂商接入预设。
type Preset struct {
	ID         string      `json:"id"`   // 预设 ID（provider add -preset）
	Name       string      `json:"name"` // 默认显示名
	Protocol   Protocol    `json:"protocol"`
	BaseURL    string      `json:"base_url"` // 按 Protocol 的上游前缀（网关追加 /chat/completions 或 /v1/messages）
	Note       string      `json:"note,omitempty"`
	QuotaProbe *QuotaProbe `json:"quota_probe,omitempty"` // 余额轮询端点（M3-W5）
}

// Presets 是内置预设表（ID 唯一；后缀 -messages 表示 Anthropic 协议形态）。
// QuotaProbe：Coding Plan 用量端点 /api/monitor/usage/quota/limit 返回
// limits[] 双窗口（5小时+周），由网关 extractPlanQuota 归一（M9-W3 真机
// 采样）；按量厂商用余额端点（Zhipu finance/balance、SiliconFlow
// user/info 的 "¥xx.xx" 字符串余额）。
var Presets = []Preset{
	{ID: "glm-coding", Name: "智谱 GLM Coding Plan", Protocol: ProtocolChat,
		BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4", Note: "Coding Plan key 专用；按量 key 请用 glm",
		QuotaProbe: &QuotaProbe{URL: "https://open.bigmodel.cn/api/monitor/usage/quota/limit"}},
	{ID: "glm-coding-messages", Name: "智谱 GLM Coding Plan（Anthropic 协议）", Protocol: ProtocolMessages,
		BaseURL: "https://open.bigmodel.cn/api/anthropic",
		QuotaProbe: &QuotaProbe{URL: "https://open.bigmodel.cn/api/monitor/usage/quota/limit"}},
	{ID: "glm", Name: "智谱开放平台（按量计费）", Protocol: ProtocolChat,
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Note: "Coding Plan key 会被 1113 拒绝并自动纠偏到 coding 端点",
		QuotaProbe: &QuotaProbe{URL: "https://open.bigmodel.cn/api/paas/v4/finance/balance"}},
	{ID: "zai-coding", Name: "Z.ai GLM Coding Plan", Protocol: ProtocolChat,
		BaseURL: "https://api.z.ai/api/coding/paas/v4",
		QuotaProbe: &QuotaProbe{URL: "https://api.z.ai/api/monitor/usage/quota/limit"}},
	{ID: "zai-coding-messages", Name: "Z.ai GLM Coding Plan（Anthropic 协议）", Protocol: ProtocolMessages,
		BaseURL: "https://api.z.ai/api/anthropic",
		QuotaProbe: &QuotaProbe{URL: "https://api.z.ai/api/monitor/usage/quota/limit"}},
	{ID: "zai", Name: "Z.ai 开放平台（按量计费）", Protocol: ProtocolChat,
		BaseURL:    "https://api.z.ai/api/paas/v4",
		QuotaProbe: &QuotaProbe{URL: "https://api.z.ai/api/paas/v4/finance/balance"}},
	{ID: "deepseek", Name: "DeepSeek（OpenAI 协议）", Protocol: ProtocolChat,
		BaseURL: "https://api.deepseek.com"},
	{ID: "deepseek-messages", Name: "DeepSeek（Anthropic 协议）", Protocol: ProtocolMessages,
		BaseURL: "https://api.deepseek.com/anthropic"},
	{ID: "kimi", Name: "Kimi/Moonshot（OpenAI 协议）", Protocol: ProtocolChat,
		BaseURL: "https://api.moonshot.cn/v1"},
	{ID: "kimi-messages", Name: "Kimi/Moonshot（Anthropic 协议）", Protocol: ProtocolMessages,
		BaseURL: "https://api.moonshot.cn/anthropic"},
	{ID: "siliconflow", Name: "SiliconFlow 硅基流动", Protocol: ProtocolChat,
		BaseURL: "https://api.siliconflow.cn/v1",
		// /v1/user/info 已被厂商停服（2026-08-14，HTTP 410 code 20092），
		// 官方替代 API 尚未上线。保留端点占位：探测侧按 410 识别为
		// 「厂商停服」中性提示而非失败告警；官方出新端点后此处跟进替换。
		QuotaProbe: &QuotaProbe{URL: "https://api.siliconflow.cn/v1/user/info"}},
	{ID: "anthropic", Name: "Anthropic 官方", Protocol: ProtocolMessages,
		BaseURL: "https://api.anthropic.com"},
	{ID: "openai", Name: "OpenAI 官方", Protocol: ProtocolChat,
		BaseURL: "https://api.openai.com/v1"},
	{ID: "gemini", Name: "Google Gemini 官方", Protocol: ProtocolGemini,
		BaseURL: "https://generativelanguage.googleapis.com",
		Note:    "AI Studio API key；预检经 /v1beta/models 自动拉取对话模型清单"},
}

// PresetByID 查找预设。
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}
