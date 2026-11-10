// 配置写前快照（备份栈，M2-W1）：管理 API 每次覆写配置文件前先把
// 当前内容留底到 backup/ 目录，同一文件只保留最近 keep 份——连续编辑
// 不会覆盖最早的备份，用户随时可手工还原（铁律：覆写前留底）。
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"yardmaster/internal/edit"
)

// BackupConfig 把 path 的当前内容复制为 backupDir/<base>-<时间戳>.json，
// 并把 backupDir 下 <base>-*.json 截断到最近 keep 份。
//
//   - 源文件不存在（首次创建）返回 nil：无旧值可留底不算错误；
//   - 复制走 WriteAtomic，快照自身也原子落盘；
//   - 备份失败返回错误，调用方必须中止覆写（宁可拒绝编辑也不无备份写入）。
func BackupConfig(path, backupDir, base string, keep int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s for backup: %w", path, err)
	}
	if keep <= 0 {
		keep = 10
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("ensure backup dir: %w", err)
	}
	// 纳秒精度：同秒连续编辑不互相覆盖；定宽格式保证字典序=时间序。
	// 扩展名随源文件（credentials.bin 等非 JSON 库同样走此备份栈）。
	ext := filepath.Ext(path)
	name := fmt.Sprintf("%s-%s%s", base, time.Now().Format("20060102-150405.000000000"), ext)
	if err := edit.WriteAtomic(filepath.Join(backupDir, name), data); err != nil {
		return fmt.Errorf("write backup snapshot: %w", err)
	}
	return trimBackups(backupDir, base, keep, ext)
}

// trimBackups 删除同 base 的最旧快照，只留最近 keep 份（调用方已建目录）。
func trimBackups(backupDir, base string, keep int, ext string) error {
	matches, err := filepath.Glob(filepath.Join(backupDir, base+"-*"+ext))
	if err != nil {
		return fmt.Errorf("glob backups: %w", err)
	}
	// 文件名含时间戳（同秒覆盖写同一路径），字典序即时间序。
	sort.Strings(matches)
	for _, old := range matches[:max(0, len(matches)-keep)] {
		if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove old backup %s: %w", old, err)
		}
	}
	return nil
}

// BaseName 取路径去扩展名的文件名，作为备份快照前缀（防路径分隔符注入）。
func BaseName(path string) string {
	b := filepath.Base(path)
	return strings.TrimSuffix(b, filepath.Ext(b))
}
