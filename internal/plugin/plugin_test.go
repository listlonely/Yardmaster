// M7-W3 插件运行时测试：fail-closed 校验、握手/事件/解析钩子端到端、
// 崩溃退避重启、握手失败降级、能力熔断。假插件用测试二进制自身
// （TestPluginHelperProcess，不碰网络、不依赖外部运行时）。
package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidatePluginFile(t *testing.T) {
	ok := Spec{ID: "p", Command: []string{"x"}, Capabilities: []string{CapEvents}}
	cases := []struct {
		name string
		f    File
		want string
	}{
		{"id 空", File{Plugins: []Spec{{Command: []string{"x"}}}}, "id 不能为空"},
		{"id 重复", File{Plugins: []Spec{ok, ok}}, "重复"},
		{"command 空", File{Plugins: []Spec{{ID: "p"}}}, "command 不能为空"},
		{"未知能力", File{Plugins: []Spec{{ID: "p", Command: []string{"x"}, Capabilities: []string{"nope"}}}}, "未知能力"},
		{"超时越界", File{Plugins: []Spec{{ID: "p", Command: []string{"x"}, TimeoutMS: 50}}}, "100~30000"},
		{"合法通过", File{Plugins: []Spec{ok}}, ""},
	}
	for _, c := range cases {
		err := Validate(c.f)
		if c.want == "" {
			if err != nil {
				t.Fatalf("%s: 意外失败: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want 含 %q", c.name, err, c.want)
		}
	}
	// Load：不存在 → ErrNotExist；损坏 → 错误（fail-closed 由调用方禁用）。
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "plugins.json")); err == nil {
		t.Fatal("不存在应返回错误")
	}
	mustWrite(t, filepath.Join(dir, "plugins.json"), "{broken")
	if _, err := Load(filepath.Join(dir, "plugins.json")); err == nil {
		t.Fatal("损坏应返回错误")
	}
}

// TestPluginHelperProcess 不是真正的测试：作为假插件子进程运行。
func TestPluginHelperProcess(t *testing.T) {
	if os.Getenv("GO_PLUGIN_HELPER") != "1" {
		return
	}
	behavior := os.Getenv("GO_PLUGIN_BEHAVIOR")
	out := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 1<<20)
	switch behavior {
	case "ok":
		out(map[string]any{"plugin": "ok", "proto": 1, "capabilities": []string{CapEvents, CapDispatch}})
		in.Scan() // 父回执
		for in.Scan() {
			var env struct {
				Type    string          `json:"type"`
				ID      string          `json:"id"`
				Payload json.RawMessage `json:"payload"`
			}
			if json.Unmarshal(in.Bytes(), &env) != nil {
				continue
			}
			switch env.Type {
			case "event":
				fmt.Fprintln(os.Stderr, "got-event")
			case "parse":
				out(map[string]any{"v": 1, "type": "resp", "id": env.ID,
					"payload": map[string]any{"events": []map[string]any{{"kind": "text", "text": "插件解析的文本"}}}})
			case "ping":
				out(map[string]any{"v": 1, "type": "pong"})
			}
		}
	case "crash": // 握手后立即退出（崩溃重启载体）
		out(map[string]any{"plugin": "crash", "proto": 1, "capabilities": []string{CapEvents}})
		in.Scan()
		os.Exit(3)
	case "nohandshake": // 不握手（握手失败降级载体）
		time.Sleep(30 * time.Second)
	case "slow": // 握手后响应挂起（熔断载体）
		out(map[string]any{"plugin": "slow", "proto": 1, "capabilities": []string{CapDispatch}})
		in.Scan()
		for in.Scan() {
			var env struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if json.Unmarshal(in.Bytes(), &env) != nil {
				continue
			}
			if env.Type == "ping" {
				out(map[string]any{"v": 1, "type": "pong"})
			}
		}
	}
	os.Exit(0)
}

