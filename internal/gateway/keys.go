// 调用方默认模型绑定（W5 设计决策并入）：keys.json 把网关侧调用方
// key（gw-<agent> 命名，约定不强制）绑定到默认路由目标。请求未带
// model 或 model 为 "default" 时使用绑定；文件每次读取（小文件，
// CLI 编辑天然热生效）。
package gateway

import (
	"errors"
	"log/slog"
	"os"

	"agenthub/internal/store"
)

// KeyBinding 是一条绑定：调用方 key → 默认路由目标。
type KeyBinding struct {
	Key          string `json:"key"`
	DefaultModel string `json:"default_model"`
}

// KeysFile 是 keys.json 的文件形态。
type KeysFile struct {
	Bindings []KeyBinding `json:"bindings"`
}

// LoadKeysFile 读取绑定文件；不存在返回 os.ErrNotExist。
func LoadKeysFile(path string) (KeysFile, error) {
	var f KeysFile
	if err := store.LoadJSON(path, &f); err != nil {
		return f, err
	}
	return f, nil
}

// SaveKeysFile 原子写入绑定文件。
func SaveKeysFile(path string, f KeysFile) error { return store.SaveJSON(path, f) }

// KeyStore 查询调用方默认模型。
type KeyStore struct{ path string }

// NewKeyStore 构建存储。
func NewKeyStore(path string) *KeyStore { return &KeyStore{path: path} }

// DefaultModel 返回 key 绑定的默认模型；未绑定/无 key/读不懂返回空。
func (k *KeyStore) DefaultModel(key string) string {
	if k == nil || key == "" {
		return ""
	}
	f, err := LoadKeysFile(k.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("keys.json 读取失败，按未绑定处理", "path", k.path, "err", err)
		}
		return ""
	}
	for _, b := range f.Bindings {
		if b.Key == key {
			return b.DefaultModel
		}
	}
	return ""
}
