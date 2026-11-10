// 声明式 Agent 注册表（PRD F4）：每个 Agent 的配置位置、格式与要写的
// 字段全部数据化，接线引擎不感知具体 Agent。
//
// §4.2 铁律：字段名与取值必须来自该 Agent 自己的文档/实现。字段形状
// 经真实世界核实的参考实现交叉校准（Magpie agent-wiring codex.go/
// gemini.go，[2026-11-03-M9-W2]；cline 经官方源码 + docs.cline.bot +
// vercel 生产写入器三源核验，[2026-11-09-cline-spec]）。规格未核验的
// Agent 明确拒绝接线并说明原因——宁可少接一个，不写猜出来的键。
package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Op 是对目标配置的一次键写入；JSON 格式用 JSONPath，TOML 用表+键，
// env 用 EnvKey。Volatile 标记值随接线时刻变化的时间戳类键：漂移比对
// 跳过（写完即「漂移」的比对无意义），其余语义（写入/逆操作）不变。
type Op struct {
	Desc      string   `json:"desc"`
	JSONPath  []string `json:"json_path,omitempty"`
	TOMLTable []string `json:"toml_table,omitempty"`
	TOMLKey   string   `json:"toml_key,omitempty"`
	EnvKey    string   `json:"env_key,omitempty"`
	Value     any      `json:"value"`
	Volatile bool      `json:"volatile,omitempty"`
}

// Def 是一个受支持 Agent 的声明式描述。Extras 是随主文件一同接线的
// 附属文件（如 Gemini CLI 的 ~/.gemini/.env）：各自 Format/RelPaths/Ops，
// 与主文件同一份接线记录（Files 按序展开）。CLI 是该 Agent 的命令行
// 可执行名（存在性检测用）：配置文件未落盘但 CLI 已在 PATH 上（npm 全
// 局装 shim）时状态为「未接入」而非「未安装」（OBS-1）；空=仅按配置
// 文件判定。
type Def struct {
	ID       string // 稳定 ID；网关 key = gw-<ID>
	Name     string
	Format   Format
	RelPaths []string // 候选配置路径（支持 ~ 展开），按优先级
	CLI      string   // CLI 可执行名（PATH 存在性检测）；空=不检测
	SpecNote string   // 非空 = 写入规格待核验，拒绝接线（§4.2）
	Ops      func(gwAddr, key, model string) []Op
	Extras   []Def
}

