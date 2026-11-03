// 声明式 Agent 注册表（PRD F4）：每个 Agent 的配置位置、格式与要写的
// 字段全部数据化，接线引擎不感知具体 Agent。
//
// §4.2 铁律：字段名与取值必须来自该 Agent 自己的文档/实现。字段形状
// 经真实世界核实的参考实现交叉校准（Magpie agent-wiring codex.go/
// gemini.go，[2026-11-03-M9-W2]）；cline 的写入规格尚未对照官方实现
// 核验（[TBD]），注册表明确拒绝接线并说明原因——宁可少接一个，不写
// 猜出来的键。
package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Op 是对目标配置的一次键写入；JSON 格式用 JSONPath，TOML 用表+键，
// env 用 EnvKey。
type Op struct {
	Desc      string   `json:"desc"`
	JSONPath  []string `json:"json_path,omitempty"`
	TOMLTable []string `json:"toml_table,omitempty"`
	TOMLKey   string   `json:"toml_key,omitempty"`
	EnvKey    string   `json:"env_key,omitempty"`
	Value     any      `json:"value"`
}

// Def 是一个受支持 Agent 的声明式描述。Extras 是随主文件一同接线的
// 附属文件（如 Gemini CLI 的 ~/.gemini/.env）：各自 Format/RelPaths/Ops，
// 与主文件同一份接线记录（Files 按序展开）。
type Def struct {
	ID       string // 稳定 ID；网关 key = gw-<ID>
	Name     string
	Format   Format
	RelPaths []string // 候选配置路径（支持 ~ 展开），按优先级
	SpecNote string   // 非空 = 写入规格待核验，拒绝接线（§4.2）
	Ops      func(gwAddr, key, model string) []Op
	Extras   []Def
}

// Registry 返回全部受支持 Agent（MVP 5 个）。
func Registry() []Def {
	return []Def{
		{
			ID: "claude-code", Name: "Claude Code", Format: FormatJSONC,
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
			ID: "codex", Name: "Codex", Format: FormatTOML,
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
			ID: "gemini-cli", Name: "Gemini CLI", Format: FormatJSONC,
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
			ID: "opencode", Name: "OpenCode", Format: FormatJSONC,
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
			ID: "cline", Name: "Cline", Format: FormatJSONC,
			RelPaths: []string{
				"~/AppData/Roaming/Code/User/globalStorage/saoudrizwan.claude-dev/settings",
				"~/.config/Code/User/globalStorage/saoudrizwan.claude-dev/settings",
			},
			SpecNote: "Cline 的 provider 配置存于 VS Code globalState 存储层，文件写入规格待对照其实现核验（[TBD]）",
		},
	}
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
