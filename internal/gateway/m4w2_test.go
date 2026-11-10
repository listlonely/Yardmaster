// M4-W2 测试：Responses 双向翻译（非流/流式/工具调用/二级串联）。
package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// chat 上游流 → Responses 客户端事件流。
func TestResponsesStreamFromChatUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "text/event-stream", chatSSE("hi")
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/responses", `{"model":"prov/up-m","input":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{
		"event: response.output_item.added",
		"event: response.output_text.delta",
		`"delta":"h"`,
		"event: response.output_text.done",
		`"text":"hi"`,
		"event: response.completed",
		`"status":"completed"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应缺少 %q:\n%s", want, body)
		}
	}
}

// Responses 上游流 → Chat 客户端 chunk 流（文本 + 完成事件 + usage）。
func TestChatStreamFromResponsesUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		if rec.path != "/responses" {
			t.Errorf("上游路径 = %q", rec.path)
		}
		return http.StatusOK, "text/event-stream", responsesSSE("hi")
	})
	gw := newGateway(t, responsesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	text, finish := chatSSEToText(t, readAll(t, resp.Body))
	if text != "hi" || finish != "stop" {
		t.Fatalf("text=%q finish=%q", text, finish)
	}
}

// Responses 上游流 × Messages 客户端：二级串联（resp→chat→msgs）与
// 收尾顺序（上游 done 终止 chunk 先进编码链，message_stop 最后）。
func TestMessagesStreamFromResponsesUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "text/event-stream", responsesSSE("ok")
	})
	gw := newGateway(t, responsesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/messages", `{"model":"prov/up-m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	text, stop := msgSSEToText(t, body)
	if text != "ok" || stop != "end_turn" {
		t.Fatalf("text=%q stop=%q\n%s", text, stop, body)
	}
}

// 工具调用双向：Responses 客户端 ← Chat 上游（tool_calls → function_call）。
func TestResponsesToolCallFromChatUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "application/json", `{"id":"c1","object":"chat.completion","model":"up-m",
"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}],
"usage":{"prompt_tokens":4,"completion_tokens":5}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/responses", `{"model":"prov/up-m","input":"list files","tools":[{"type":"function","name":"ls","parameters":{"type":"object"}}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{
		`"type":"function_call"`, `"call_id":"call_1"`, `"name":"ls"`,
		`"arguments":"{\"path\":\".\"}"`, `"input_tokens":4`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
}

// 工具调用反向：Chat 客户端 ← Responses 上游（function_call → tool_calls）。
func TestChatToolCallFromResponsesUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "application/json", `{"id":"resp_9","object":"response","model":"up-m","status":"completed",
"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"ls","arguments":"{\"path\":\".\"}"}],
"usage":{"input_tokens":4,"output_tokens":5}}`
	})
	gw := newGateway(t, responsesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{`"tool_calls"`, `"id":"call_1"`, `"name":"ls"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":4`} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
}

// responsesSSE 构造一段上游 Responses 事件流（文本 delta + completed）。
func responsesSSE(text string) string {
	rs := []rune(text)
	var b strings.Builder
	ev := func(name string, body string) {
		b.WriteString("event: " + name + "\ndata: " + body + "\n\n")
	}
	ev("response.created", `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`)
	ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_0","role":"assistant"}}`)
	ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_0","delta":"`+string(rs[0])+`"}`)
	ev("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_0","delta":"`+string(rs[1:])+`"}`)
	ev("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","role":"assistant","status":"completed"}}`)
	ev("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}`)
	return b.String()
}
