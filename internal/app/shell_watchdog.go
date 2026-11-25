// 壳层卡死看门狗（2026-11-25 用户需求：「监测到系统壳层卡住后直接重启」
// ——v0.2.14 期间 Explorer 挂起导致托盘/窗口全部失联，希望自动自愈）：
// 周期探测 Windows 壳层（Explorer）是否「进程存活但窗口挂起」——这种
// 病态下 fyne 托盘的 TaskbarCreated 自愈机制帮不上忙（Explorer 没死，
// 不会广播任务栏重建）。连续命中达阈值即走与自动更新/手动重启完全相同
// 的接力通道（spawnRestartFn → 气泡告知 → stop/quitTray），新进程接棒
// 后托盘随新进程恢复。
//
// 不覆盖 Explorer 真崩溃退出：壳进程消失后系统广播 TaskbarCreated，托盘
// 由 systray 内部 nid.add() 自动重注册，无需本组件（真死场景探测返回
// shellUnknown，绝不凭空触发）。
//
// 保守性设计：①判定需连续 4 次探测命中（15s × 4 ≈ 60s 持续挂起），
// 瞬时卡顿不触发；②10 分钟跨进程冷却（数据目录 stamp 文件，接力后新
// 进程共享）防止「重启也治不好」的环境问题反复拉起；③设置页可关
// （settings.ShellWatchdog，nil=开，落盘钩子热切换无需重启）；④探测/
// 收尾任何 panic 由顶层 recover 隔离（语义同托盘 runTrayIsolated），
// 绝不拖垮网关进程。
//
// 装配点：仅桌面壳（runServe -tray / runApp 含托盘态）启动——headless
// serve 无托盘可恢复，重启无意义。本文件为平台无关主体；探测实装见
// shell_watchdog_windows.go（非 Windows 桩恒 unknown）。
package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 看门狗参数：探测节奏 / 触发阈值 / 冷却窗（见文件头注释）。
const (
	shellHangPollEvery = 15 * time.Second         // 单次探测间隔
	shellHangStreak    = 4                        // 连续命中次数（≈60s）
	shellHangCooldown  = 10 * time.Minute         // 跨进程冷却窗
	shellHangStampName = "shell-hang-relay.stamp" // 数据目录下的冷却戳
)

// shellHealth 是单次壳层探测结论。
type shellHealth int

const (
	shellOK      shellHealth = iota // 壳层健康
	shellHung                       // 壳窗口存在但挂起（超系统阈值无消息响应）
	shellUnknown                    // 无法判定（无壳窗口/平台未实装）——不计不清
)

// shellProbeFn 是平台探测函数（windows 实装 GetShellWindow +
// IsHungAppWindow；其他平台恒 shellUnknown）。测试替换用。
var shellProbeFn = shellHungReport

// shellHangStampPath 冷却戳落数据目录（跨进程共享——接力重启后新进程
// 读到旧进程留下的戳，冷却窗才能跨过进程边界生效）。
func (st *serveSetup) shellHangStampPath() string {
	return filepath.Join(st.paths.AppDir, shellHangStampName)
}

// shellHangCoolingDown 报告冷却窗内是否已触发过（戳内容=触发时刻 unix
// 秒；读不到/读不懂=未冷却）。
func (st *serveSetup) shellHangCoolingDown() bool {
	b, err := os.ReadFile(st.shellHangStampPath())
	if err != nil {
		return false
	}
	t, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || t <= 0 {
		return false
	}
	return time.Since(time.Unix(t, 0)) < shellHangCooldown
}

// markShellHangRelay 写冷却戳（失败仅降级留痕——冷却失效的代价是极端
// 场景下多一次接力重启，不值得为此阻断触发）。
func (st *serveSetup) markShellHangRelay() {
	p := st.shellHangStampPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		slog.Warn("壳层看门狗冷却戳目录创建失败", "dir", filepath.Dir(p), "err", err)
		return
	}
	if err := os.WriteFile(p, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644); err != nil {
		slog.Warn("壳层看门狗冷却戳写入失败", "file", p, "err", err)
	}
}

// shellWatchdogStep 是看门狗单次探测判定（主循环每 tick 调一次；抽出为
// 独立函数供表驱动测试直测，不依赖真实节奏）。streak 为跨 tick 连击数
// （调用方持有）；返回 true=接力已发起（本进程即将退出，主循环应返回）。
func (st *serveSetup) shellWatchdogStep(streak *int, relay func() error) bool {
	if !st.watchdogOn.Load() { // 设置热切换：关闭即复位连击
		*streak = 0
		return false
	}
	switch shellProbeFn() {
	case shellHung:
		*streak++
		slog.Warn("壳层挂起探测命中", "streak", *streak, "need", shellHangStreak)
		if *streak < shellHangStreak {
			return false
		}
		if st.shellHangCoolingDown() {
			slog.Warn("壳层仍挂起但处于冷却窗（跳过自动重启）",
				"cooldown", shellHangCooldown.String())
			*streak = 0
			return false
		}
		slog.Warn("壳层持续挂起，触发自动接力重启",
			"old_pid", os.Getpid(), "streak", *streak)
		if err := relay(); err != nil {
			slog.Error("壳层卡死自动重启失败（继续监测）", "err", err)
			*streak = 0 // spawn 失败=进程未退出且未接力，复位连击下轮再试（不写冷却戳）
			return false
		}
		st.markShellHangRelay() // 接力已发起才进冷却窗（失败重试不受冷却限制）
		return true             // stop/quitTray 异步收尾，本进程即将退出
	case shellOK:
		if *streak > 0 {
			slog.Info("壳层恢复响应（连击复位）", "was_streak", *streak)
		}
		*streak = 0
	case shellUnknown:
		*streak = 0 // 无壳窗口/平台未实装：序列中断，绝不凭空触发
	}
	return false
}

// runShellWatchdog 看门狗主循环（桌面壳装配）。触发后接力钩子已异步
// 收尾（stop/quitTray），本 goroutine 返回、进程随即退出。任何 panic
// 被 recover 隔离并放弃本次运行的自动自愈（宁可不自愈也不拖垮网关）。
func (st *serveSetup) runShellWatchdog(stop context.CancelFunc, quitTray func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("壳层看门狗 panic（已隔离；本次运行放弃自动自愈）", "panic", r)
		}
	}()
	relay := st.restartHookNotify(stop, quitTray, shellHangKey,
		"Yardmaster 检测到壳层卡死", "壳层持续无响应，已自动接力重启")
	tick := time.NewTicker(shellHangPollEvery)
	defer tick.Stop()
	streak := 0
	for range tick.C {
		if st.shellWatchdogStep(&streak, relay) {
			return
		}
	}
}
