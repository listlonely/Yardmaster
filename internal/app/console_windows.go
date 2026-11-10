// Windows 控制台收起与留痕（M8 收官后用户反馈「每次启动应用会打开终端」）：
// console subsystem 的二进制双击/开始菜单启动时 conhost 黑窗全程伴随
// （app 模式常驻=黑窗常驻）。方案：孤立启动（父进程为 explorer.exe）
// 时 FreeConsole 收起黑窗，GUI（托盘+原生窗口）继续承载，slog 落数据
// 目录 app.log 保住崩溃/回退/托盘隔离留痕；从终端启动（父为 shell）
// 不干预——banner/日志/Ctrl-C 语义零变化。纯 syscall 零 cgo。
//
//go:build windows

package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole              = kernel32.NewProc("FreeConsole")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	procCloseHandle              = kernel32.NewProc("CloseHandle")
)

// hideConsoleIfDetached 孤立启动时收起黑窗，返回是否已收起（调用方应
// 随之把 slog 落文件）。父进程非 explorer（终端启动）不干预。
func hideConsoleIfDetached() bool {
	parent, ok := parentProcessName()
	if !ok || !strings.EqualFold(parent, "explorer.exe") {
		return false
	}
	r, _, _ := procFreeConsole.Call()
	return r != 0
}

// parentProcessName 用 Toolhelp32 快照返回父进程映像名（进程数百级，
// 启动时一次快照两次线性查找，开销可忽略）。
func parentProcessName() (string, bool) {
	const th32csSnapProcess = 0x2
	type processEntry struct {
		Size          uint32
		Usage         uint32
		PID           uint32
		DefaultHeapID uintptr
		ModuleID      uint32
		Threads       uint32
		ParentPID     uint32
		PriClassBase  int32
		Flags         uint32
		ExeFile       [260]uint16
	}
	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == uintptr(syscall.InvalidHandle) || snap == 0 {
		return "", false
	}
	defer func() { _, _, _ = procCloseHandle.Call(snap) }()

	find := func(pid uint32) (string, bool) {
		entry := &processEntry{Size: uint32(unsafe.Sizeof(processEntry{}))}
		for first := true; ; first = false {
			if first {
				_, _, _ = procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(entry)))
			} else if r, _, _ := procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(entry))); r == 0 {
				return "", false
			}
			if entry.PID == pid {
				return strings.TrimLeft(syscall.UTF16ToString(entry.ExeFile[:]), "\x00"), true
			}
		}
	}
	self := uint32(os.Getpid())
	parentPID, ok := func() (uint32, bool) {
		entry := &processEntry{Size: uint32(unsafe.Sizeof(processEntry{}))}
		for first := true; ; first = false {
			if first {
				_, _, _ = procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(entry)))
			} else if r, _, _ := procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(entry))); r == 0 {
				return 0, false
			}
			if entry.PID == self {
				return entry.ParentPID, true
			}
		}
	}()
	if !ok {
		return "", false
	}
	return find(parentPID)
}

// setupAppLog 孤立启动（黑窗已收）后的留痕通道：slog 落数据目录
// app.log（启动时 >1MB 轮转为 .1，覆盖旧轮转）——崩溃/回退/托盘隔离
// 日志在无终端场景仍可诊断。打不开就静默走原通道，不阻塞启动。
func setupAppLog(dir string) {
	path := filepath.Join(dir, "app.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 1<<20 {
		_ = os.Rename(path, filepath.Join(dir, "app.log.1"))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
}
