// Package store 负责 settings/providers/trace 的持久化。JSON 配置可备份
// 可 diff（设计文档 §6.1）；决策记录只存元数据，永不落盘对话内容（§4.4）。
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"yardmaster/internal/edit"
)

// Paths 聚合本机数据目录布局。AppDir 可注入以便 testenv 沙箱。
type Paths struct{ AppDir string }

// DefaultPaths 返回当前用户的默认数据目录：<用户目录>/.yardmaster。
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home: %w", err)
	}
	return Paths{AppDir: filepath.Join(home, ".yardmaster")}, nil
}

// SettingsPath 返回 settings.json 路径（网关地址、开关等）。
func (p Paths) SettingsPath() string { return filepath.Join(p.AppDir, "settings.json") }

// ProvidersPath 返回 providers.json 路径（Provider/账号/路由组）。
func (p Paths) ProvidersPath() string { return filepath.Join(p.AppDir, "providers.json") }

// TraceDir 返回决策记录目录（保留 30 天，F9）。
func (p Paths) TraceDir() string { return filepath.Join(p.AppDir, "trace") }

// IntentsPath 返回 intents.json 路径（用户自定义识别策略，PRD F8）。
func (p Paths) IntentsPath() string { return filepath.Join(p.AppDir, "intents.json") }

// RulesPath 返回 rules.json 路径（任务规则，PRD F7；W5 新增）。
func (p Paths) RulesPath() string { return filepath.Join(p.AppDir, "rules.json") }

// KeysPath 返回 keys.json 路径（调用方 key 绑定默认模型；W5 新增）。
func (p Paths) KeysPath() string { return filepath.Join(p.AppDir, "keys.json") }

// PricingPath 返回 pricing.json 路径（模型单价表，M2-W4）。
func (p Paths) PricingPath() string { return filepath.Join(p.AppDir, "pricing.json") }

// DispatchPath 返回 dispatch.json 路径（任务→Agent 派发配置，M5-W4）。
func (p Paths) DispatchPath() string { return filepath.Join(p.AppDir, "dispatch.json") }

// BackupDir 返回配置写前快照目录（备份栈，M2-W1）。
func (p Paths) BackupDir() string { return filepath.Join(p.AppDir, "backup") }

// CredentialsPath 返回 OAuth 凭证库路径（M3-W1）。Windows 为 DPAPI
// 密文 credentials.bin，其余平台为 0600 JSON credentials.json——文件
// 形态由 internal/credential 决定，此处与其保持一致。
func (p Paths) CredentialsPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(p.AppDir, "credentials.bin")
	}
	return filepath.Join(p.AppDir, "credentials.json")
}

// AppliedPath 返回 applied.json 路径（接线审计记录，F4/F5/F10）。
func (p Paths) AppliedPath() string { return filepath.Join(p.AppDir, "applied.json") }

// StashDir 返回配置备份目录（断开还原用，F4）。
func (p Paths) StashDir() string { return filepath.Join(p.AppDir, "stash") }

// SaveJSON 以缩进 JSON 原子写入 v（经 edit.WriteAtomic，0600）。
func SaveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	return edit.WriteAtomic(path, data)
}

// LoadJSON 读取并反序列化 path 到 v。
//
// 语义遵循「读不懂 = 未知」（§4.2）：文件不存在返回包内 os.ErrNotExist
// 供调用方决定默认值；解析失败返回带路径的错误，调用方必须保留旧值
// 或 fail-closed，绝不允许当作空配置吞掉。
func LoadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// UTF-8 BOM 容错（Windows 记事本等编辑器常态）：带 BOM 的 JSON 是
	// 可读文件，不算「读不懂」——与 edit 包对 Agent 配置的 BOM 处理同规。
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
