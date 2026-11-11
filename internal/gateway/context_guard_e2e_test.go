// context-guard 网关接线 e2e 测试（2026-11-11）：真 node 子进程跑
// magpie-bridge adapter + context-guard 中间件，经 HTTP 全链路验证
// handleInference 的 rewrite 钩子——超限请求被裁（假上游收到占位+保留
// 轮，tool_result 截断，tool_calls/tool 配对完整），异步摘要落缓存后
// 第二次请求命中（上游收到真实摘要文本）。node 不可用时跳过。
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// guardChatReq 构造超限的 openai chat 入站 body：两轮工具调用（tool
// 内容各塞长文本），末轮纯文本。轮起点 [0,4,8]，keep_recent_turns=1 →
// 切点 4（前缀 [0..3] 的 tool_calls/tool 配对完整裁除）。
func guardChatReq(model string, longLen int) string {
	long := strings.Repeat("word ", longLen/5)
	return fmt.Sprintf(`{"model":%q,"stream":false,"messages":[
		{"role":"user","content":"task begin"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t1","content":%q},
		{"role":"assistant","content":"done step 1"},
		{"role":"user","content":"next step"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"t2","type":"function","function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t2","content":%q},
		{"role":"assistant","content":"done step 2"},
		{"role":"user","content":"final"}]}`, model, long, long)
}

// summaryFakeUpstream 假摘要上游（openai 形态），返回固定摘要文本。
func summaryFakeUpstream(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	return srv
}

// TestContextGuardE2ETrim：超限请求经网关 → 假推理上游收到裁剪后 body：
// 占位 marker、消息数 6、保留轮的 tool 内容截断、第二轮 tool_calls/tool
// 配对完整（t2）；客户端 200。
func TestContextGuardE2ETrim(t *testing.T) {
	srv, fu := newBridgeGateway(t, "context-guard.middleware.js",
		map[string]any{
			"trigger_tokens": 100, "target_tokens": 500, "keep_recent_turns": 1,
			"tail_tool_max_chars": 4000, // 无 summary：纯本地裁剪模式
		})
	resp := post(t, srv.URL, "/v1/chat/completions", guardChatReq("prov/up-m", 8000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
			ToolCallID string         `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatalf("上游 body 非 JSON: %v", err)
	}
	if len(body.Messages) != 6 {
		t.Fatalf("占位+保留 5 条应为 6 条: %d", len(body.Messages))
	}
	var placeholder string
	if err := json.Unmarshal(body.Messages[0].Content, &placeholder); err != nil {
		t.Fatalf("占位 content 非 string: %s", body.Messages[0].Content)
	}
	if !strings.Contains(placeholder, "[context summary]") {
		t.Fatalf("占位应含 marker: %q", placeholder)
	}
	// 保留部分：user("next step") → assistant(tool_calls t2) → tool(t2，
	// 内容截断) → assistant → user("final")。tool_use 的 id 在 assistant
	// 的 tool_calls[0].id 里，tool 消息的 tool_call_id 与之配对。
	if body.Messages[1].Role != "user" {
		t.Fatalf("Messages[1] 应为保留轮起点的 user: %+v", body.Messages[1])
	}
	if len(body.Messages[2].ToolCalls) == 0 || !strings.Contains(string(body.Messages[2].ToolCalls), `"t2"`) {
		t.Fatalf("Messages[2] assistant 的 tool_calls 应含 t2: %s", body.Messages[2].ToolCalls)
	}
	if body.Messages[3].ToolCallID != "t2" {
		t.Fatalf("Messages[3] tool 消息应与 t2 配对: %+v", body.Messages[3])
	}
	var toolContent string
	if err := json.Unmarshal(body.Messages[3].Content, &toolContent); err != nil {
		t.Fatalf("tool content 非 string: %s", body.Messages[3].Content)
	}
	if len(toolContent) >= 8000 || !strings.HasSuffix(toolContent, "...[truncated]") {
		t.Fatalf("保留轮的超长 tool 内容应被截断: len=%d", len(toolContent))
	}
}

// TestContextGuardE2ESummaryCache：配置摘要通道 → 第一次裁剪（占位），
// 异步摘要落缓存后重发同 body → 假推理上游收到真实摘要文本（命中路径）。
func TestContextGuardE2ESummaryCache(t *testing.T) {
	sumSrv := summaryFakeUpstream(t, "任务目标：修 bug。已完成：改了 a.go。")
	srv, fu := newBridgeGateway(t, "context-guard.middleware.js",
		map[string]any{
			"trigger_tokens": 100, "target_tokens": 500, "keep_recent_turns": 1,
			"tail_tool_max_chars": 4000,
			"summary": map[string]any{
				"endpoint": sumSrv.URL, "api_key": "sk-test", "model": "sum-m",
				"protocol": "openai", "max_tokens": 100,
			},
		})
	req := guardChatReq("prov/up-m", 8000)
	if resp := post(t, srv.URL, "/v1/chat/completions", req); resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}

	// 轮询重发同 body，直到命中缓存（上游 body 含摘要文本；fire-and-forget
	// 摘要通常一秒内落盘）。
	deadline := time.Now().Add(10 * time.Second)
	hit := false
	for time.Now().Before(deadline) {
		if resp := post(t, srv.URL, "/v1/chat/completions", req); resp.StatusCode != http.StatusOK {
			t.Fatalf("重发应 200: %d", resp.StatusCode)
		}
		calls := fu.calls()
		if len(calls) == 0 {
			t.Fatal("上游应收到调用")
		}
		if strings.Contains(calls[len(calls)-1].body, "任务目标：修 bug") {
			hit = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !hit {
		t.Fatal("缓存未在期限内命中（上游未收到摘要文本）")
	}
	// 命中路径：占位消失，摘要文本替换为上游可见的首条消息
	calls := fu.calls()
	last := calls[len(calls)-1].body
	if strings.Contains(last, "摘要生成中") {
		t.Fatalf("命中路径不应残留未命中占位: %s", last)
	}
}
