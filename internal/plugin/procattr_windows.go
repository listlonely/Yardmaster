// Windows 插件子进程创建标志（2026-11-11）：插件为 NDJSON 管道通信的
// 后台服务型子进程（node/exe 长驻），从 GUI 版网关（windowsgui 无控制
// 台）spawn console 子进程时 Windows 会新建 conhost 黑窗（用户实测：
// 每次启动弹 node.exe 黑窗）。CREATE_NO_WINDOW 让子进程无声运行——
// stdin/stdout 管道与 stderr 转发不受影响。
//
//go:build windows

package plugin

import "syscall"

// createNoWindow 是 CREATE_NO_WINDOW 进程创建标志。
const createNoWindow = 0x08000000

// spawnSysProcAttr 返回插件 spawn 的进程属性（nil 字段安全）。
func spawnSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