func helperSpec(t *testing.T, behavior string, caps ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{
		ID: behavior, Enabled: true, Capabilities: caps, TimeoutMS: 300,
		Command: []string{exe, "-test.run=TestPluginHelperProcess", "--"},
		Env:     []string{"GO_PLUGIN_HELPER=1", "GO_PLUGIN_BEHAVIOR=" + behavior},
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// 端到端：握手（granted=配置∩声明）→ 事件订阅到达 → 解析钩子往返。
func TestPluginLifecycleEndToEnd(t *testing.T) {
	reg := NewRegistry("")
	reg.backoff = 10 * time.Millisecond
	spec := helperSpec(t, "ok", CapEvents, CapDispatch)
	reg.Start(File{Plugins: []Spec{spec}})
	defer reg.Stop()

	waitFor(t, "插件 running", func() bool {
		for _, v := range reg.Views() {
			if v.Status == StatusRunning {
				return true
			}
		}
		return false
	})
	var view StatusView
	for _, v := range reg.Views() {
		if v.ID == "ok" {
			view = v
		}
	}
	if len(view.Granted) != 2 {
		t.Fatalf("granted 应为 2 项能力: %+v", view.Granted)
	}

	// 事件订阅：payload 到达插件（helper 以 stderr 行回执）。
	reg.TraceEvent(map[string]any{"id": "d-1", "outcome": "ok"})
	waitFor(t, "事件到达", func() bool {
		for _, l := range reg.LogTail("ok") {
			if strings.Contains(l, "got-event") {
				return true
			}
		}
		return false
	})

	// 解析钩子：请求/响应往返。
	raw, err := reg.ParseLine("ok", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "插件解析的文本") {
		t.Fatalf("解析响应: %s", string(raw))
	}
}

// 崩溃退避重启：握手后自杀 → backoff 10ms → 重建实例回 running；
// Crash1H 累计。
func TestPluginCrashRestart(t *testing.T) {
	reg := NewRegistry("")
	reg.backoff = 10 * time.Millisecond
	reg.Start(File{Plugins: []Spec{helperSpec(t, "crash", CapEvents)}})
	defer reg.Stop()

	waitFor(t, "至少两次崩溃且仍在守候", func() bool {
		for _, v := range reg.Views() {
			if v.ID == "crash" && v.Crash1H >= 2 {
				return true
			}
		}
		return false
	})
	for _, v := range reg.Views() {
		if v.ID == "crash" && v.Status == StatusFailed {
			t.Fatal("未超限不应标 failed")
		}
	}
}

// 握手失败降级：不握手的插件永不 running，能力请求 ErrNoPlugin。
func TestPluginHandshakeFailDegraded(t *testing.T) {
	reg := NewRegistry("")
	reg.backoff = 10 * time.Millisecond
	reg.Start(File{Plugins: []Spec{helperSpec(t, "nohandshake", CapEvents)}})
	defer reg.Stop()

	time.Sleep(200 * time.Millisecond)
	for _, v := range reg.Views() {
		if v.Status == StatusRunning {
			t.Fatal("未握手不应 running")
		}
	}
	if _, err := reg.RequestByCap(CapEvents, "x", nil, 50*time.Millisecond); err != ErrNoPlugin {
		t.Fatalf("应 ErrNoPlugin: %v", err)
	}
}

// 能力熔断：响应挂起的插件连续 3 次超时 → ErrCooldown。
func TestRegistryCooldown(t *testing.T) {
	reg := NewRegistry("")
	reg.backoff = 10 * time.Millisecond
	reg.Start(File{Plugins: []Spec{helperSpec(t, "slow", CapDispatch)}})
	defer reg.Stop()

	waitFor(t, "插件 running", func() bool {
		for _, v := range reg.Views() {
			if v.Status == StatusRunning {
				return true
			}
		}
		return false
	})
	for i := 0; i < 3; i++ {
		if _, err := reg.RequestByCap(CapDispatch, "parse", map[string]any{"line": "x"}, 100*time.Millisecond); err == nil {
			t.Fatal("挂起插件应超时")
		}
	}
	if _, err := reg.RequestByCap(CapDispatch, "parse", map[string]any{"line": "x"}, 100*time.Millisecond); err != ErrCooldown {
		t.Fatalf("第 4 次应 ErrCooldown: %v", err)
	}
}

// Reload：禁用翻转 → 实例重建停止；恢复启用 → 重新运行。
func TestRegistryReloadToggle(t *testing.T) {
	reg := NewRegistry("")
	reg.backoff = 10 * time.Millisecond
	spec := helperSpec(t, "ok", CapEvents)
	reg.Start(File{Plugins: []Spec{spec}})
	defer reg.Stop()
	waitFor(t, "running", func() bool { return reg.Views()[0].Status == StatusRunning })

	disabled := spec
	disabled.Enabled = false
	reg.Reload(File{Plugins: []Spec{disabled}})
	waitFor(t, "禁用后停止", func() bool { return reg.Views()[0].Status == StatusStopped })

	reg.Reload(File{Plugins: []Spec{spec}})
	waitFor(t, "重新启用后 running", func() bool { return reg.Views()[0].Status == StatusRunning })
}

var _ = io.Discard
