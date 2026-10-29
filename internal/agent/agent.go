// Package agent 管理 Agent 注册表与接线：检测、配置改写、备份还原、
// 漂移检测（PRD F4、F5）。本包是全项目最敏感区域，改写铁律见
// 开发文档 §4.3：只写名下键、原子写、备份栈、用户值优先、审计快照。
//
// 骨架阶段只定义声明式描述与状态模型；探测与改写逻辑 W4 实现。
package agent

// Format 是目标 Agent 配置文件格式（edit 包提供对应键级编辑器）。
type Format string

const (
	FormatJSONC Format = "jsonc"
	FormatTOML  Format = "toml"
	FormatYAML  Format = "yaml"
	FormatEnv   Format = "env"
)

// Def 是一个受支持 Agent 的声明式描述（设计文档 §4.5：声明式而非
// 硬编码逻辑）。M1 首发 5 个：claude-code / codex / gemini-cli /
// opencode / cline。
type Def struct {
	ID          string   // 稳定 ID，同时用于网关 key：gw-<id>（F10）
	Name        string
	ConfigPaths []string // 候选配置文件路径，按优先级；支持 ~ 展开
	Format      Format
	DetectDirs  []string // 安装目录探测：存在其一且 CLI 缺失仍视为已安装的兜底
}

// State 是接线状态机的状态（PRD 规则页「Agent 接线状态」，勿另立定义）。
type State string

const (
	StateMissing State = "missing" // 未安装：灰置 + 安装命令
	StateUnwired State = "unwired" // 未接入
	StateWiring  State = "wiring"  // 接线中（亚秒级；失败回 unwired 并展示原因）
	StateWired   State = "wired"   // 已接入：名下键均为最后写入值
	StateDrift   State = "drift"   // 漂移：名下键被外部改动（F5）
)

// Wiring 记录一次接入的全部可审计信息（F10 改写审计）。
type Wiring struct {
	AgentID    string            `json:"agent_id"`
	State      State             `json:"state"`
	GatewayKey string            `json:"gateway_key"` // 形如 gw-<agentid>
	Applied    map[string]string `json:"applied"`     // 名下键 → 最后写入值（漂移比对基准）
	StashPath  string            `json:"stash_path"`  // 原值备份文件路径
}
