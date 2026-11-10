// provider.rewrite 网关接线集成测试（2026-11-10）：真 node 子进程跑
// magpie-bridge adapter + param-override 中间件，经 HTTP 全链路验证
// handleInference 的 rewrite 钩子——改写生效（假上游收到 temperature=
// 0.2）、return_error 拒绝（客户端 403 且未达上游）、Spec.Options 经
// 握手 settings 透传。node 不可用时跳过。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/plugin"
	"yardmaster/internal/provider"
)

// newBridgeGateway 起一个带 magpie-bridge 插件的网关（假上游记录收到的
// 请求体）。mw 为中间件文件名；options 为插件的静态配置（Spec.Options →
// 握手 settings）。
func newBridgeGateway(t *testing.T, mw string, options map[string]any) (*httptest.Server, *fakeUpstream) {
	return newBridgeGatewayFull(t, mw, options, func(rec recordedReq, n int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	})
}

// newBridgeGatewayFull 同上，假上游响应自定义（回复掩码类测试需要响应
// 含特定文本）。
func newBridgeGatewayFull(t *testing.T, mw string, options map[string]any,
	respond func(rec recordedReq, n int) (int, string, string)) (*httptest.Server, *fakeUpstream) {
	return newBridgeGatewayProv(t, mw, options, nil, respond)
}

// newBridgeGatewayProv 指定上游 Provider 形态（nil=chat 默认）——codex-
// history 等 responses 同协议透传测试用。
func newBridgeGatewayProv(t *testing.T, mw string, options map[string]any,
	prov *provider.Provider, respond func(rec recordedReq, n int) (int, string, string)) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 不在 PATH，跳过 rewrite 接线集成测试")
	}
	adapter, err := filepath.Abs(filepath.Join("..", "..", "plugins", "magpie-bridge", "adapter.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	mwPath := filepath.Join(filepath.Dir(adapter), mw)

	fu := newFakeUpstream(t, respond)
	reg := plugin.NewRegistry()
	spec := plugin.Spec{
		ID:           "param-override-test",
		Command:      []string{node, adapter, "param-override-test", mwPath},
		Enabled:      true,
		Capabilities: []string{plugin.CapRewrite, plugin.CapRewriteResp, plugin.CapRewriteEvent},
		Options:      options,
	}
	reg.Start(plugin.File{Plugins: []plugin.Spec{spec}})
	t.Cleanup(reg.Stop)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		for _, v := range reg.Views() {
			if v.ID == spec.ID && v.Status == plugin.StatusRunning {
				ok = true
			}
		}
		if ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if prov == nil {
		p := chatProvider(fu.srv.URL)
		prov = &p
	} else if prov.BaseURL == "" {
		prov.BaseURL = fu.srv.URL // 空 BaseURL 绑定假上游（responsesProvider("") 等形态复用）
	}
	s := &Server{Planner: NewPlanner([]provider.Provider{*prov}, nil), Version: "test"}
	s.AttachPlugins(reg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, fu
}

// newRewriteGateway 兼容封装（param-override 载体）。
func newRewriteGateway(t *testing.T, options map[string]any) (*httptest.Server, *fakeUpstream) {
	return newBridgeGateway(t, "param-override.middleware.js", options)
}

// TestRewriteHookChangesUpstreamBody：Spec.Options 的 temperature 设置经
// 钩子生效——假上游收到的 body 已被改写，客户端照常 200。
func TestRewriteHookChangesUpstreamBody(t *testing.T) {
	srv, fu := newRewriteGateway(t, map[string]any{"temperature": 0.2})
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatalf("上游 body 非 JSON: %v", err)
	}
	if body["temperature"] != 0.2 {
		t.Fatalf("上游 body 的 temperature 应被改写为 0.2: %v", body)
	}
	if _, ok := body["messages"]; !ok {
		t.Fatal("未改动字段应原样保留")
	}
}

// TestRewriteHookReject：return_error 拒绝 → 客户端 403，请求未达上游，
// trace 留 fail。
func TestRewriteHookReject(t *testing.T) {
	srv, fu := newRewriteGateway(t, map[string]any{
		"operations": []any{
			map[string]any{"mode": "return_error",
				"value": map[string]any{"status": 403, "message": "blocked by policy"}},
		},
	})
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("up-m", false))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("客户端应 403: %d", resp.StatusCode)
	}
	if calls := fu.calls(); len(calls) != 0 {
		t.Fatalf("请求不应到达上游: %d", len(calls))
	}
}

