// M4-W3 测试：Gemini 入站/上游/翻译（含流式与工具调用）。
package gateway

import (
	"net/http"
	"strings"
	"testing"

	"yardmaster/internal/provider"
)

func geminiProvider(baseURL string) provider.Provider {
	p := chatProvider(baseURL)
	p.Protocol = provider.ProtocolGemini
	return p
}

// Gemini 入站（非流）：URL 取 model/stream，翻译为 Chat 上游，响应译回
// Gemini 形状（candidates/usageMetadata/finishReason）。
func TestGeminiInboundToChatUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		if rec.path != "/chat/completions" {
			t.Errorf("上游路径 = %q", rec.path)
		}
		if !strings.Contains(rec.body, `"role":"system"`) || !strings.Contains(rec.body, `"model":"up-m"`) {
			t.Errorf("chat 请求翻译异常: %s", rec.body)
		}
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"yo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	// 模型名含 / 时按 %2F 编码（ServeMux 段匹配在 escaped path 上进行）。
	resp := post(t, gw.URL, "/v1beta/models/prov%2Fup-m:generateContent",
		`{"systemInstruction":{"parts":[{"text":"be brief"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{
		`"candidates"`, `"role":"model"`, `"text":"yo"`, `"finishReason":"STOP"`,
		`"promptTokenCount":3`, `"candidatesTokenCount":2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
}

// Gemini 上游（非流）：URL 含上游模型名、x-goog-api-key 认证；Chat 客户端
// 收到翻译后的 chat 响应。
func TestChatClientToGeminiUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		if rec.path != "/v1beta/models/up-m:generateContent" {
			t.Errorf("上游路径 = %q", rec.path)
		}
		if rec.googKey != "sk-one" {
			t.Errorf("x-goog-api-key 认证异常: %q", rec.googKey)
		}
		if !strings.Contains(rec.body, `"contents"`) {
			t.Errorf("gemini 请求翻译异常: %s", rec.body)
		}
		return http.StatusOK, "application/json",
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`
	})
	gw := newGateway(t, geminiProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{`"content":"hi"`, `"finish_reason":"stop"`, `"prompt_tokens":3`} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
}

// Gemini 同协议透传：body 原样、URL 换上游模型名。
func TestGeminiPassthroughRewritesModelInURL(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		if rec.path != "/v1beta/models/up-m:generateContent" {
			t.Errorf("上游路径 = %q", rec.path)
		}
		if !strings.Contains(rec.body, `"contents"`) || strings.Contains(rec.body, `"model"`) {
			t.Errorf("body 应原样透传且无 model 字段: %s", rec.body)
		}
		return http.StatusOK, "application/json",
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
	})
	gw := newGateway(t, geminiProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1beta/models/prov%2Fup-m:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(readAll(t, resp.Body), `"text":"ok"`) {
		t.Fatal("透传响应异常")
	}
}

// Gemini 入站流式：chat 上游 SSE → gemini chunk 事件（无 [DONE]）。
func TestGeminiInboundStreamFromChatUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "text/event-stream", chatSSE("hi")
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1beta/models/prov%2Fup-m:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{`"parts":[{"text":"h"}]`, `"finishReason":"STOP"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatal("Gemini 流不应含 [DONE]（Google SDK 兼容）")
	}
}

// Gemini 上游流式 × Messages 客户端：gemini→chat→msgs 多级串联。
func TestMessagesStreamFromGeminiUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "text/event-stream",
			"data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}` + "\n\n" +
				"data: " + `{"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}` + "\n\n"
	})
	gw := newGateway(t, geminiProvider(up.srv.URL))

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

// 工具调用：Gemini 客户端 ← Chat 上游（tool_calls → functionCall part）。
func TestGeminiToolCallFromChatUpstream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "application/json", `{"id":"c1","object":"chat.completion","model":"up-m",
"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"path\":\".\"}"}}]},"finish_reason":"tool_calls"}],
"usage":{"prompt_tokens":4,"completion_tokens":5}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1beta/models/prov%2Fup-m:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"ls"}]}],"tools":[{"functionDeclarations":[{"name":"ls"}]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	for _, want := range []string{`"functionCall"`, `"name":"ls"`, `"path":"."`, `"promptTokenCount":4`} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
}

// 工具结果回传：Gemini 入站 functionResponse → Chat role:"tool"。
func TestGeminiFunctionResponseToToolRole(t *testing.T) {
	var gotBody string
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		gotBody = rec.body
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1beta/models/prov%2Fup-m:generateContent",
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"ls","args":{"path":"."}}}]},{"role":"user","parts":[{"functionResponse":{"name":"ls","response":{"files":1}}}]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{`"tool_calls"`, `"role":"tool"`, `"tool_call_id":"gemini-ls"`, `"files\"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("chat 请求缺少 %q:\n%s", want, gotBody)
		}
	}
}

func TestGeminiInputStats(t *testing.T) {
	gr := &GeminiRequest{
		SystemInstruction: &GeminiContent{Parts: []GeminiPart{{Text: "sys"}}},
		Contents: []GeminiContent{
			{Role: "user", Parts: []GeminiPart{{Text: "q1"}}},
			{Role: "model", Parts: []GeminiPart{{Text: "a1"}}},
			{Role: "user", Parts: []GeminiPart{{Text: "q2"}}},
		},
	}
	lastUser, est := inputStats(gr)
	if lastUser != "q2" || est == 0 {
		t.Fatalf("lastUser=%q est=%d", lastUser, est)
	}
}
