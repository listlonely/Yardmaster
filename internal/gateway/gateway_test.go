// W2 回放测试：httptest 假上游 + 真实网关 Handler 栈（经 HTTP 而非直调
// Handler，保证 lanGuard 之外的全链路被测——开发文档 §5「测试检查行为」）。
// SSE 夹具为手工构造的真实格式样例；拿到真实抓包后逐步替换（TODO(W3)）。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agenthub/internal/provider"
)

type recordedReq struct {
	auth    string
	path    string
	body    string
	xAPIKey string
}

type fakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []recordedReq
	respond func(rec recordedReq, n int) (int, string, string) // status, contentType, body
}

func newFakeUpstream(t *testing.T, respond func(rec recordedReq, n int) (int, string, string)) *fakeUpstream {
	t.Helper()
	fu := &fakeUpstream{respond: respond}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fu.mu.Lock()
		rec := recordedReq{
			auth:    r.Header.Get("Authorization"),
			xAPIKey: r.Header.Get("X-Api-Key"),
			path:    r.URL.Path,
			body:    string(b),
		}
		fu.reqs = append(fu.reqs, rec)
		n := len(fu.reqs)
		fu.mu.Unlock()
		status, ct, body := fu.respond(rec, n)
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	fu.srv = httptest.NewServer(mux)
	t.Cleanup(fu.srv.Close)
	return fu
}

func (fu *fakeUpstream) calls() []recordedReq {
	fu.mu.Lock()
	defer fu.mu.Unlock()
	return append([]recordedReq(nil), fu.reqs...)
}

func chatProvider(baseURL string) provider.Provider {
	return provider.Provider{
		ID: "prov", Name: "Test", Protocol: provider.ProtocolChat, BaseURL: baseURL,
		Accounts: []provider.Account{
			{Name: "one", APIKey: "sk-one", Status: provider.AccountAvailable},
			{Name: "two", APIKey: "sk-two", Status: provider.AccountAvailable},
		},
	}
}

func messagesProvider(baseURL string) provider.Provider {
	p := chatProvider(baseURL)
	p.Protocol = provider.ProtocolMessages
	return p
}

func newGateway(t *testing.T, prov provider.Provider) *httptest.Server {
	t.Helper()
	s := &Server{Planner: NewPlanner([]provider.Provider{prov}, nil), Version: "test"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, path, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func chatReqBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
}

func chatSSE(text string) string {
	var b strings.Builder
	rs := []rune(text)
	mk := func(delta, finish string) {
		chunk := map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "model": "up-m",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": finish}},
		}
		if delta == "" && finish == "" {
			chunk["choices"].([]any)[0].(map[string]any)["delta"] = map[string]any{"role": "assistant"}
		}
		j, _ := json.Marshal(chunk)
		b.WriteString("data: " + string(j) + "\n\n")
	}
	mk("", "")
	mk(string(rs[0]), "")
	mk(string(rs[1:]), "")
	mk("", "stop")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func msgSSE(text string) string {
	rs := []rune(text)
	return "event: message_start\n" + `data: {"type":"message_start","message":{"model":"up-m"}}` + "\n\n" +
		"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + string(rs[0]) + `"}}` + "\n\n" +
		"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + string(rs[1:]) + `"}}` + "\n\n" +
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
}