// TestRewriteHookNoPlugin：无 rewrite 插件时请求原样通过（回归——接线
// 不得影响无插件路径）。
func TestRewriteHookNoPlugin(t *testing.T) {
	fu := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	})
	srv := newGateway(t, chatProvider(fu.srv.URL))
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["temperature"]; ok {
		t.Fatal("无插件不应出现改写字段")
	}
}

// ---------- word-guard / system-prompt 迁移全链路（2026-11-10） ----------

// TestSystemPromptInjectOnRequest：无系统提示词的请求注入
// system_prompt（chat 形态 = messages 首部 system 消息）。
func TestSystemPromptInjectOnRequest(t *testing.T) {
	srv, fu := newBridgeGateway(t, "system-prompt.middleware.js",
		map[string]any{"system_prompt": "Answer in English."})
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("应注入一条 system 消息（共 2 条）: %v", body.Messages)
	}
	if body.Messages[0].Role != "system" || body.Messages[0].Content != "Answer in English." {
		t.Fatalf("注入不符: %v", body.Messages[0])
	}
	if body.Messages[1].Content != "hi" {
		t.Fatalf("原消息应保留: %v", body.Messages[1])
	}
}

// TestWordGuardRejectRequest：action=reject 命中敏感词 → 请求侧 400，
// 未达上游。
func TestWordGuardRejectRequest(t *testing.T) {
	srv, fu := newBridgeGateway(t, "word-guard.middleware.js",
		map[string]any{"words": []any{"secret"}, "action": "reject"})
	req := `{"model":"prov/up-m","stream":false,"messages":[{"role":"user","content":"tell me about secret docs"}]}`
	resp := post(t, srv.URL, "/v1/chat/completions", req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("应 400: %d", resp.StatusCode)
	}
	if calls := fu.calls(); len(calls) != 0 {
		t.Fatalf("请求不应到达上游: %d", len(calls))
	}
}

