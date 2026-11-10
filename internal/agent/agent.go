// Package agent 管理 Agent 注册表与接线：检测、配置改写、备份还原、
// 漂移检测（PRD F4、F5）。本包是全项目最敏感区域，改写铁律见
// 开发文档 §4.3：只写名下键、原子写、备份栈、用户值优先、审计快照。
//
// 类型分布：Def/Op（声明式描述）见 registry.go；State/WiringRecord/
// 接线引擎见 wiring.go；本文件仅保留格式常量。
package agent

// Format 是目标 Agent 配置文件格式（edit 包提供对应键级编辑器）。
type Format string

const (
	FormatJSONC Format = "jsonc" // JSON 与 JSONC 同一实现
	FormatTOML  Format = "toml"
	FormatYAML  Format = "yaml" // YAML/env 编辑器待首个需要的 Agent 引入时实现
	FormatEnv   Format = "env"
)
