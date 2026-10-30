// Package provider 定义上游模型服务的领域模型：Provider、账号、路由组
// 与候选休眠（PRD F2、F6）。状态字典与 PRD「规则」页一一对应，禁止在
// 其他包另立状态定义。
package provider

import (
	"fmt"

	"yardmaster/internal/netproxy"
)

// Protocol 是网关与上游通信的 API 协议。M1 仅 Chat 与 Messages（设计文档 §6.2）。
type Protocol string

const (
	ProtocolChat     Protocol = "openai-chat"        // OpenAI Chat Completions
	ProtocolMessages Protocol = "anthropic-messages" // Anthropic Messages
)

// ProviderStatus 对应 PRD 规则页「Provider 状态」。
type ProviderStatus string

const (
	StatusOK       ProviderStatus = "ok"       // 正常：预检通过
	StatusPrefail  ProviderStatus = "prefail"  // 预检失败（仍可参与路由，持续红色提示）
	StatusDisabled ProviderStatus = "disabled" // 已停用：不参与候选计划
)

// AccountStatus 账号级状态（PRD 规则页）。休眠由网关写入与解除，
// 用户可在管理页手动「立即恢复」（F6）。
type AccountStatus string

const (
	AccountAvailable AccountStatus = "available"      // 可用（预检通过或未测）
	AccountPrefail   AccountStatus = "prefail"        // 预检失败：仍可参与路由，持续红色提示（F3）
	AccountResting   AccountStatus = "resting"        // 休眠至 T：429/额度/5xx 后暂时退出候选
	AccountBadCred   AccountStatus = "bad-credential" // 凭证失败：切走但标记需人工处理，不自动重试
)

// Provider 是一个上游模型服务商。
type Provider struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Protocol Protocol     `json:"protocol"`
	BaseURL  string       `json:"base_url"`
	Proxy    netproxy.Ref `json:"proxy,omitempty"` // 空=继承全局；direct=显式直连（F3）
	Disabled bool         `json:"disabled,omitempty"`
	Models   []Model      `json:"models,omitempty"` // 预检拉取的模型清单（F2）
	Accounts []Account    `json:"accounts"`
}

// AuthKind 是账号的认证形态（M3-W1）。零值为 api_key，兼容既有配置。
type AuthKind string

const (
	AuthAPIKey AuthKind = ""      // 静态 API key（存 providers.json，现状行为）
	AuthOAuth  AuthKind = "oauth" // OAuth 订阅凭证：密文存凭证库（internal/credential），
	// providers.json 只存非密钥元数据；与 api_key 互斥。
)

// OAuth 账号状态字典（M3-W1；W3 刷新机制扩展写路径）。
const (
	OAuthStateOK      = "ok"      // 凭证有效（含未到期的导入态）
	OAuthStateStale   = "stale"   // access token 已过期，等待刷新（W3 处理）
	OAuthStateUnknown = "unknown" // 上游未提供过期时间
)

// OAuthMeta 是 OAuth 账号在 providers.json 中的非密钥元数据。
// access/refresh token 等秘密只存 internal/credential 管理的凭证库
// （Windows DPAPI / 其余 0600），永不进入 providers.json 及其快照。
type OAuthMeta struct {
	ExpiresAt   int64  `json:"expires_at,omitempty"`   // access token 过期（Unix 秒；0=未知）
	RefreshedAt int64  `json:"refreshed_at,omitempty"` // 最近一次导入/刷新（Unix 秒）
	State       string `json:"state,omitempty"`        // ok | stale | unknown
	LastError   string `json:"last_error,omitempty"`   // 脱敏摘要（不含 token）
}

// Account 是 Provider 下的一个 key/账号，可配独立代理（F3）。
// APIKey 仅存本机（F10）；任何日志/界面输出必须经 netproxy.Redact（§4.4）。
type Account struct {
	Name         string        `json:"name"`
	APIKey       string        `json:"api_key"`
	Proxy        netproxy.Ref  `json:"proxy,omitempty"` // 空=继承 Provider
	Status       AccountStatus `json:"status"`
	RestingUntil int64         `json:"resting_until,omitempty"` // Unix 秒；休眠至 T
	Auth         AuthKind      `json:"auth,omitempty"`          // 空=api_key；oauth=订阅凭证（M3-W1）
	OAuth        *OAuthMeta    `json:"oauth,omitempty"`         // auth=oauth 时的非密钥元数据

	// 当日用量（W6 由响应 usage 填充）：smart 策略按剩余比例排序。
	// QuotaTokensDay<=0 视为无数据（smart 回落 rotate）。
	// UsageDay 是 UsedTokens 的归属日期（"2006-01-02"），跨日自动重置。
	UsedTokens     int64  `json:"used_tokens,omitempty"`
	QuotaTokensDay int64  `json:"quota_tokens_day,omitempty"`
	UsageDay       string `json:"usage_day,omitempty"`
}

// Validate 校验账号认证形态一致性（配置矛盾即拒绝，fail-closed）：
// auth=oauth 与 api_key 互斥；auth 为空时不得携带 oauth 元数据；
// 未知 auth 形态拒绝（读不懂=未知，不放行）。
func (a Account) Validate() error {
	switch a.Auth {
	case AuthAPIKey:
		if a.OAuth != nil {
			return fmt.Errorf("账号 %q: auth=api_key 时不应有 oauth 元数据", a.Name)
		}
	case AuthOAuth:
		if a.APIKey != "" {
			return fmt.Errorf("账号 %q: oauth 与 api_key 互斥（OAuth token 不存 providers.json）", a.Name)
		}
	default:
		return fmt.Errorf("账号 %q: 未知 auth 形态 %q（空=api_key | oauth）", a.Name, a.Auth)
	}
	return nil
}

// Strategy 是路由组策略名（PRD F6）。策略实现在 internal/routes。
type Strategy string

const (
	StrategyOrder  Strategy = "order"  // 固定顺序，用尽再换
	StrategyRotate Strategy = "rotate" // 每轮换下一成员
	StrategySmart  Strategy = "smart"  // 优先额度窗口最快重置、浪费最少的账号
	StrategyUsage  Strategy = "usage"  // 当日用量升序（负载均衡；无需额度配置）
	StrategyWeight Strategy = "weight" // 加权轮询：按 Group.Weights 分流
)

// Group 把「成员模型 × 账号池」组成一个可被引用的虚拟模型（F6）。
// Agent 只看到一个名字（group/<id>），切换发生在网关内部。
type Group struct {
	ID       string         `json:"id"`
	Strategy Strategy       `json:"strategy"`
	Members  []string       `json:"members"`           // 形如 "provider/model"
	Weights  map[string]int `json:"weights,omitempty"` // weight 策略：成员引用 → 权重（缺省 1）
}
