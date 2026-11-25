// 壳层看门狗表驱动测试（2026-11-25）：假探测 + 假接力 + 临时数据目录，
// 直测 shellWatchdogStep（单步判定内核，不依赖真实 15s tick）——覆盖
// 触发阈值 / OK 与 unknown 复位 / 冷却窗 / 开关关闭 / spawn 失败续转。
package app

import (
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"yardmaster/internal/store"
)

// watchdogFixture 构造看门狗测试环境：假探测序列注入 shellProbeFn、假
// 接力（t.Cleanup 恢复注入点）、临时数据目录 + 静默通知器 + 开关初开。
// 返回 st（断言冷却戳用）与单步调用闭包（闭包内持有跨步连击数）。
func watchdogFixture(t *testing.T, probe func() shellHealth, relay func() error) (*serveSetup, func() bool) {
	t.Helper()
	oldProbe := shellProbeFn
	shellProbeFn = probe
	t.Cleanup(func() { shellProbeFn = oldProbe })
	st := &serveSetup{paths: store.Paths{AppDir: t.TempDir()}, notifier: newNotifier(false)}
	st.watchdogOn.Store(true)
	streak := 0
	return st, func() bool { return st.shellWatchdogStep(&streak, relay) }
}

// hungSeqFixture 在 watchdogFixture 基础上按序吐探测结论（步进索引）。
func hungSeqFixture(t *testing.T, seq []shellHealth, relay func() error) func() bool {
	t.Helper()
	i := 0
	_, step := watchdogFixture(t, func() shellHealth {
		h := seq[i%len(seq)]
		i++
		return h
	}, relay)
	return step
}

func TestShellWatchdogTriggerAfterStreak(t *testing.T) {
	var calls int
	st, step := watchdogFixture(t,
		func() shellHealth { return shellHung },
		func() error { calls++; return nil })
	for i := 0; i < shellHangStreak-1; i++ {
		if step() {
			t.Fatalf("第 %d 次挂起未达阈值，不应触发", i+1)
		}
	}
	if !step() {
		t.Fatalf("连续 %d 次挂起应触发接力", shellHangStreak)
	}
	if calls != 1 {
		t.Fatalf("接力应恰好发起一次，实际 %d", calls)
	}
	b, err := os.ReadFile(st.shellHangStampPath())
	if err != nil {
		t.Fatalf("触发后冷却戳未写入: %v", err)
	}
	ts, perr := strconv.ParseInt(string(b), 10, 64)
	if perr != nil || time.Since(time.Unix(ts, 0)) > time.Minute {
		t.Fatalf("冷却戳内容异常: %q err=%v", b, perr)
	}
}

func TestShellWatchdogStreakResetByOK(t *testing.T) {
	// 3 连击后被 OK 打断复位，重新累计满 4 次才触发。
	seq := make([]shellHealth, 0, shellHangStreak*2)
	for i := 0; i < shellHangStreak-1; i++ {
		seq = append(seq, shellHung)
	}
	seq = append(seq, shellOK)
	for i := 0; i < shellHangStreak; i++ {
		seq = append(seq, shellHung)
	}
	step := hungSeqFixture(t, seq, func() error { return nil })
	for n := 0; n < len(seq)-1; n++ {
		if step() {
			t.Fatalf("OK 复位场景第 %d 步提前触发", n+1)
		}
	}
	if !step() {
		t.Fatal("OK 复位后重新累计满阈值应触发")
	}
}

func TestShellWatchdogStreakResetByUnknown(t *testing.T) {
	// unknown（无壳窗口）同样中断序列——绝不凭空触发。
	seq := []shellHealth{shellHung, shellHung, shellHung, shellUnknown,
		shellHung, shellHung, shellHung, shellHung}
	step := hungSeqFixture(t, seq, func() error { return nil })
	for n := 0; n < len(seq)-1; n++ {
		if step() {
			t.Fatalf("unknown 复位场景第 %d 步提前触发", n+1)
		}
	}
	if !step() {
		t.Fatal("unknown 复位后重新累计满阈值应触发")
	}
}

func TestShellWatchdogCooldown(t *testing.T) {
	var fired int
	st, step := watchdogFixture(t, func() shellHealth { return shellHung },
		func() error { fired++; return nil })
	fresh := strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.WriteFile(st.shellHangStampPath(), []byte(fresh), 0o644); err != nil {
		t.Fatalf("预写冷却戳失败: %v", err)
	}
	for i := 0; i < shellHangStreak; i++ {
		if step() {
			t.Fatal("冷却窗内连击满也不应触发")
		}
	}
	if fired != 0 {
		t.Fatalf("冷却窗内发起了 %d 次接力", fired)
	}
	// 冷却过期后重新累计满阈值可再次触发。
	expired := strconv.FormatInt(time.Now().Add(-shellHangCooldown-time.Minute).Unix(), 10)
	if err := os.WriteFile(st.shellHangStampPath(), []byte(expired), 0o644); err != nil {
		t.Fatalf("改写过期戳失败: %v", err)
	}
	for i := 0; i < shellHangStreak-1; i++ {
		if step() {
			t.Fatalf("过期戳场景第 %d 步提前触发", i+1)
		}
	}
	if !step() {
		t.Fatal("冷却过期后连击满应触发")
	}
	if fired != 1 {
		t.Fatalf("过期后应恰好发起 1 次接力，实际 %d", fired)
	}
}

func TestShellWatchdogDisabled(t *testing.T) {
	st, step := watchdogFixture(t, func() shellHealth { return shellHung }, func() error {
		t.Fatal("开关关闭不应发起接力")
		return nil
	})
	st.watchdogOn.Store(false) // 设置热切换路径：任意时刻关掉
	for i := 0; i < shellHangStreak+2; i++ {
		if step() {
			t.Fatal("关闭态不应触发")
		}
	}
}

func TestShellWatchdogSpawnFailureContinues(t *testing.T) {
	// spawn 失败：不报告接力成功，连击复位后下轮满阈值再试。
	var attempts int
	step := hungSeqFixture(t,
		[]shellHealth{shellHung}, // 恒挂起
		func() error { attempts++; return errors.New("spawn fail") })
	for i := 0; i < shellHangStreak*2; i++ {
		if step() {
			t.Fatal("spawn 失败不应报告接力成功")
		}
	}
	if attempts != 2 {
		t.Fatalf("失败后应复位连击并再试一轮，实际尝试 %d 次", attempts)
	}
}

func TestShellWatchdogStampGarbageNotCooling(t *testing.T) {
	st, _ := watchdogFixture(t, func() shellHealth { return shellHung }, func() error { return nil })
	if err := os.WriteFile(st.shellHangStampPath(), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("写垃圾戳失败: %v", err)
	}
	if st.shellHangCoolingDown() {
		t.Fatal("非法戳内容不应视为冷却中")
	}
}
