// M10-W3 响应侧 thinking 流出 + W4 C3：GLM reasoning_content → evThinking
// → messages thinking 块 / responses reasoning item / gemini thought part；
// reasoning_tokens 入账（D4）；chat 客户端不输出（D5）；C3 中途 system
// <system-reminder> 包装。还原 protocol.md IR v2 决策即失败。
package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// w2w3Buf 是内存 SSE 收集器。
type w3Buf struct{ b []byte }

func (o *w3Buf) Write(p []byte) (int, error) { o.b = append(o.b, p...); return len(p), nil }
func (o *w3Buf) flush()                      {}
func (o *w3Buf) String() string              { return string(o.b) }

// TestThinkingToMessagesStream：GLM reasoning_content 增量 → msgsEncoder
// thinking 块（content_block_start/delta，无 signature_delta——D3），
// 正文文本接在 thinking 块之后（块序惯例）。
func TestThinkingToMessagesStream(t *testing.T) {
	buf := &w3Buf{}
	enc := newMsgsEncoder(&sseOut{w: buf, flush: buf.flush})
	enc.Encode(gwEvent{kind: evStart, model: "up-m"})
	enc.Encode(gwEvent{kind: evThinking, text: "思考一"})
	enc.Encode(gwEvent{kind: evThinking, text: "思考二"})
	enc.Encode(gwEvent{kind: evText, text: "答案"})
	enc.Encode(gwEvent{kind: evStop, stop: "stop"})
	enc.Encode(gwEvent{kind: evUsage, usage: gwUsage{prompt: 10, completion: 20, reasoning: 15}})
	enc.Finish()
	s := buf.String()
	if !strings.Contains(s, `"content_block":{"type":"thinking"}`) {
		t.Errorf("缺 thinking 块开启: %s", s)
	}
	// 注：map 序列化键按字母序，断言只做键序无关包含
	if !strings.Contains(s, "thinking_delta") || !strings.Contains(s, "思考一") {
		t.Errorf("缺 thinking_delta: %s", s)
	}
	if strings.Contains(s, "signature") {
		t.Errorf("D3：不应产出 signature: %s", s)
	}
	// 块序：thinking 块 index 先于 text 块
	ti := strings.Index(s, `"content_block":{"type":"thinking"}`)
	tx := strings.Index(s, `"content_block":{"type":"text"}`)
	if ti < 0 || tx < 0 || ti > tx {
		t.Errorf("thinking 块应在 text 前: ti=%d tx=%d", ti, tx)
	}
	if !strings.Contains(s, "text_delta") || !strings.Contains(s, "答案") {
		t.Errorf("正文丢失: %s", s)
	}
}

// TestThinkingNonStreamPaths：非流 hub chunk（reasoning_content）→
// messages thinking 块前置；→ responses reasoning item（message 前）；
// → gemini thought part（前置于正文）。
func TestThinkingNonStreamPaths(t *testing.T) {
	cc := &ChatChunk{ID: "c1", Choices: []ChatChoice{{
		Index: 0, FinishReason: "stop",
		Message: &ChatMessage{Role: "assistant", Content: "答案", Reasoning: "推理过程"},
	}}}
	// messages：thinking 块在 text 前
	ms := chatResponseToMessages(&MessagesRequest{Model: "m"}, cc)
	if len(ms.Content) < 2 || ms.Content[0].Type != "thinking" || ms.Content[0].Thinking != "推理过程" {
		t.Errorf("messages thinking 块缺失或顺序错误: %+v", ms.Content)
	}
	if ms.Content[1].Type != "text" || ms.Content[1].Text != "答案" {
		t.Errorf("正文块错误: %+v", ms.Content[1])
	}
	// responses：reasoning item 在 message 前
	rsp := chatResponseToResponses("m", cc)
	if len(rsp.Output) < 2 || rsp.Output[0].Type != "reasoning" {
		t.Errorf("responses reasoning item 缺失或顺序错误: %+v", rsp.Output)
	}
	b, _ := json.Marshal(rsp.Output[0])
	if !strings.Contains(string(b), `"summary_text"`) || !strings.Contains(string(b), "推理过程") {
		t.Errorf("reasoning summary 形状错误: %s", b)
	}
	// gemini：thought part 前置于正文
	gr := chatResponseToGemini(cc)
	if len(gr.Candidates) == 0 {
		t.Fatal("gemini 无候选")
	}
	parts := gr.Candidates[0].Content.Parts
	if len(parts) < 2 || !parts[0].Thought || parts[0].Text != "推理过程" {
		t.Errorf("gemini thought part 缺失或顺序错误: %+v", parts)
	}
	if parts[1].Text != "答案" || parts[1].Thought {
		t.Errorf("gemini 正文 part 错误: %+v", parts[1])
	}
}

