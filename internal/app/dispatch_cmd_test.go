// dispatch run -events 测试（M6-W3）：事件流实时渲染 + text 回退透传。
// 假 Agent 用测试二进制自身（TestDispatchJSONLHelper），HOME 沙箱经
// testenv 隔离，不碰真机 Agent、不碰网络。
package app

import (
	"fmt"
	"io"
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
	if os.Getenv("YM_DSP_HELPER_BEHAVIOR") == "text" { // M6-W4 模板测试：回显任务字节数
		task, _ := io.ReadAll(os.Stdin)
		fmt.Printf("收到任务(%d字节)\n", len(task))
		fmt.Println("结果是 42")
		os.Exit(0)
	}
	fmt.Println(`{"type":"system","subtype":"init"}`)
	fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"事件中的文本"}]}}`)
	fmt.Println(`{"type":"result","subtype":"success","result":"CLI 事件终态"}`)
	os.Exit(0)
}

// writeDispatchFiles 在沙箱 HOME 写 dispatch.json（假 Agent default 兜底，
// 可选挂模板）。
func writeDispatchFiles(t *testing.T, templates []dispatch.Template) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := dispatch.AgentSpec{
		ID: "fake", Output: "jsonl", Default: true,
		Command: []string{exe, "-test.run=TestDispatchJSONLHelper", "--"},
		Env:     []string{"YM_DSP_HELPER_ON=1", "YM_DSP_HELPER_BEHAVIOR=" + os.Getenv("YM_DSP_TEST_BEHAVIOR")},
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.AppDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJSON(paths.DispatchPath(), dispatch.DispatchFile{
		Agents: []dispatch.AgentSpec{spec}, Templates: templates,
	}); err != nil {
		t.Fatal(err)
	}
}

// writeDispatchJSONL 在沙箱 HOME 写 dispatch.json（jsonl 假 Agent，default 兜底）。
func writeDispatchJSONL(t *testing.T) {
	t.Helper()
	t.Setenv("YM_DSP_TEST_BEHAVIOR", "")
	writeDispatchFiles(t, nil)
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

// -t 模板：占位实例化（「向 {name} 问好」+ name=世界 → 任务 17 字节，
// 假 Agent 回显字节数为证）+ 模板默认 agent 生效。
func TestDispatchRunTemplateFlag(t *testing.T) {
	testenv.New(t)
	t.Setenv("YM_DSP_TEST_BEHAVIOR", "text")
	writeDispatchFiles(t, []dispatch.Template{
		{Name: "greet", Task: "向 {name} 问好", Agent: "fake"},
	})

	var out, errOut strings.Builder
	if err := Run([]string{"dispatch", "run", "-t", "greet", "name=世界"}, &out, &errOut); err != nil {
		t.Fatalf("dispatch run -t: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	// UTF-8 字节数：向(3)+空格(1)+世界(6)+空格(1)+问好(6) = 17。
	if !strings.Contains(out.String(), "收到任务(17字节)") {
		t.Fatalf("模板实例化未生效: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "选中 fake") {
		t.Fatalf("模板默认 agent 未生效: %q", errOut.String())
	}
}

// -t fail-closed：缺参/多传键/未知模板/参数缺等号 → 报错拒绝（不派发）。
func TestDispatchRunTemplateFailClosed(t *testing.T) {
	testenv.New(t)
	t.Setenv("YM_DSP_TEST_BEHAVIOR", "text")
	writeDispatchFiles(t, []dispatch.Template{
		{Name: "greet", Task: "向 {name} 问好", Agent: "fake"},
	})
	for _, args := range [][]string{
		{"dispatch", "run", "-t", "greet"},                  // 缺参
		{"dispatch", "run", "-t", "greet", "name=a", "x=b"}, // 多传键
		{"dispatch", "run", "-t", "ghost", "name=a"},        // 未知模板
		{"dispatch", "run", "-t", "greet", "name"},          // 参数缺等号
	} {
		var out, errOut strings.Builder
		if err := Run(args, &out, &errOut); err == nil {
			t.Fatalf("应拒绝: %v\nstdout=%s", args, out.String())
		}
	}
}
