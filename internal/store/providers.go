// providers.json 的统一文件形态：app（CLI/装配）与 gateway（用量
// 落盘）共用同一 schema，避免双处定义漂移。
package store

import "agenthub/internal/provider"

// ProvidersFile 是 providers.json 的文件形态。
type ProvidersFile struct {
	Providers []provider.Provider `json:"providers"`
	Groups    []provider.Group    `json:"groups,omitempty"`
}

// LoadProvidersFile 读取 provider/组配置；不存在返回 os.ErrNotExist。
func LoadProvidersFile(path string) (ProvidersFile, error) {
	var f ProvidersFile
	if err := LoadJSON(path, &f); err != nil {
		return f, err
	}
	return f, nil
}

// SaveProvidersFile 原子写入 provider/组配置。
func SaveProvidersFile(path string, f ProvidersFile) error { return SaveJSON(path, f) }
