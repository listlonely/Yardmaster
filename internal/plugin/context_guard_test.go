// context-guard 中间件集成测试（2026-11-11）：真 node 子进程跑
// plugins/magpie-bridge/adapter.mjs + context-guard 中间件，经 Registry
// 的 rewrite 钩子全链路验证——未超限放行、超限裁剪（tool_use/tool_result
// 配对完整 + 尾部 tool_result 截断）、异步摘要经假上游落缓存（YM_APP_DIR
// 注入生效）、第二次同前缀命中缓存、1MB 级 body 在 800ms 内返回。
// node 不可用时跳过（CI Linux 有 node）。
package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startGuard 起一个 context-guard 插件实例（appDir 注入 YM_APP_DIR），
// 返回注册表与缓存文件路径（appDir/context-guard-cache.json）。
func startGuard(t *testing.T, options map[string]any) (*Registry, string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node 不在 PATH，跳过 context-guard 集成测试")
	}
	appDir := t.TempDir()
	r := NewRegistry(appDir)
	r.backoff = 100 * time.Millisecond
	spec := Spec{
		ID:           "context-guard-test",
		Command:      magpieBridgeCmd(t, "context-guard.middleware.js"),
		Enabled:      true,
		Capabilities: []string{CapRewrite},
		TimeoutMS:    800,
		Options:      options,
	}
	r.Start(File{Plugins: []Spec{spec}})
	t.Cleanup(r.Stop)
	waitRunning(t, r, "context-guard-test")
	return r, filepath.Join(appDir, "context-guard-cache.json")
}

// guardBody 构造超限的多轮 body：两轮工具调用（tool_result 各塞长文本），
// 末轮纯文本。轮起点 [0, 4, 8]，keep_recent_turns=1 → 切点 4。
func guardBody(longLen int) map[string]any {
	long := strings.Repeat("word ", longLen/5) // longLen 字符
	return map[string]any{
		"model": "m1",
		"messages": []any{
			map[string]any{"role": "user", "content": "task begin"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "bash", "input": map[string]any{"cmd": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": long},
			}},
			map[string]any{"role": "assistant", "content": "done step 1"},
			map[string]any{"role": "user", "content": "next step"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t2", "name": "bash", "input": map[string]any{"cmd": "pwd"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": long},
			}},
			map[string]any{"role": "assistant", "content": "done step 2"},
			map[string]any{"role": "user", "content": "final"},
		},
	}
}

func msgList(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	arr, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages 缺失: %v", body)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, m := range arr {
		out = append(out, m.(map[string]any))
	}
	return out
}

// TestContextGuardBelowTrigger：未达阈值原样放行。
func TestContextGuardBelowTrigger(t *testing.T) {
	r, _ := startGuard(t, map[string]any{"trigger_tokens": 160000})
	out := rewrite(t, r, map[string]any{
		"body": map[string]any{"model": "m1", "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		}},
		"model": "m1", "agent": "t",
	})
	if out["changed"] != false {
		t.Fatalf("未超限应放行: %v", out)
	}
}

// TestContextGuardLocalTrim：超限+缓存未命中（无摘要通道）→ 本地裁剪：
// 前缀替换为占位、tool_use/tool_result 配对完整、保留轮的超长 tool_result
// 被截断。
func TestContextGuardLocalTrim(t *testing.T) {
	r, cachePath := startGuard(t, map[string]any{
		"trigger_tokens": 100, "target_tokens": 500, "keep_recent_turns": 1,
		"tail_tool_max_chars": 4000,
	})
	out := rewrite(t, r, map[string]any{"body": guardBody(8000), "model": "m1", "agent": "trim-t"})
	if out["changed"] != true {
		t.Fatalf("超限应改写: %v", out)
	}
	msgs := msgList(t, bridgeBody(t, out))
	if len(msgs) != 6 {
		t.Fatalf("占位+保留 5 条应为 6 条: %d", len(msgs))
	}
	// 占位消息（首条 user，含 marker）
	if msgs[0]["role"] != "user" {
		t.Fatalf("首条应为 user 占位: %v", msgs[0])
	}
	if !strings.Contains(msgs[0]["content"].(string), "[context summary]") {
		t.Fatalf("占位应含 marker: %v", msgs[0])
	}
	// 保留部分：tool_use(t2) 与 tool_result 配对完整
	if msgs[2]["role"] != "assistant" {
		t.Fatalf("第 3 条应为 assistant(tool_use): %v", msgs[2])
	}
	toolResult := msgs[3]
	blocks := toolResult["content"].([]any)
	tr := blocks[0].(map[string]any)
	if tr["tool_use_id"] != "t2" {
		t.Fatalf("tool_result 配对应保留: %v", tr)
	}
	// 尾部超长 tool_result 被截断（原 8000 字符 → ≤4000+后缀）
	if got := tr["content"].(string); len(got) >= 8000 || !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("超长 tool_result 应被截断: len=%d", len(got))
	}
	// 纯本地裁剪模式（未配 summary）不产生缓存
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("未配 summary 不应写缓存: %v", err)
	}
}

