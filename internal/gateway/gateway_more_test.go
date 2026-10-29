// W2 覆盖率补充测试：非流翻译、工具流式、端点与 SSE 扫描边界。
package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agenthub/internal/provider"
)

func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

func TestNonStreamMessagesToChat(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "application/json",
			`{"id":"chatcmpl-1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"答案"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/messages", `{"model":"prov/up-m","max_tokens":64,"messages":[{"role":"user","content":"问"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var ms MessagesResponse
	if err := json.Unmarshal(b, &ms); err != nil {
		t.Fatalf("客户端响应解析: %v\n%s", err, b)
	}
	if len(ms.Content) != 1 || ms.Content[0].Text != "答案" || ms.StopReason != "end_turn" {
		t.Fatalf("翻译结果: %+v", ms)
	}
	if ms.Usage.InputTokens != 7 || ms.Usage.OutputTokens != 2 {
		t.Fatalf("用量未映射: %+v", ms.Usage)
	}
	rec := up.calls()[0]
	if !strings.Contains(rec.body, `"max_tokens":64`) {
		t.Fatalf("max_tokens 未透传: %s", rec.body)
	}
}

func TestNonStreamChatToolCallsFromMessages(t *testing.T) {
	// 客户端 OpenAI（无 max_tokens）→ 上游 Anthropic：默认 4096；tool_use → tool_calls。
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if !strings.Contains(rec.body, `"max_tokens":4096`) {
			t.Errorf("anthropic 缺省 max_tokens 未注入: %s", rec.body)
		}
		return 200, "application/json",
			`{"id":"msg_1","type":"message","role":"assistant","model":"up-m",
			  "content":[{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{"city":"北京"}}],
			  "stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":9}}`
	})
	gw := newGateway(t, messagesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions",
		`{"model":"prov/up-m","messages":[{"role":"user","content":"天气"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	var cc ChatChunk
	if err := json.Unmarshal(b, &cc); err != nil {
		t.Fatalf("客户端响应解析: %v\n%s", err, b)
	}
	msg := cc.Choices[0].Message
	if cc.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", cc.Choices[0].FinishReason)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_calls 翻译错误: %+v", msg)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("arguments = %q", msg.ToolCalls[0].Function.Arguments)
	}
}

func TestStreamChatUpstreamToolCallsToMessagesClient(t *testing.T) {
	// 上游 chat 流式带 tool_calls → 客户端 messages 流式 tool_use 事件。
	var b strings.Builder
	chunk := func(delta map[string]any, finish string) {
		j, _ := json.Marshal(map[string]any{
			"id": "c9", "object": "chat.completion.chunk", "model": "up-m",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		b.WriteString("data: " + string(j) + "\n\n")
	}
	chunk(map[string]any{"role": "assistant"}, "")
	chunk(map[string]any{"content": "查一下"}, "")
	chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "get_weather", "arguments": ""},
	}}}, "")
	chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "function": map[string]any{"arguments": `{"city":`},
	}}}, "")
	chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": 0, "function": map[string]any{"arguments": `"北京"}`},
	}}}, "")
	chunk(map[string]any{}, "tool_calls")
	b.WriteString("data: [DONE]\n\n")

	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "text/event-stream", b.String()
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/messages",
		`{"model":"prov/up-m","max_tokens":99,"stream":true,"messages":[{"role":"user","content":"天气"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)

	var toolStart, args string
	var stop string
	var sawTextDelta bool
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e msgEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatalf("事件解析: %v\n%s", err, line)
		}
		switch e.Type {
		case "content_block_start":
			if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
				toolStart = e.ContentBlock.Name
			}
		case "content_block_delta":
			if e.Delta.Type == "text_delta" && e.Delta.Text == "查一下" {
				sawTextDelta = true
			}
			if e.Delta.Type == "input_json_delta" {
				args += e.Delta.PartialJSON
			}
		case "message_delta":
			stop = e.Delta.StopReason
		}
	}
	if toolStart != "get_weather" || !sawTextDelta {
		t.Fatalf("tool_use 块/文本缺失: name=%q sawText=%v", toolStart, sawTextDelta)
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("聚合 arguments = %q", args)
	}
	if stop != "tool_use" {
		t.Fatalf("stop_reason = %q", stop)
	}
}

func TestStreamUpstreamErrorEventEndsCleanly(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "text/event-stream",
			"event: error\n" + `data: {"type":"error","error":{"message":"overloaded"}}` + "\n\n"
	})
	gw := newGateway(t, messagesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions",
		`{"model":"prov/up-m","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	// 上游 error 事件使流提前结束：客户端仍应收到结构完整的 chat 流
	if !strings.Contains(string(b), "data: [DONE]") {
		t.Fatalf("流未正常收尾\n%s", b)
	}
}

func TestEndpointsStateModelsPage(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "application/json", `{}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	// /v1/models
	resp := get(t, gw.URL, "/v1/models")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 1 || list.Data[0]["id"] != "prov" {
		t.Fatalf("models = %+v", list.Data)
	}
	// /
	resp2 := get(t, gw.URL, "/")
	if ct := resp2.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("管理页 Content-Type = %q", ct)
	}
	// /api/state 含休眠账号
	s := &Server{Planner: NewPlanner([]provider.Provider{chatProvider(up.srv.URL)}), Version: "t"}
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	pl := s.Planner
	prov, _, _ := pl.ResolveModel("prov/up-m")
	cands := pl.Candidates(prov, "up-m")
	pl.RestUntil(cands[0], time.Now().Add(time.Minute))
	resp3 := get(t, h.URL, "/api/state")
	var st map[string]any
	if err := json.NewDecoder(resp3.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st["resting"].(map[string]any)["prov/one"]; !ok {
		t.Fatalf("state 缺少休眠账号: %+v", st)
	}
}

