// 声明式 Agent 注册表（PRD F4）：每个 Agent 的配置位置、格式与要写的
// 字段全部数据化，接线引擎不感知具体 Agent。
//
// §4.2 铁律：字段名与取值必须来自该 Agent 自己的文档/实现。gemini-cli
// 与 cline 的写入规格尚未对照官方实现核验（[TBD]），注册表明确拒绝
// 接线并说明原因——宁可少接一个，不写猜出来的键。
package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Op 是对目标配置的一次键写入；JSON 格式用 JSONPath，TOML 用表+键。
type Op struct {
	Desc      string `json:"desc"`
	JSONPath  []string `json:"json_path,omitempty"`
	TOMLTable []string `json:"toml_table,omitempty"`
	TOMLKey   string   `json:"toml_key,omitempty"`
	Value     any      `json:"value"`
}

// Def 是一个受支持 Agent 的声明式描述。
type Def struct {
	ID       string // 稳定 ID；网关 key = gw-<ID>
	Name     string
	Format   Format
	RelPaths []string // 候选配置路径（支持 ~ 展开），按优先级
	SpecNote string   // 非空 = 写入规格待核验，拒绝接线（§4.2）
	Ops      func(gwAddr, key, model string) []Op
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
			Ops: func(addr, key, model string) []Op {
				return []Op{
					{Desc: "model → 默认模型", TOMLKey: "model", Value: model},
					{Desc: "model_provider → agenthub", TOMLKey: "model_provider", Value: "agenthub"},
					{Desc: "[model_providers.agenthub].name", TOMLTable: []string{"model_providers", "agenthub"}, TOMLKey: "name", Value: "AgentHub"},
					{Desc: "[model_providers.agenthub].base_url", TOMLTable: []string{"model_providers", "agenthub"}, TOMLKey: "base_url", Value: strings.TrimSuffix(addr, "/") + "/v1"},
					{Desc: "[model_providers.agenthub].api_key", TOMLTable: []string{"model_providers", "agenthub"}, TOMLKey: "api_key", Value: key},
					{Desc: "[model_providers.agenthub].wire_api", TOMLTable: []string{"model_providers", "agenthub"}, TOMLKey: "wire_api", Value: "chat"},
				}
			},
		},
		{
			ID: "gemini-cli", Name: "Gemini CLI", Format: FormatJSONC,
			RelPaths: []string{"~/.gemini/settings.json"},
			SpecNote: "Gemini CLI 的自定义端点依赖环境变量注入（GEMINI_API_KEY / GOOGLE_GEMINI_BASE_URL），settings.json 无独立键；需 env 注入方案（P1），不写猜测的键",
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
					{Desc: "provider.agenthub.npm", JSONPath: []string{"provider", "agenthub", "npm"}, Value: "@ai-sdk/openai-compatible"},
					{Desc: "provider.agenthub.options.baseURL", JSONPath: []string{"provider", "agenthub", "options", "baseURL"}, Value: strings.TrimSuffix(addr, "/") + "/v1"},
					{Desc: "provider.agenthub.options.apiKey", JSONPath: []string{"provider", "agenthub", "options", "apiKey"}, Value: key},
					{Desc: "model → agenthub/<模型>", JSONPath: []string{"model"}, Value: "agenthub/" + model},
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