// chatSSEToText 把客户端收到的 chat 流式 SSE 聚合为文本与 finish_reason。
func chatSSEToText(t *testing.T, body string) (string, string) {
	t.Helper()
	var text, finish string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("解析客户端收到的 chat 块: %v\n%s", err, payload)
		}
		for _, ch := range chunk.Choices {
			if s, ok := ch.Delta.Content.(string); ok {
				text += s
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	return text, finish
}

// msgSSEToText 把客户端收到的 anthropic 流式 SSE 聚合为文本与 stop_reason。
func msgSSEToText(t *testing.T, body string) (string, string) {
	t.Helper()
	var text, stop string
	var events []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e msgEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatalf("解析客户端收到的 messages 事件: %v\n%s", err, line)
		}
		events = append(events, e.Type)
		switch e.Type {
		case "content_block_delta":
			if e.Delta.Type == "text_delta" {
				text += e.Delta.Text
			}
		case "message_delta":
			stop = e.Delta.StopReason
		}
	}
	if len(events) == 0 || events[0] != "message_start" {
		t.Fatalf("首个事件应为 message_start，got %v", events)
	}
	if events[len(events)-1] != "message_stop" {
		t.Fatalf("末个事件应为 message_stop，got %v", events)
	}
	return text, stop
}

func TestPassthroughChatStream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		// §5.1：断言上游实际收到改写后的模型名（provider/ 前缀必须剥除）
		if !strings.Contains(rec.body, `"model":"up-m"`) {
			t.Errorf("上游收到的 model 未改写: %s", rec.body)
		}
		return 200, "text/event-stream", chatSSE("Hello")
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", true))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Gw-Provider"); got != "prov" {
		t.Fatalf("X-Gw-Provider = %q", got)
	}
	if got := resp.Header.Get("X-Gw-Account"); got != "one" {
		t.Fatalf("X-Gw-Account = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	text, finish := chatSSEToText(t, string(body))
	if text != "Hello" || finish != "stop" {
		t.Fatalf("text=%q finish=%q", text, finish)
	}
	if ct := up.calls()[0].auth; ct != "Bearer sk-one" {
		t.Fatalf("upstream auth = %q", ct)
	}
}

func TestMessagesToChatStreamTranslation(t *testing.T) {
	// 旗舰场景：Claude 风格客户端（Messages）→ OpenAI 兼容上游。
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if rec.path != "/chat/completions" {
			t.Errorf("上游路径 = %q", rec.path)
		}
		if !strings.Contains(rec.body, `"model":"up-m"`) {
			t.Errorf("上游收到的 model 未改写: %s", rec.body)
		}
		return 200, "text/event-stream", chatSSE("你好")
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	reqBody := `{"model":"prov/up-m","max_tokens":256,"stream":true,
		"system":"You are helpful",
		"messages":[{"role":"user","content":[{"type":"text","text":"打个招呼"}]}]}`
	resp := post(t, gw.URL, "/v1/messages", reqBody)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, b)
	}
	// 上游收到的应是 chat 请求：system 提为消息、max_tokens 透传
	rec := up.calls()[0]
	var upReq ChatRequest
	if err := json.Unmarshal([]byte(rec.body), &upReq); err != nil {
		t.Fatalf("上游请求体解析: %v\n%s", err, rec.body)
	}
	if len(upReq.Messages) != 2 || upReq.Messages[0].Role != "system" || upReq.Messages[0].Content != "You are helpful" {
		t.Fatalf("system 未提升: %+v", upReq.Messages)
	}
	if upReq.MaxTokens != 256 {
		t.Fatalf("max_tokens = %d", upReq.MaxTokens)
	}
	if !upReq.Stream {
		t.Fatal("上游应为流式")
	}
	body, _ := io.ReadAll(resp.Body)
	text, stop := msgSSEToText(t, string(body))
	if text != "你好" || stop != "end_turn" {
		t.Fatalf("text=%q stop=%q", text, stop)
	}
}

func TestChatToMessagesStreamWithTools(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if rec.path != "/v1/messages" {
			t.Errorf("上游路径 = %q", rec.path)
		}
		if !strings.Contains(rec.body, `"input_schema"`) {
			t.Errorf("工具定义未翻译为 input_schema: %s", rec.body)
		}
		return 200, "text/event-stream", msgSSE("ok")
	})
	gw := newGateway(t, messagesProvider(up.srv.URL))

	reqBody := `{"model":"prov/up-m","stream":true,"max_tokens":128,
		"tools":[{"type":"function","function":{"name":"get_weather","description":"查天气","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"天气如何"}]}`
	resp := post(t, gw.URL, "/v1/chat/completions", reqBody)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, b)
	}
	body, _ := io.ReadAll(resp.Body)
	text, finish := chatSSEToText(t, string(body))
	if text != "ok" || finish != "stop" {
		t.Fatalf("text=%q finish=%q", text, finish)
	}
}