// TestWordGuardMaskRequest：action=mask → 上游收到的用户文本被掩码。
func TestWordGuardMaskRequest(t *testing.T) {
	srv, fu := newBridgeGateway(t, "word-guard.middleware.js",
		map[string]any{"words": []any{"secret"}, "action": "mask", "mask": "***"})
	req := `{"model":"prov/up-m","stream":false,"messages":[{"role":"user","content":"tell me about secret docs"}]}`
	resp := post(t, srv.URL, "/v1/chat/completions", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Messages[0].Content != "tell me about *** docs" {
		t.Fatalf("请求文本应被掩码: %q", body.Messages[0].Content)
	}
}

// TestWordGuardMaskReplyNonStream：replies=true → 非流式回复掩码
// （provider.rewrite.response，同协议透传路径）。
func TestWordGuardMaskReplyNonStream(t *testing.T) {
	srv, fu := newBridgeGatewayFull(t, "word-guard.middleware.js",
		map[string]any{"words": []any{"classified"}, "action": "mask", "mask": "***", "replies": true},
		func(rec recordedReq, n int) (int, string, string) {
			return http.StatusOK, "application/json",
				`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"the classified file"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
		})
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `the *** file`) {
		t.Fatalf("回复应被掩码: %s", b)
	}
	if !strings.Contains(string(b), `"total_tokens":8`) {
		t.Fatal("usage 数字应保留（掩码不动数值字段）")
	}
	if calls := fu.calls(); len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
}

// TestWordGuardMaskReplyStream：replies=true → 流式回复掩码
// （provider.rewrite.event，SSE 帧级；跨 chunk 拆词不在感知范围——上游
// 单 chunk 携带完整词）。
func TestWordGuardMaskReplyStream(t *testing.T) {
	srv, fu := newBridgeGatewayFull(t, "word-guard.middleware.js",
		map[string]any{"words": []any{"classified"}, "action": "mask", "mask": "***", "replies": true},
		func(rec recordedReq, n int) (int, string, string) {
			return http.StatusOK, "text/event-stream",
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"up-m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
					"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"up-m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"the classified file\"},\"finish_reason\":null}]}\n\n" +
					"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"up-m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: [DONE]\n\n"
		})
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `the *** file`) {
		t.Fatalf("流式内容应被掩码: %s", b)
	}
	if strings.Contains(string(b), "classified") {
		t.Fatal("流式内容不应残留敏感词")
	}
	if !strings.Contains(string(b), "data: [DONE]") {
		t.Fatal("帧边界与 [DONE] 应保留")
	}
	if calls := fu.calls(); len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
}

// ---------- output-strategy / history-trim / codex-history 迁移（2026-11-10） ----------

// TestOutputStrategyInjectOnRequest：styles 选中 → chat 形态注入 system
// 消息（含 [Output Strategy] marker），原消息保留。
func TestOutputStrategyInjectOnRequest(t *testing.T) {
	srv, fu := newBridgeGateway(t, "output-strategy.middleware.js",
		map[string]any{"styles": map[string]any{"terse-prose": true}})
	resp := post(t, srv.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("应注入一条 system（共 2 条）: %v", body.Messages)
	}
	if body.Messages[0].Role != "system" || !strings.Contains(body.Messages[0].Content, "[Output Strategy]") {
		t.Fatalf("system 注入不符: %+v", body.Messages[0])
	}
	if body.Messages[1].Content != "hi" {
		t.Fatalf("原消息应保留: %+v", body.Messages[1])
	}
}

// TestHistoryTrimCutToolResult：历史中超长的 tool 结果按 maxToolLength
// 词边界截断；tool 之后有 assistant 收尾（tool 才在历史内），最后的 user
// 是当前回合——四步全部不动。
func TestHistoryTrimCutToolResult(t *testing.T) {
	long := strings.Repeat("word ", 80) // 400 字符 > 300
	srv, fu := newBridgeGateway(t, "history-trim.middleware.js",
		map[string]any{"maxToolLength": 300})
	req := fmt.Sprintf(`{"model":"prov/up-m","stream":false,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"checking"},
		{"role":"tool","content":%q},
		{"role":"assistant","content":"done"},
		{"role":"user","content":"thanks"}]}`, long)
	resp := post(t, srv.URL, "/v1/chat/completions", req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("客户端应 200: %d", resp.StatusCode)
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 5 {
		t.Fatalf("消息数应不变: %d", len(body.Messages))
	}
	tool := body.Messages[2]
	if !strings.HasSuffix(tool.Content, "\n...[truncated]") {
		t.Fatalf("tool 结果应被截断标记: %q", tool.Content)
	}
	if len(tool.Content) >= len(long) {
		t.Fatalf("截断后应更短: %d", len(tool.Content))
	}
	if body.Messages[4].Content != "thanks" {
		t.Fatalf("当前回合应不动: %+v", body.Messages[4])
	}
}

// TestCodexHistoryFiltersResponses：responses 同协议透传，input 里的
// reasoning 项被滤除、store=false 注入（按 provider 规则显式开启）。
func TestCodexHistoryFiltersResponses(t *testing.T) {
	p := responsesProvider("")
	srv, fu := newBridgeGatewayProv(t, "codex-history.middleware.js",
		map[string]any{
			"providers": map[string]any{"magpie:prov": map[string]any{"historyFilter": true}},
		},
		&p,
		func(rec recordedReq, n int) (int, string, string) {
			return http.StatusOK, "application/json",
				`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`
		})
	req := `{"model":"prov/up-m","stream":false,"input":[
		{"type":"reasoning","summary":[]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	resp := post(t, srv.URL, "/v1/responses", req)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("客户端应 200: %d, body: %s, 上游收到: %v", resp.StatusCode, b, fu.calls())
	}
	calls := fu.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次调用: %d", len(calls))
	}
	var body struct {
		Store bool             `json:"store"`
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(calls[0].body), &body); err != nil {
		t.Fatalf("上游 body 非 JSON: %v (%s)", err, calls[0].body)
	}
	if len(body.Input) != 1 {
		t.Fatalf("reasoning 项应被滤除（剩 1 条）: %v", body.Input)
	}
	if body.Input[0]["type"] != "message" {
		t.Fatalf("保留的应为 message: %v", body.Input[0])
	}
}

// TestCodexHistoryRejectsNonPortable：previous_response_id 引用不可迁移
// → 显式拒绝（FilterError 默认 400，绝不静默丢弃历史），未达上游。
func TestCodexHistoryRejectsNonPortable(t *testing.T) {
	p := responsesProvider("")
	srv, fu := newBridgeGatewayProv(t, "codex-history.middleware.js",
		map[string]any{
			"providers": map[string]any{"magpie:prov": map[string]any{"historyFilter": true}},
		},
		&p,
		func(rec recordedReq, n int) (int, string, string) {
			return http.StatusOK, "application/json", `{"id":"resp_1","object":"response","status":"completed","output":[]}`
		})
	req := `{"model":"prov/up-m","stream":false,"previous_response_id":"resp_old","input":"hello"}`
	resp := post(t, srv.URL, "/v1/responses", req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("应 400: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "Codex history filter") {
		t.Fatalf("拒绝原因应透传: %s", b)
	}
	if calls := fu.calls(); len(calls) != 0 {
		t.Fatalf("请求不应到达上游: %d", len(calls))
	}
}
