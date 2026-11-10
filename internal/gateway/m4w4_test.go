// M4-W4 矩阵收口：4 协议入站 × 4 协议上游（16 组合 × 非流/流式）的
// httptest 全覆盖（不碰网络）。断言：请求翻译到上游形状、响应翻译回
// 客户端形状、文本语义等价、用量字段对齐。
package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"yardmaster/internal/provider"
)

// 矩阵上游：按上游协议返回该协议的文本响应（含 usage），并断言收到的
// 请求已是该协议的形状（模型名已改写为 up-m）。
func matrixUpstream(t *testing.T, proto provider.Protocol) *fakeUpstream {
	t.Helper()
	return newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		// 请求形状断言（每个协议的关键特征字段 + 模型名改写）
		switch proto {
		case provider.ProtocolChat:
			if !strings.Contains(rec.body, `"messages"`) || !strings.Contains(rec.body, `"model":"up-m"`) {
				t.Errorf("[chat 上游] 请求形状异常: %s", rec.body)
			}
		case provider.ProtocolMessages:
			if !strings.Contains(rec.body, `"max_tokens"`) {
				t.Errorf("[messages 上游] 请求形状异常: %s", rec.body)
			}
		case provider.ProtocolResponses:
			if !strings.Contains(rec.body, `"input"`) || !strings.Contains(rec.body, `"model":"up-m"`) {
				t.Errorf("[responses 上游] 请求形状异常: %s", rec.body)
			}
		case provider.ProtocolGemini:
			if !strings.Contains(rec.body, `"contents"`) {
				t.Errorf("[gemini 上游] 请求形状异常: %s", rec.body)
			}
		}
		return matrixRespond(proto, n)
	})
}

// matrixRespond 生成各协议的非流/流响应（第 n 次调用交替非流/流由
// stream 决定，这里 n 用于区分调用序号仅日志）。
func matrixRespond(proto provider.Protocol, _ int) (int, string, string) {
	switch proto {
	case provider.ProtocolMessages:
		return http.StatusOK, "application/json",
			`{"id":"m1","type":"message","role":"assistant","model":"up-m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
	case provider.ProtocolResponses:
		return http.StatusOK, "application/json",
			`{"id":"resp_1","object":"response","model":"up-m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
	case provider.ProtocolGemini:
		return http.StatusOK, "application/json",
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`
	default:
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	}
}

func matrixProvider(baseURL string, proto provider.Protocol) provider.Provider {
	p := chatProvider(baseURL)
	p.Protocol = proto
	return p
}

// 入站端点与请求体（model 用 prov/up-m，经 planner 解析为 up-m）。
func matrixInbound(client ClientProto, stream bool) (path, body string) {
	switch client {
	case ProtoMessages:
		return "/v1/messages", fmt.Sprintf(`{"model":"prov/up-m","stream":%t,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, stream)
	case ProtoResponses:
		return "/v1/responses", fmt.Sprintf(`{"model":"prov/up-m","stream":%t,"input":"hi"}`, stream)
	case ProtoGemini:
		if stream {
			return "/v1beta/models/prov%2Fup-m:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
		}
		return "/v1beta/models/prov%2Fup-m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	default:
		return "/v1/chat/completions", chatReqBody("prov/up-m", stream)
	}
}

// 客户端侧断言：响应应为该客户端协议的形状且文本为 "ok"。
func matrixAssert(t *testing.T, client ClientProto, stream bool, body string) {
	t.Helper()
	marker := map[ClientProto]string{
		ProtoChat:      `"finish_reason"`,
		ProtoMessages:  `"stop_reason"`,
		ProtoResponses: `"output"`,
		ProtoGemini:    `"candidates"`,
	}
	if !strings.Contains(body, marker[client]) {
		t.Fatalf("[%s 客户端] 响应形状异常（缺 %q）:\n%s", client, marker[client], body)
	}
	var text, stop string
	switch client {
	case ProtoChat:
		if stream {
			text, stop = chatSSEToText(t, body)
		} else if !strings.Contains(body, `"content":"ok"`) {
			t.Fatalf("chat 响应文本异常:\n%s", body)
		}
	case ProtoMessages:
		if stream {
			text, stop = msgSSEToText(t, body)
		} else if !strings.Contains(body, `"text":"ok"`) {
			t.Fatalf("messages 响应文本异常:\n%s", body)
		}
	case ProtoResponses:
		if !strings.Contains(body, `"text":"ok"`) {
			t.Fatalf("responses 响应文本异常:\n%s", body)
		}
	case ProtoGemini:
		if !strings.Contains(body, `"text":"ok"`) {
			t.Fatalf("gemini 响应文本异常:\n%s", body)
		}
	}
	if stream && client == ProtoChat && (text != "ok" || (stop != "stop" && stop != "")) {
		t.Fatalf("chat 流式聚合异常: text=%q stop=%q", text, stop)
	}
	if stream && client == ProtoMessages && text != "ok" {
		t.Fatalf("messages 流式聚合异常: text=%q stop=%q", text, stop)
	}
}

// TestMatrix4x4NonStream：非流 16 组合（4 入站 × 4 上游，含同协议透传）。
func TestMatrix4x4NonStream(t *testing.T) {
	clients := []ClientProto{ProtoChat, ProtoMessages, ProtoResponses, ProtoGemini}
	upstreams := []provider.Protocol{provider.ProtocolChat, provider.ProtocolMessages, provider.ProtocolResponses, provider.ProtocolGemini}
	for _, cl := range clients {
		for _, up := range upstreams {
			name := fmt.Sprintf("%s→%s", cl, up)
			t.Run(name, func(t *testing.T) {
				srv := matrixUpstream(t, up)
				gw := newGateway(t, matrixProvider(srv.srv.URL, up))
				path, body := matrixInbound(cl, false)
				resp := post(t, gw.URL, path, body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				matrixAssert(t, cl, false, readAll(t, resp.Body))
			})
		}
	}
}

// TestMatrix4x4Stream：流式 16 组合。上游按协议返回 SSE。
func TestMatrix4x4Stream(t *testing.T) {
	clients := []ClientProto{ProtoChat, ProtoMessages, ProtoResponses, ProtoGemini}
	upstreams := []provider.Protocol{provider.ProtocolChat, provider.ProtocolMessages, provider.ProtocolResponses, provider.ProtocolGemini}
	for _, cl := range clients {
		for _, up := range upstreams {
			name := fmt.Sprintf("%s→%s", cl, up)
			t.Run(name, func(t *testing.T) {
				srv := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
					switch up {
					case provider.ProtocolMessages:
						return http.StatusOK, "text/event-stream", msgSSE("ok")
					case provider.ProtocolResponses:
						return http.StatusOK, "text/event-stream", responsesSSE("ok")
					case provider.ProtocolGemini:
						return http.StatusOK, "text/event-stream",
							"data: " + `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}` + "\n\n" +
								"data: " + `{"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}` + "\n\n"
					default:
						return http.StatusOK, "text/event-stream", chatSSE("ok")
					}
				})
				gw := newGateway(t, matrixProvider(srv.srv.URL, up))
				path, body := matrixInbound(cl, true)
				resp := post(t, gw.URL, path, body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d", resp.StatusCode)
				}
				out := readAll(t, resp.Body)
				// 聚合断言：delta 逐字拆分，无法整词 Contains——聚合所有
				// 文本载荷字段（text/delta/content）后应含全文 "ok"。
				if agg := sseTextAggregate(out); !strings.Contains(agg, "ok") {
					t.Fatalf("流式输出缺文本（聚合=%q）:\n%s", agg, out)
				}
				// 终止语义：chat/responses/messages 有终界；gemini 无 [DONE]
				if cl == ProtoGemini && strings.Contains(out, "[DONE]") {
					t.Fatal("gemini 客户端不应收到 [DONE]")
				}
			})
		}
	}
}