func TestFailoverOn500(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if rec.auth == "Bearer sk-one" {
			return 500, "application/json", `{"error":{"message":"boom"}}`
		}
		return 200, "application/json", `{"id":"c2","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != 200 {
		t.Fatalf("failover 后应 200，got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Gw-Account"); got != "two" {
		t.Fatalf("X-Gw-Account = %q, want two", got)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ok") {
		t.Fatalf("body = %s", b)
	}
}

func Test429RestsCandidate(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if rec.auth == "Bearer sk-one" {
			return 429, "application/json", `{"error":{"message":"quota"}}`
		}
		return 200, "application/json", `{"id":"c3","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != 200 {
		t.Fatalf("req 0: status = %d", resp.StatusCode)
	}
	n0 := len(up.calls()) // req0 结束时：sk-one 429 + sk-two 200
	for i := 0; i < 2; i++ {
		resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: status = %d", i+1, resp.StatusCode)
		}
		rest := up.calls()[n0:]
		for _, c := range rest {
			if c.auth == "Bearer sk-one" {
				t.Fatalf("休眠候选不应再被尝试，本请求调用了 %v", rest)
			}
		}
	}
	if len(up.calls()) != n0+2 {
		t.Fatalf("后续两次请求应各只打账号 two，共 %d calls", len(up.calls()))
	}
}

func TestRequestErrorNoFailover(t *testing.T) {
	attempts := 0
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		attempts++
		return 400, "application/json", `{"error":{"message":"bad request shape"}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != 400 {
		t.Fatalf("请求类 4xx 应原样返回，got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "bad request shape") {
		t.Fatalf("body = %s", b)
	}
	if attempts != 1 {
		t.Fatalf("4xx 不应切换候选，attempts = %d", attempts)
	}
}

func TestAllFailReturnsLastError(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 503, "application/json", fmt.Sprintf(`{"error":{"message":"down-%d"}}`, n)
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("prov/up-m", false))
	if resp.StatusCode != 503 {
		t.Fatalf("全败应回最后一次状态码，got %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "down-2") {
		t.Fatalf("应回最后一次错误，body = %s", b)
	}
	if len(up.calls()) != 2 {
		t.Fatalf("两个候选都应被尝试，got %d", len(up.calls()))
	}
}

func TestUnknownModelRejected(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) { return 200, "application/json", "{}" })
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("nope/m", false))
	if resp.StatusCode != 404 {
		t.Fatalf("未知 provider 应 404，got %d", resp.StatusCode)
	}
	resp2 := post(t, gw.URL, "/v1/chat/completions", chatReqBody("bare", false))
	if resp2.StatusCode != 404 {
		t.Fatalf("非 provider/model 形式应 404，got %d", resp2.StatusCode)
	}
	if len(up.calls()) != 0 {
		t.Fatal("不应触达上游")
	}
}

func TestPlannerRestLastOrdering(t *testing.T) {
	p := NewPlanner([]provider.Provider{chatProvider("http://unused")}, nil)
	prov, _, err := p.ResolveModel("prov/up-m")
	if err != nil {
		t.Fatal(err)
	}
	cands := p.Candidates(prov, "up-m")
	if len(cands) != 2 || cands[0].Account.Name != "one" {
		t.Fatalf("初始顺序错误: %+v", cands)
	}
	p.Rest(cands[0], 10*time.Minute)
	cands = p.Candidates(prov, "up-m")
	if cands[0].Account.Name != "two" || cands[1].Account.Name != "one" {
		t.Fatalf("休眠候选应排末尾: %+v", cands)
	}
}
