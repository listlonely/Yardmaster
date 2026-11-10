//go:build windows

package app

import "golang.org/x/sys/windows"

// diskFree 返回 path 所在盘调用方可写字节数（M11-W2 下载预检；
// GetDiskFreeSpaceEx 取调用方配额，与实际可写一致）。
func diskFree(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}
