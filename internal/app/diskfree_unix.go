//go:build !windows

package app

import "syscall"

// diskFree 返回 path 所在文件系统可写字节数（Statfs Bavail×Bsize）。
// 自动更新仅 Windows 分发 zip——此实现主要服务 Linux CI 单测与交叉
// 构建完整性（真 release 中非 Windows 平台资产查无而自然报错）。
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