// TestThinkingToResponsesStream：evThinking → reasoning_summary_text.delta
// + output_index 动态（reasoning=0/message=1）+ completed output 顺序。
func TestThinkingToResponsesStream(t *testing.T) {
	buf := &w3Buf{}
	enc := newRespEncoder(&sseOut{w: buf, flush: buf.flush})
	enc.Encode(gwEvent{kind: evStart, msgID: "resp_x"})
	enc.Encode(gwEvent{kind: evThinking, text: "思考"})
	enc.Encode(gwEvent{kind: evText, text: "答案"})
	enc.Encode(gwEvent{kind: evStop, stop: "stop"})
	enc.Encode(gwEvent{kind: evUsage, usage: gwUsage{prompt: 10, completion: 20, reasoning: 5}})
	enc.Finish()
	s := buf.String()
	if !strings.Contains(s, "reasoning_summary_text.delta") {
		t.Errorf("缺 reasoning_summary_text.delta: %s", s)
	}
	ri := strings.Index(s, `"type":"reasoning"`)
	mi := strings.Index(s, `"type":"message"`)
	if ri < 0 || mi < 0 || ri > mi {
		t.Errorf("reasoning item 应在 message 前: ri=%d mi=%d", ri, mi)
	}
	// message item 的 output_index 应为 1（reasoning 占 0）
	if !strings.Contains(s, `"output_index":1`) {
		t.Errorf("message item output_index 应为 1: %s", s)
	}
}

// TestReasoningTokensUsage：chat 上游 reasoning_tokens → responses
// output_tokens_details（流式 Finish 与非流翻译）与 gemini
// thoughtsTokenCount（D4）。
func TestReasoningTokensUsage(t *testing.T) {
	// 流式：respEncoder Finish usage
	buf := &w3Buf{}
	enc := newRespEncoder(&sseOut{w: buf, flush: buf.flush})
	enc.Encode(gwEvent{kind: evUsage, usage: gwUsage{prompt: 27, completion: 89, reasoning: 59}})
	enc.Finish()
	s := buf.String()
	if !strings.Contains(s, `"output_tokens_details":{"reasoning_tokens":59}`) {
		t.Errorf("responses usage 缺 reasoning_tokens: %s", s)
	}
	// 流式：geminiEncoder Finish usage
	buf2 := &w3Buf{}
	genc := newGeminiEncoder(&sseOut{w: buf2, flush: buf2.flush})
	genc.Encode(gwEvent{kind: evUsage, usage: gwUsage{prompt: 27, completion: 89, reasoning: 59}})
	genc.Finish()
	if !strings.Contains(buf2.String(), `"thoughtsTokenCount":59`) {
		t.Errorf("gemini usage 缺 thoughtsTokenCount: %s", buf2.String())
	}
	// 非流：chat chunk → responses Usage
	cc := &ChatChunk{Choices: []ChatChoice{{Index: 0, FinishReason: "stop", Message: &ChatMessage{Role: "assistant"}}},
		Usage: &ChatUsage{PromptTokens: 27, CompletionTokens: 89,
			CompletionTokensDetails: &struct {
				ReasoningTokens int `json:"reasoning_tokens,omitempty"`
			}{ReasoningTokens: 110}}}
	rsp := chatResponseToResponses(cc.Model, cc)
	if rsp.Usage == nil || rsp.Usage.OutputTokensDetails == nil || rsp.Usage.OutputTokensDetails.ReasoningTokens != 110 {
		t.Errorf("非流 responses usage reasoning_tokens 缺失: %+v", rsp.Usage)
	}
	// 非流：chat chunk → gemini UsageMeta
	gr := chatResponseToGemini(cc)
	if gr.UsageMeta == nil || gr.UsageMeta.ThoughtsTokenCount != 110 {
		t.Errorf("非流 gemini thoughtsTokenCount 缺失: %+v", gr.UsageMeta)
	}
}