// summaryFake 起一个假摘要上游（openai 形态，返回固定文本），记录请求体。
func summaryFake(t *testing.T, reply string) (*httptest.Server, func() int) {
	t.Helper()
	calls := 0
	mu := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu <- struct{}{}
		calls++
		<-mu
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "sum-m" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu <- struct{}{}; n := calls; <-mu; return n }
}

// TestContextGuardSummaryCache：配置摘要通道 → 第一次触发（占位"摘要生成
// 中"）+ 异步摘要经假上游完成并落缓存（YM_APP_DIR 注入生效）；第二次同
// 前缀请求命中缓存，占位替换为真实摘要文本。
func TestContextGuardSummaryCache(t *testing.T) {
	srv, calls := summaryFake(t, "任务目标：修 bug。已完成：改了 a.go。")
	r, cachePath := startGuard(t, map[string]any{
		"trigger_tokens": 100, "target_tokens": 500, "keep_recent_turns": 1,
		"tail_tool_max_chars": 4000,
		"summary": map[string]any{
			"endpoint": srv.URL, "api_key": "sk-test", "model": "sum-m",
			"protocol": "openai", "max_tokens": 100,
		},
	})
	payload := map[string]any{"body": guardBody(8000), "model": "m1", "agent": "cache-t"}

	// 第一次：占位（摘要生成中）
	out := rewrite(t, r, payload)
	if out["changed"] != true {
		t.Fatalf("第一次超限应改写: %v", out)
	}
	first := msgList(t, bridgeBody(t, out))[0]["content"].(string)
	if !strings.Contains(first, "摘要生成中") {
		t.Fatalf("第一次应为未命中占位: %q", first)
	}

	// 轮询等异步摘要落缓存（fire-and-forget，≤10s）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cachePath); err == nil {
			b, _ := os.ReadFile(cachePath)
			if strings.Contains(string(b), "任务目标：修 bug") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("缓存应落 $YM_APP_DIR: %v", err)
	}
	if !strings.Contains(string(b), "anchorHash") {
		t.Fatalf("缓存条目应含锚点指纹: %s", b)
	}
	if got := calls(); got != 1 {
		t.Fatalf("假摘要上游应收到 1 次调用: %d", got)
	}

	// 第二次：同前缀命中缓存 → 摘要文本替换占位
	out = rewrite(t, r, payload)
	if out["changed"] != true {
		t.Fatalf("第二次应改写: %v", out)
	}
	second := msgList(t, bridgeBody(t, out))[0]["content"].(string)
	if !strings.Contains(second, "任务目标：修 bug") || strings.Contains(second, "摘要生成中") {
		t.Fatalf("第二次应命中缓存摘要: %q", second)
	}
	if got := calls(); got != 1 {
		t.Fatalf("命中路径不应再调摘要: %d", got)
	}
}

// TestContextGuardLargeBody：1MB 级 body 的估算+裁剪在 800ms 内返回
// （rewrite 钩子的网关侧硬超时预算）。
func TestContextGuardLargeBody(t *testing.T) {
	r, _ := startGuard(t, map[string]any{
		"trigger_tokens": 100, "target_tokens": 500, "keep_recent_turns": 1,
		"tail_tool_max_chars": 4000,
	})
	body := guardBody(900000) // ≈1MB JSON
	t0 := time.Now()
	out := rewrite(t, r, map[string]any{"body": body, "model": "m1", "agent": "large-t"})
	elapsed := time.Since(t0)
	if out["changed"] != true {
		t.Fatalf("超限应改写: %v", out)
	}
	if elapsed > 800*time.Millisecond {
		t.Fatalf("1MB 级 body 应在 800ms 内返回: %v", elapsed)
	}
}
