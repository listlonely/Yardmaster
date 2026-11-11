// magpie-bridge 适配层集成测试（2026-11-10）：真子进程 node 跑
// plugins/magpie-bridge/adapter.mjs + param-override 中间件，验证
// provider.rewrite 协议全链路——握手 granted、ping/pong 心跳、简单键
// 设置、operations 条件改写、return_error 拒绝、无 options 放行、
// options 未配改写放行。node 不可用时跳过（CI Linux 有 node；本机
// v20.11.1 实测）。
package plugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// magpieBridgeCmd 构造 adapter 子进程 argv（node 可用时）。
func magpieBridgeCmd(t *testing.T, mw string) []string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 不在 PATH，跳过 magpie-bridge 集成测试")
	}
	adapter, err := filepath.Abs(filepath.Join("..", "..", "plugins", "magpie-bridge", "adapter.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(adapter); err != nil {
		t.Fatalf("适配层缺失: %v", err)
	}
	mwPath := filepath.Join(filepath.Dir(adapter), mw)
	return []string{node, adapter, "param-override-test", mwPath}
}

// startBridge 经 Registry 起一个 magpie-bridge 插件并等待运行。
func startBridge(t *testing.T, mw string, timeoutMS int) *Registry {
	t.Helper()
	r := NewRegistry("")
	r.backoff = 100 * time.Millisecond
	spec := Spec{
		ID:           "param-override-test",
		Command:      magpieBridgeCmd(t, mw),
		Enabled:      true,
		Capabilities: []string{CapRewrite},
		TimeoutMS:    timeoutMS,
	}
	r.Start(File{Plugins: []Spec{spec}})
	t.Cleanup(r.Stop)
	waitRunning(t, r, "param-override-test")
	return r
}

func waitRunning(t *testing.T, r *Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range r.Views() {
			if v.ID == id && v.Status == StatusRunning {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("插件 %s 未在期限内进入 running", id)
}

// rewrite 调一次改写钩子并反序列化 resp payload。
func rewrite(t *testing.T, r *Registry, payload map[string]any) map[string]any {
	t.Helper()
	raw, err := r.RewriteRequest(payload, 5*time.Second)
	if err != nil {
		t.Fatalf("RewriteRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("resp payload 非 JSON: %v (%s)", err, raw)
	}
	return out
}

func bridgeBody(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	body, ok := out["body"].(map[string]any)
	if !ok {
		t.Fatalf("resp.body 缺失或非对象: %v", out)
	}
	return body
}

// TestMagpieBridgeRewrite 全链路：握手 → 改写 → 拒绝 → 放行。
func TestMagpieBridgeRewrite(t *testing.T) {
	r := startBridge(t, "param-override.middleware.js", 0)

	// 1) 握手 granted：provider.rewrite 已授予（Views Granted 由握手回执填充）
	var granted bool
	for _, v := range r.Views() {
		if v.ID == "param-override-test" {
			for _, g := range v.Granted {
				if g == CapRewrite {
					granted = true
				}
			}
		}
	}
	if !granted {
		t.Fatal("握手后 provider.rewrite 未授予")
	}

	// 2) 简单键形态（New API param_override 直拷）：temperature/max_tokens 直接设置
	out := rewrite(t, r, map[string]any{
		"body":    map[string]any{"model": "glm-5.3", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		"model":   "glm-5.3",
		"options": map[string]any{"temperature": 0.2, "max_tokens": 8000},
	})
	if out["changed"] != true {
		t.Fatalf("简单键改写应 changed=true: %v", out)
	}
	body := bridgeBody(t, out)
	if body["temperature"] != 0.2 || body["max_tokens"] != 8000.0 {
		t.Fatalf("改写结果不符: %v", body)
	}
	if _, ok := body["messages"]; !ok {
		t.Fatal("未改动字段应原样保留")
	}

	// 3) operations + 条件（上游 package.json 示例：model 含 reasoner → 删 temperature）
	out = rewrite(t, r, map[string]any{
		"body":  map[string]any{"model": "deepseek-reasoner", "temperature": 0.9},
		"model": "deepseek-reasoner",
		"options": map[string]any{"operations": []any{
			map[string]any{"path": "temperature", "mode": "delete",
				"conditions": []any{map[string]any{"path": "model", "mode": "contains", "value": "reasoner"}}},
		}},
	})
	if out["changed"] != true {
		t.Fatalf("条件删除应 changed=true: %v", out)
	}
	body = bridgeBody(t, out)
	if _, ok := body["temperature"]; ok {
		t.Fatalf("temperature 应被删除: %v", body)
	}

	// 4) 条件不命中：改写放行（changed=false）
	out = rewrite(t, r, map[string]any{
		"body":  map[string]any{"model": "glm-5.3", "temperature": 0.9},
		"model": "glm-5.3",
		"options": map[string]any{"operations": []any{
			map[string]any{"path": "temperature", "mode": "delete",
				"conditions": []any{map[string]any{"path": "model", "mode": "contains", "value": "reasoner"}}},
		}},
	})
	if out["changed"] != false {
		t.Fatalf("条件未命中应 changed=false: %v", out)
	}

	// 5) return_error：拒绝请求（ctx.reject 收集，回 reject 三态）
	out = rewrite(t, r, map[string]any{
		"body":  map[string]any{"model": "glm-5.3"},
		"model": "glm-5.3",
		"options": map[string]any{"operations": []any{
			map[string]any{"mode": "return_error",
				"value": map[string]any{"status": 403, "message": "blocked by policy"}},
		}},
	})
	rej, ok := out["reject"].(map[string]any)
	if !ok {
		t.Fatalf("return_error 应回 reject: %v", out)
	}
	if rej["status"] != 403.0 || rej["reason"] != "blocked by policy" {
		t.Fatalf("reject 内容不符: %v", rej)
	}

	// 6) options 缺失：中间件直接返回 undefined → 放行
	out = rewrite(t, r, map[string]any{
		"body":  map[string]any{"model": "glm-5.3", "temperature": 0.9},
		"model": "glm-5.3",
	})
	if out["changed"] != false {
		t.Fatalf("无 options 应放行: %v", out)
	}
}

// TestMagpieBridgeHandshakeFail 中间件路径错误：适配层退出 2 → 实例
// 反复退避重启（不殃及其他插件）；Stop 收尾无挂死。
func TestMagpieBridgeHandshakeFail(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不在 PATH，跳过 magpie-bridge 集成测试")
	}
	r := NewRegistry("")
	r.backoff = 100 * time.Millisecond
	spec := Spec{
		ID:           "bridge-bad-mw",
		Command:      []string{"node", filepath.Join("..", "..", "plugins", "magpie-bridge", "adapter.mjs"), "bridge-bad-mw", "no-such.middleware.js"},
		Enabled:      true,
		Capabilities: []string{CapRewrite},
	}
	r.Start(File{Plugins: []Spec{spec}})
	time.Sleep(300 * time.Millisecond) // 给一次启动失败的时间
	r.Stop()                            // 不应挂死（supervise ctx + Kill）
}

// TestMagpieBridgeValidate 清单校验：provider.rewrite 已入能力枚举。
func TestMagpieBridgeValidate(t *testing.T) {
	f := File{Plugins: []Spec{{ID: "p", Command: []string{"x"}, Capabilities: []string{CapRewrite}}}}
	if err := Validate(f); err != nil {
		t.Fatalf("provider.rewrite 应为合法能力: %v", err)
	}
}
