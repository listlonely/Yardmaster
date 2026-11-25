//go:build !windows

// 非 Windows 桩（2026-11-25）：探测恒「未知」——看门狗空转不触发
// （参数常量与主循环仍编译，GOOS=linux 交叉构建铁律照常通过）。
package app

// shellHungReport 非 Windows 无壳层挂起探测实装：恒 unknown（主循环
// 视为序列中断，永不触发）。
func shellHungReport() shellHealth { return shellUnknown }
