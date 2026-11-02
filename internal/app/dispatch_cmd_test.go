// dispatch run -events 测试（M6-W3）：事件流实时渲染 + text 回退透传。
// 假 Agent 用测试二进制自身（TestDispatchJSONLHelper），HOME 沙箱经
// testenv 隔离，不碰真机 Agent、不碰网络。
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/store"
	"yardmaster/internal/testenv"
)

// TestDispatchJSONLHelper 不是真正的测试：作为假 Agent 子进程运行。
func TestDispatchJSONLHelper(t *testing.T) {
	if os.Getenv("YM_DSP_HELPER_ON") != "1" {
		return
	}
	fmt.Println(`{"type":"system","subtype":"init"}`)
	fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"事件中的文本"}]}}`)
	fmt.Println(`{"type":"result","subtype":"success","result":"CLI 事件终态"}`)
	os.Exit(0)
}

// writeDispatchJSONL 在沙箱 HOME 写 dispatch.json（jsonl 假 Agent，default 兜底）。
func writeDispatchJSONL(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := dispatch.AgentSpec{
		ID: "fake", Output: "jsonl", Default: true,
		Command: []string{exe, "-test.run=TestDispatchJSONLHelper", "--"},
		Env:     []string{"YM_DSP_HELPER_ON=1"},
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.AppDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJSON(paths.DispatchPath(), dispatch.DispatchFile{Agents: []dispatch.AgentSpec{spec}}); err != nil {
		t.Fatal(err)
	}
}

// -events：事件实时渲染（raw 之外按词表着行），不再重复打印提取结果行。
func TestDispatchRunEventsRendersStream(t *testing.T) {
	home := testenv.New(t)
	writeDispatchJSONL(t)

	var out, errOut strings.Builder
	if err := Run([]string{"dispatch", "run", "-events", "任务"}, &out, &errOut); err != nil {
		t.Fatalf("dispatch run -events: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("事件行数 = %d: %q", len(lines), out.String())
	}
	if lines[0] != "[init] init" {
		t.Fatalf("init 行: %q", lines[0])
	}
	if lines[1] != "事件中的文本" {
		t.Fatalf("text 行: %q", lines[1])
	}
	if lines[2] != "[result·success] CLI 事件终态" {
		t.Fatalf("result 行: %q", lines[2])
	}
	if strings.Contains(out.String(), "[dispatch] 结果:") {
		t.Fatal("事件流模式不应重复打印提取结果")
	}
	if !strings.Contains(errOut.String(), " · ok · ") {
		t.Fatalf("摘要行缺失: %q", errOut.String())
	}
	// 沙箱留档（CLI 派发与网关同留档，M6-W2 语义延续）。
	if _, err := os.Stat(filepath.Join(home, ".yardmaster", "trace", "dispatch-sessions.jsonl")); err != nil {
		t.Fatalf("会话未落盘: %v", err)
	}
}

// 无 -events：jsonl 输出走原透传 + 提取结果行（既有行为不回归）。
func TestDispatchRunWithoutEventsPassthrough(t *testing.T) {
	testenv.New(t)
	writeDispatchJSONL(t)

	var out, errOut strings.Builder
	if err := Run([]string{"dispatch", "run", "任务"}, &out, &errOut); err != nil {
		t.Fatalf("dispatch run: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), `"type":"result"`) {
		t.Fatalf("原文未透传: %q", out.String())
	}
	if !strings.Contains(out.String(), "[dispatch] 结果: CLI 事件终态") {
		t.Fatalf("提取结果行缺失: %q", out.String())
	}
}