// TestD5ChatClientNoThinking：evThinking 进 chatEncoder 不产出任何思考
// 形态（D5：hub 不对 chat 客户端发明协议）。
func TestD5ChatClientNoThinking(t *testing.T) {
	buf := &w3Buf{}
	enc := newChatEncoder(&sseOut{w: buf, flush: buf.flush})
	enc.Encode(gwEvent{kind: evThinking, text: "思考"})
	enc.Encode(gwEvent{kind: evText, text: "答案"})
	enc.Finish()
	s := buf.String()
	if strings.Contains(s, "reasoning_content") || strings.Contains(s, "思考") {
		t.Errorf("D5：chat 客户端不应收到思考: %s", s)
	}
	if !strings.Contains(s, "答案") {
		t.Errorf("正文丢失: %s", s)
	}
}

// TestGeminiThoughtDecode：上游 gemini thought part → evThinking（进
// messages encoder 变 thinking 块）。
func TestGeminiThoughtDecode(t *testing.T) {
	d := newGeminiDecoder()
	evs, err := d.Feed(sseEvent{Data: []byte(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"思考","thought":true},{"text":"答"}]},"finishReason":"STOP"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var gotThinking, gotText bool
	for _, ev := range evs {
		if ev.kind == evThinking && ev.text == "思考" {
			gotThinking = true
		}
		if ev.kind == evText && ev.text == "答" {
			gotText = true
		}
	}
	if !gotThinking || !gotText {
		t.Errorf("thought/text 解析缺失: %+v", evs)
	}
}

// TestC2TimeoutErrNote：504/524 语义分级（C2）——候选摘要前缀 + 全败
// 结构化文案；其余状态码不注入（原始体原样回传语义不变）。
func TestC2TimeoutErrNote(t *testing.T) {
	if upstreamErrNote(504) != "网关/代理超时（504）" || upstreamErrNote(524) != "后端超时（Cloudflare 524）" || upstreamErrNote(500) != "" {
		t.Errorf("upstreamErrNote 分级错误: 504=%q 524=%q 500=%q", upstreamErrNote(504), upstreamErrNote(524), upstreamErrNote(500))
	}
	s := &Server{}
	for _, tc := range []struct {
		status int
		want   string
	}{
		{524, "后端超时（Cloudflare 524）"},
		{504, "网关/代理超时（504）"},
	} {
		rec := httptest.NewRecorder()
		s.relayLastError(rec, ProtoMessages, attemptResult{status: tc.status, body: []byte("upstream says no")})
		if rec.Code != tc.status {
			t.Errorf("[%d] 状态码应透传: %d", tc.status, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("[%d] 缺语义文案: %s", tc.status, rec.Body.String())
		}
	}
	// 非 504/524：原始体原样（既有语义）
	rec := httptest.NewRecorder()
	s.relayLastError(rec, ProtoMessages, attemptResult{status: 500, body: []byte("raw-upstream-body")})
	if rec.Body.String() != "raw-upstream-body" {
		t.Errorf("非 504/524 应原样回传: %s", rec.Body.String())
	}
}

// TestC3SystemReminder：多 system 消息——首条进顶层 system，中途 system
// 包 <system-reminder> user 消息（Claude Code 兼容惯例）。
func TestC3SystemReminder(t *testing.T) {
	cr := &ChatRequest{Model: "m", MaxTokens: 100, Messages: []ChatMessage{
		{Role: "system", Content: "主系统提示"},
		{Role: "user", Content: "问题"},
		{Role: "system", Content: "中途注入提醒"},
	}}
	mr := chatToMessagesRequest(cr)
	if mr.System != "主系统提示" {
		t.Errorf("首条 system 应进顶层: %v", mr.System)
	}
	found := false
	for _, m := range mr.Messages {
		if m.Role == "user" {
			if s, ok := m.Content.([]Block); ok && len(s) == 1 &&
				strings.Contains(s[0].Text, "<system-reminder>") && strings.Contains(s[0].Text, "中途注入提醒") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("中途 system 未按 C3 包装: %+v", mr.Messages)
	}
	// 单 system（现状主流路径）行为不变
	cr2 := &ChatRequest{Model: "m", MaxTokens: 100, Messages: []ChatMessage{
		{Role: "system", Content: "唯一系统"},
		{Role: "user", Content: "hi"},
	}}
	mr2 := chatToMessagesRequest(cr2)
	if mr2.System != "唯一系统" || len(mr2.Messages) != 1 {
		t.Errorf("单 system 现状行为漂移: %v %+v", mr2.System, mr2.Messages)
	}
}
