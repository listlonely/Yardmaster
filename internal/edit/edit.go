// Package edit 提供 Agent 配置文件的安全编辑：原子写、编码保留
// （BOM/CRLF）、注释保留——配置改写铁律（开发文档 §4.3）的落地层。
//
// 骨架阶段先落地 WriteAtomic；JSONC/TOML/YAML/env 的键级编辑器
// （只写名下键、保留注释与顺序）在 M1-W4 实现。
package edit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic 以「临时文件 + rename」原子写入 path。
//
//   - 目标已存在时保留其权限位；新建文件权限 0600（配置可能含密钥，§4.4）；
//   - 任何失败不触碰原文件，不产生部分写入（F4 边界规则）；
//   - 内容层面的保留策略（BOM/CRLF/注释/名下键）由各键级编辑器负责，
//     整文件覆盖调用方必须先完成备份（stash）。
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure dir %s: %w", dir, err)
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".agenthub-edit-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// 成功 rename 后本清理是空操作；失败路径上保证不留临时文件。
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