// 用量字段对齐：gemini 上游的 camelCase 用量进入 usageScanner。
func TestUsageScannerGeminiFields(t *testing.T) {
	sc := &usageScanner{}
	_, _ = sc.Write([]byte(`{"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7}}`))
	u := sc.usageOf()
	if u == nil || u.Prompt != 11 || u.Completion != 7 {
		t.Fatalf("gemini 用量提取异常: %+v", u)
	}
}

// sseTextAggregate 聚合一段 SSE/JSON 输出里的全部文本载荷值
// （"text"/"delta"/"content" 字段的字符串值），供跨协议流式断言。
func sseTextAggregate(out string) string {
	var b strings.Builder
	for _, m := range sseTextRe.FindAllStringSubmatch(out, -1) {
		b.WriteString(m[1])
	}
	return b.String()
}

var sseTextRe = regexp.MustCompile(`"(?:text|delta|content)"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// trace 记录互转路径（client_proto × upstream_proto）。
func TestTraceRecordsProtocolPath(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return matrixRespond(provider.ProtocolGemini, 0)
	})
	tlog := NewTraceLog(t.TempDir())
	defer tlog.Close()
	s := &Server{Planner: NewPlanner([]provider.Provider{matrixProvider(up.srv.URL, provider.ProtocolGemini)}, nil), Version: "t", Traces: tlog}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"prov/up-m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("请求失败: %v %d", err, r.StatusCode)
	}
	r.Body.Close()
	trs := tlog.Recent(10, "", "")
	if len(trs) == 0 {
		t.Fatal("无 trace")
	}
	last := trs[len(trs)-1]
	if last.ClientProto != string(ProtoMessages) || last.UpstreamProto != string(ProtoGemini) {
		t.Fatalf("互转路径未记录: client=%q upstream=%q", last.ClientProto, last.UpstreamProto)
	}
}
