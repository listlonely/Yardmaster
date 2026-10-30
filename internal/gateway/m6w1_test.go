// M6-W1 真实校准回归测试：claude-code 真实冒烟（anthropic 入站 × chat 上游）
// 抓到的缺陷——msgsEncoder 流式回翻把 usage 硬编码为 output_tokens:0，
// 真实客户端（claude-code）上下文统计全零。测试断言真实数值回填，
// 还原旧代码（无 evUsage 处理 + 硬编码 0）即失败。
package gateway

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestMsgsClientStreamCarriesRealUsage：chat 上游流式（末 chunk 带 usage）
// → messages 客户端，message_delta.usage 必须回填上游真实 prompt/completion。
func TestMsgsClientStreamCarriesRealUsage(t *testing.T) {
	var b strings.Builder
	chunk := func(v map[string]any) {
		j, _ := json.Marshal(v)
		b.WriteString("data: " + string(j) + "\n\n")
	}
	chunk(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "model": "up-m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})
	chunk(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "model": "up-m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "1+1等于2。"}, "finish_reason": nil}},
	})
	// 终止 chunk：finish + usage（openai 形状，usage 在顶层；cached 含于 prompt）。
	chunk(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "model": "up-m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage": map[string]any{
			"prompt_tokens": 11881, "completion_tokens": 17,
			"prompt_tokens_details": map[string]any{"cached_tokens": 6912},
		},
	})
	b.WriteString("data: [DONE]\n\n")

	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "text/event-stream", b.String()
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/messages",
		`{"model":"prov/up-m","max_tokens":99,"stream":true,"messages":[{"role":"user","content":"问"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)

	var inTok, outTok, cacheRead = -1, -1, -1
	var sawStop bool
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
		case "message_delta":
			sawStop = e.Delta.StopReason == "end_turn"
			if e.Usage != nil {
				inTok, outTok, cacheRead = e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CacheReadInputTokens
			}
		}
	}
	if !sawStop {
		t.Fatalf("缺 message_delta/stop_reason=end_turn")
	}
	// anthropic 出口语义：input_tokens 不含命中（11881-6912=4969），命中单列。
	if inTok != 4969 || outTok != 17 || cacheRead != 6912 {
		t.Fatalf("message_delta.usage = (input=%d, output=%d, cache_read=%d)，应为 (4969, 17, 6912)——旧代码硬编码 output_tokens:0 且丢弃 cached", inTok, outTok, cacheRead)
	}
}
