// Package store 负责 settings/providers/trace 的持久化。JSON 配置可备份
// 可 diff（设计文档 §6.1）；决策记录只存元数据，永不落盘对话内容（§4.4）。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"agenthub/internal/edit"
)

// Paths 聚合本机数据目录布局。AppDir 可注入以便 testenv 沙箱。
type Paths struct{ AppDir string }

// DefaultPaths 返回当前用户的默认数据目录：<用户目录>/.agenthub。
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home: %w", err)
	}
	return Paths{AppDir: filepath.Join(home, ".agenthub")}, nil
}

// SettingsPath 返回 settings.json 路径（网关地址、开关等）。
func (p Paths) SettingsPath() string { return filepath.Join(p.AppDir, "settings.json") }

// ProvidersPath 返回 providers.json 路径（Provider/账号/路由组）。
func (p Paths) ProvidersPath() string { return filepath.Join(p.AppDir, "providers.json") }

// TraceDir 返回决策记录目录（保留 30 天，F9）。
func (p Paths) TraceDir() string { return filepath.Join(p.AppDir, "trace") }

// IntentsPath 返回 intents.json 路径（用户自定义识别策略，PRD F8）。
func (p Paths) IntentsPath() string { return filepath.Join(p.AppDir, "intents.json") }

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
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