func get(t *testing.T, url, path string) *http.Response {
	t.Helper()
	r, err := http.Get(url + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { r.Body.Close() })
	return r
}

func TestMessagesRequestToolRoundTripToChat(t *testing.T) {
	// anthropic 工具流（assistant tool_use + user tool_result）→ chat 顺序。
	mr := &MessagesRequest{
		Model:     "m",
		MaxTokens: 100,
		Tools:     []AnthropicTool{{Name: "get_weather", Description: "d", InputSchema: map[string]any{"type": "object"}}},
		Messages: []MessagesMessage{
			{Role: "user", Content: "天气"},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "查天气中"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"city": "北京"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "晴 25 度"},
			}},
		},
	}
	cr := messagesToChatRequest(mr)
	if len(cr.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(cr.Messages), cr.Messages)
	}
	if cr.Messages[0].Role != "user" {
		t.Fatalf("msg0 = %+v", cr.Messages[0])
	}
	a := cr.Messages[1]
	if a.Role != "assistant" || len(a.ToolCalls) != 1 || a.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("assistant 翻译错误: %+v", a)
	}
	tl := cr.Messages[2]
	if tl.Role != "tool" || tl.ToolCallID != "toolu_1" || tl.Content != "晴 25 度" {
		t.Fatalf("tool result 翻译错误: %+v", tl)
	}
	if len(cr.Tools) != 1 || cr.Tools[0].Function.Parameters == nil {
		t.Fatalf("tools 未映射: %+v", cr.Tools)
	}
}

func TestScanSSEEdges(t *testing.T) {
	input := ": heartbeat\n\n" +
		"event: a\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n" +
		"data: tail-no-newline"
	// 真实用法：同一条流共用一个 bufio.Reader
	br := newBufReader(strings.NewReader(input))
	ev1, err := scanSSE(br)
	if err != nil || ev1.Name != "a" || string(ev1.Data) != "line1\nline2" {
		t.Fatalf("ev1 = %+v err=%v", ev1, err)
	}
	ev2, err := scanSSE(br)
	if err != nil || string(ev2.Data) != "tail-no-newline" {
		t.Fatalf("ev2 = %+v err=%v", ev2, err)
	}
	if _, err := scanSSE(br); err == nil {
		t.Fatal("EOF 应返回错误")
	}
}