// Registry 返回全部受支持 Agent（MVP 5 个）。
func Registry() []Def {
	return []Def{
		{
			ID: "claude-code", Name: "Claude Code", Format: FormatJSONC, CLI: "claude",
			RelPaths: []string{"~/.claude/settings.json"},
			Ops: func(addr, key, model string) []Op {
				return []Op{
					{Desc: "env.ANTHROPIC_BASE_URL → 网关地址", JSONPath: []string{"env", "ANTHROPIC_BASE_URL"}, Value: addr},
					{Desc: "env.ANTHROPIC_AUTH_TOKEN → 网关 key", JSONPath: []string{"env", "ANTHROPIC_AUTH_TOKEN"}, Value: key},
					{Desc: "model → 默认模型/路由组", JSONPath: []string{"model"}, Value: model},
				}
			},
		},
		{
			ID: "codex", Name: "Codex", Format: FormatTOML, CLI: "codex",
			RelPaths: []string{"~/.codex/config.toml"},
			// 字段名经 Magpie codex.go（真实世界核实）交叉校准
			// [2026-11-03-M9-W2]：key 载体=experimental_bearer_token
			// （api_key 非 Codex 字段会被忽略）；wire_api=responses
			// （Codex 原生 Responses 协议，网关 /v1/responses M4 实装）。
			Ops: func(addr, key, model string) []Op {
				return []Op{
					{Desc: "model → 默认模型", TOMLKey: "model", Value: model},
					{Desc: "model_provider → yardmaster", TOMLKey: "model_provider", Value: "yardmaster"},
					{Desc: "[model_providers.yardmaster].name", TOMLTable: []string{"model_providers", "yardmaster"}, TOMLKey: "name", Value: "yardmaster"},
					{Desc: "[model_providers.yardmaster].base_url", TOMLTable: []string{"model_providers", "yardmaster"}, TOMLKey: "base_url", Value: strings.TrimSuffix(addr, "/") + "/v1"},
					{Desc: "[model_providers.yardmaster].experimental_bearer_token", TOMLTable: []string{"model_providers", "yardmaster"}, TOMLKey: "experimental_bearer_token", Value: key},
					{Desc: "[model_providers.yardmaster].wire_api", TOMLTable: []string{"model_providers", "yardmaster"}, TOMLKey: "wire_api", Value: "responses"},
				}
			},
		},
		{
			ID: "gemini-cli", Name: "Gemini CLI", Format: FormatJSONC, CLI: "gemini",
			RelPaths: []string{"~/.gemini/settings.json"},
			// 写入规格经 Magpie gemini.go（真实世界核实）交叉校准
			// [2026-11-03-M9-W2]：auth 选择在 settings.json 的
			// security.auth.selectedType=gemini-api-key，模型在
			// model.name；端点与 key 由 CLI 自动加载的 ~/.gemini/.env
			// 注入（GOOGLE_GEMINI_BASE_URL / GEMINI_API_KEY），
			// 无需 shell profile。
			Ops: func(addr, key, model string) []Op {
				return []Op{
					{Desc: "security.auth.selectedType → gemini-api-key", JSONPath: []string{"security", "auth", "selectedType"}, Value: "gemini-api-key"},
					{Desc: "model.name → 默认模型/路由组", JSONPath: []string{"model", "name"}, Value: model},
				}
			},
			Extras: []Def{
				{
					Format:   FormatEnv,
					RelPaths: []string{"~/.gemini/.env"},
					Ops: func(addr, key, model string) []Op {
						return []Op{
							{Desc: ".env GOOGLE_GEMINI_BASE_URL → 网关地址", EnvKey: "GOOGLE_GEMINI_BASE_URL", Value: addr},
							{Desc: ".env GEMINI_API_KEY → 网关 key", EnvKey: "GEMINI_API_KEY", Value: key},
						}
					},
				},
			},
		},
		{
			ID: "opencode", Name: "OpenCode", Format: FormatJSONC, CLI: "opencode",
			RelPaths: []string{
				"~/.config/opencode/opencode.json",
				"~/.config/opencode/opencode.jsonc",
				"~/.config/opencode/config.json",
			},
			Ops: func(addr, key, model string) []Op {
				return []Op{
					{Desc: "provider.yardmaster.npm", JSONPath: []string{"provider", "yardmaster", "npm"}, Value: "@ai-sdk/openai-compatible"},
					{Desc: "provider.yardmaster.options.baseURL", JSONPath: []string{"provider", "yardmaster", "options", "baseURL"}, Value: strings.TrimSuffix(addr, "/") + "/v1"},
					{Desc: "provider.yardmaster.options.apiKey", JSONPath: []string{"provider", "yardmaster", "options", "apiKey"}, Value: key},
					{Desc: "model → yardmaster/<模型>", JSONPath: []string{"model"}, Value: "yardmaster/" + model},
				}
			},
		},
		{
			ID: "cline", Name: "Cline", Format: FormatJSONC, CLI: "cline",
			RelPaths: []string{"~/.cline/data/settings/providers.json"},
			// 写入规格对照官方实现核验 [2026-11-09-cline-spec]：Cline 已从
			// VS Code globalState（state.vscdb）迁移到文件化存储——
			// ~/.cline/data/settings/providers.json 是其跨 IDE/CLI/SDK 的
			// 统一 provider 存储（docs.cline.bot「Config」；cline auth 与
			// vercel/vercel 生产 CLI 的 cline 写入器同写此文件）。schema
			// 取自 cline/cline 源码 sdk/packages/core/src/types/
			// provider-settings.ts（StoredProviderSettingsSchema）与
			// sdk/packages/core/src/services/llms/provider-settings.ts
			// （ProviderSettingsSchema）：信封 {version:1,
			// lastUsedProvider?, providers:{<id>:{settings, updatedAt,
			// tokenSource}}}；settings.provider 必填，baseUrl 须为合法
			// URL（z.string().url()），protocol/client 枚举含
			// openai-chat / openai-compatible。updatedAt 为条目必填的
			// 时间戳（z.string().datetime()）——值随接线时刻变化，标记
			// Volatile 使漂移比对跳过。CLINE_DIR 环境变量可迁移配置根，
			// 本引擎不支持自定义根（与 CLI/SDK 缺省一致）。
			Ops: func(addr, key, model string) []Op {
				const provKey = "yardmaster"
				updatedAt := time.Now().UTC().Format(time.RFC3339)
				return []Op{
					{Desc: "version → 1（schema literal）", JSONPath: []string{"version"}, Value: 1},
					{Desc: "lastUsedProvider → yardmaster", JSONPath: []string{"lastUsedProvider"}, Value: provKey},
					{Desc: "providers.yardmaster.settings.provider", JSONPath: []string{"providers", provKey, "settings", "provider"}, Value: provKey},
					{Desc: "providers.yardmaster.settings.baseUrl → 网关 OpenAI 兼容端点", JSONPath: []string{"providers", provKey, "settings", "baseUrl"}, Value: strings.TrimSuffix(addr, "/") + "/v1"},
					{Desc: "providers.yardmaster.settings.apiKey → 网关 key", JSONPath: []string{"providers", provKey, "settings", "apiKey"}, Value: key},
					{Desc: "providers.yardmaster.settings.model → 默认模型/路由组", JSONPath: []string{"providers", provKey, "settings", "model"}, Value: model},
					{Desc: "providers.yardmaster.settings.protocol → openai-chat", JSONPath: []string{"providers", provKey, "settings", "protocol"}, Value: "openai-chat"},
					{Desc: "providers.yardmaster.settings.client → openai-compatible", JSONPath: []string{"providers", provKey, "settings", "client"}, Value: "openai-compatible"},
					{Desc: "providers.yardmaster.tokenSource → manual", JSONPath: []string{"providers", provKey, "tokenSource"}, Value: "manual"},
					{Desc: "providers.yardmaster.updatedAt → 接线时间（漂移比对跳过）", JSONPath: []string{"providers", provKey, "updatedAt"}, Value: updatedAt, Volatile: true},
				}
			},
		},
	}
}

// cliAvailable 报告 Agent CLI 是否在 PATH 上：Windows 下 LookPath 经
// PATHEXT 覆盖 npm shim（.cmd/.exe；.ps1 不在默认 PATHEXT，但 npm 同时
// 生成 .cmd shim，足矣），Unix 直接匹配可执行位。
func cliAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ByID 按 ID 取定义。
func ByID(id string) (Def, bool) {
	for _, d := range Registry() {
		if d.ID == id {
			return d, true
		}
	}
	return Def{}, false
}

// ErrSpecTBD 表示写入规格待核验（§4.2）。
var ErrSpecTBD = errors.New("写入规格待核验")

// ExpandHome 展开 ~ 前缀为当前用户目录。
func ExpandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~/")), nil
}
