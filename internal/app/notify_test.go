// 通知器与托盘辅助测试（M9-W4）：限频语义、摘要截断、设置指针语义。
package app

import (
	"testing"
	"time"

	"yardmaster/internal/store"
)

// 同 key 限频：窗口内第二次静默；不同 key 互不影响；不限频 key（空）
// 每次 都发。
func TestNotifierRateLimit(t *testing.T) {
	n := newNotifier(true)
	sent := 0
	orig := sendBeeep
	sendBeeep = func(title, msg string) { sent++ }
	defer func() { sendBeeep = orig }()

	n.send("k", "t", "m1")
	n.send("k", "t", "m2") // TTL 内静默
	if sent != 1 {
		t.Fatalf("同 key 窗口内应限频: sent=%d", sent)
	}
	n.last["k"] = time.Now().Add(-restNotifyTTL - time.Second) // 越窗
	n.send("k", "t", "m3")
	if sent != 2 {
		t.Fatalf("越窗后应再发: sent=%d", sent)
	}
	n.send("other", "t", "m")
	if sent != 3 {
		t.Fatalf("不同 key 不互相限频: sent=%d", sent)
	}
	n.send("", "t", "m") // 空 key=不限频
	n.send("", "t", "m")
	if sent != 5 {
		t.Fatalf("空 key 应每次都发: sent=%d", sent)
	}
}

func TestNotifierDisabled(t *testing.T) {
	n := newNotifier(false)
	sent := 0
	orig := sendBeeep
	sendBeeep = func(title, msg string) { sent++ }
	defer func() { sendBeeep = orig }()
	n.send("k", "t", "m")
	if sent != 0 {
		t.Fatalf("禁用态不应发送: sent=%d", sent)
	}
	var nilN *notifier
	nilN.send("k", "t", "m") // nil 安全：不 panic 不发送
}

// settings.notify 指针语义：nil=缺省开启，false=显式关闭。
func TestSettingsNotifyEnabled(t *testing.T) {
	var s store.Settings
	if !s.NotifyEnabled() {
		t.Fatal("nil 应缺省开启")
	}
	f := false
	s.Notify = &f
	if s.NotifyEnabled() {
		t.Fatal("显式 false 应关闭")
	}
	tr := true
	s.Notify = &tr
	if !s.NotifyEnabled() {
		t.Fatal("显式 true 应开启")
	}
}
