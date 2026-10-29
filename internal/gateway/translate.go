// 跨协议翻译：Chat ↔ Messages 的请求、非流响应与双向流式状态机。
// W2 覆盖文本 + 工具定义/调用（tool_use ↔ tool_calls、tool_result ↔
// role:"tool"）；thinking/多模态块不在 W2 范围（收到即忽略文本以外的
// 块并记 TODO(W5)）。
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- 停止原因映射 ----------

func msgStopToChat(s string) string {
	switch s {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default: // end_turn / stop_sequence / ""
		return "stop"
	}
}

func chatFinishToMsg(s string) string {
	switch s {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default: // stop / ""
		return "end_turn"
	}
}

// ---------- 请求翻译 ----------

// messagesToChatRequest 把 Messages 请求翻译为 Chat 请求。
func messagesToChatRequest(mr *MessagesRequest) *ChatRequest {
	cr := &ChatRequest{Model: mr.Model, Stream: mr.Stream, MaxTokens: mr.MaxTokens}
	if mr.System != nil {
		if sys := contentText(mr.System); sys != "" {
			cr.Messages = append(cr.Messages, ChatMessage{Role: "system", Content: sys})
		}
	}
	for _, m := range mr.Messages {
		switch m.Role {
		case "assistant":
			cm := ChatMessage{Role: "assistant", Content: contentText(m.Content)}
			for _, blk := range toolUseBlocks(m.Content) {
				args, _ := json.Marshal(blk.Input)
				cm.ToolCalls = append(cm.ToolCalls, ChatToolCall{
					ID:       blk.ID,
					Type:     "function",
					Function: ChatFunction{Name: blk.Name, Arguments: string(args)},
				})
			}
			cr.Messages = append(cr.Messages, cm)
		case "user":
			results, text := splitToolResults(m.Content)
			for _, r := range results {
				cr.Messages = append(cr.Messages, ChatMessage{
					Role:       "tool",
					ToolCallID: r.ToolUseID,
					Content:    contentText(r.Content),
				})
			}
			if text != "" {
				cr.Messages = append(cr.Messages, ChatMessage{Role: "user", Content: text})
			}
		default:
			cr.Messages = append(cr.Messages, ChatMessage{Role: m.Role, Content: contentText(m.Content)})
		}
	}
	for _, t := range mr.Tools {
		var ct ChatTool
		ct.Type = "function"
		ct.Function.Name = t.Name
		ct.Function.Description = t.Description
		ct.Function.Parameters = t.InputSchema
		cr.Tools = append(cr.Tools, ct)
	}
	return cr
}

// chatToMessagesRequest 把 Chat 请求翻译为 Messages 请求。
// Anthropic 要求 max_tokens 必填，缺省 4096。
func chatToMessagesRequest(cr *ChatRequest) *MessagesRequest {
	mr := &MessagesRequest{Model: cr.Model, Stream: cr.Stream, MaxTokens: cr.MaxTokens}
	if mr.MaxTokens <= 0 {
		mr.MaxTokens = 4096
	}
	var sys []string
	for _, m := range cr.Messages {
		switch m.Role {
		case "system":
			if s := contentText(m.Content); s != "" {
				sys = append(sys, s)
			}
		case "tool":
			mr.Messages = append(mr.Messages, MessagesMessage{
				Role: "user",
				Content: []Block{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   contentText(m.Content),
				}},
			})
		case "assistant":
			var blocks []Block
			if s := contentText(m.Content); s != "" {
				blocks = append(blocks, Block{Type: "text", Text: s})
			}
			for _, tc := range m.ToolCalls {
				var input any
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
				blocks = append(blocks, Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = []Block{{Type: "text", Text: ""}}
			}
			mr.Messages = append(mr.Messages, MessagesMessage{Role: "assistant", Content: blocks})
		default:
			mr.Messages = append(mr.Messages, MessagesMessage{Role: m.Role, Content: contentText(m.Content)})
		}
	}
	if len(sys) > 0 {
		mr.System = strings.Join(sys, "\n")
	}
	for _, t := range cr.Tools {
		mr.Tools = append(mr.Tools, AnthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return mr
}

// ---------- 非流响应翻译 ----------

// chatResponseToMessages 把 Chat 非流响应翻译为 Messages 响应
// （客户端是 Messages、上游是 Chat 的场景）。
func chatResponseToMessages(mr *MessagesRequest, cc *ChatChunk) *MessagesResponse {
	ms := &MessagesResponse{Type: "message", Role: "assistant", Model: cc.Model}
	if ms.Model == "" {
		ms.Model = mr.Model
	}
	if len(cc.Choices) == 0 {
		ms.StopReason = "end_turn"
		return ms
	}
	ch := cc.Choices[0]
	if ch.Message != nil {
		if s := contentText(ch.Message.Content); s != "" {
			ms.Content = append(ms.Content, Block{Type: "text", Text: s})
		}
		for _, tc := range ch.Message.ToolCalls {
			var input any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			ms.Content = append(ms.Content, Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
		}
	}
	ms.StopReason = chatFinishToMsg(ch.FinishReason)
	if cc.Usage != nil {
		ms.Usage.InputTokens = cc.Usage.PromptTokens
		ms.Usage.OutputTokens = cc.Usage.CompletionTokens
	}
	if ms.ID == "" {
		ms.ID = "msg_agenthub"
	}
	return ms
}

// ---------- 流式翻译：上游 Chat SSE → 客户端 Messages SSE ----------

// chatToMsgsStream 是无状态推进器：Feed 一条上游 data，吐出若干客户端
// SSE 事件；流结束后调 Done 收尾。
type chatToMsgsStream struct {
	model      string
	started    bool
	curBlock   int  // 当前打开的 anthropic 块索引；-1 无
	curIsText  bool
	textOpen   bool
	nextBlock  int
	toolBlocks map[int]int    // chat tool index → anthropic 块索引
	stopReason string
	out        []byte
}

func newChatToMsgsStream() *chatToMsgsStream {
	return &chatToMsgsStream{curBlock: -1, toolBlocks: map[int]int{}}
}

func (t *chatToMsgsStream) emit(name string, v any) {
	b, _ := json.Marshal(v)
	t.out = appendSSE(t.out, name, b)
}

func (t *chatToMsgsStream) openBlock(kind string, blk *Block) {
	if t.curBlock >= 0 {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.curBlock})
	}
	t.curBlock = t.nextBlock
	t.nextBlock++
	if kind == "text" {
		t.curIsText, t.textOpen = true, true
		t.emit("content_block_start", map[string]any{"type": "content_block_start", "index": t.curBlock, "content_block": Block{Type: "text"}})
	} else {
		t.curIsText = false
		t.emit("content_block_start", map[string]any{"type": "content_block_start", "index": t.curBlock, "content_block": blk})
	}
}

func (t *chatToMsgsStream) start() {
	if t.started {
		return
	}
	t.started = true
	msg := map[string]any{"type": "message", "role": "assistant", "content": []any{}, "model": t.model}
	t.emit("message_start", map[string]any{"type": "message_start", "message": msg})
}

// Feed 处理一条上游 chat SSE data（不含 "data:" 前缀；"[DONE]" 交由 Done）。
func (t *chatToMsgsStream) Feed(data []byte) error {
	if string(bytes.TrimSpace(data)) == "[DONE]" {
		return nil
	}
	var chunk ChatChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("翻译上游 chat 块: %w", err)
	}
	if t.model == "" {
		t.model = chunk.Model
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	delta := chunk.Choices[0].Delta
	t.start()
	if delta.Content != nil {
		if s, ok := delta.Content.(string); ok && s != "" {
			if !t.textOpen || !t.curIsText {
				t.openBlock("text", nil)
			}
			t.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": t.curBlock,
				"delta": map[string]any{"type": "text_delta", "text": s},
			})
		}
	}
	for _, tc := range delta.ToolCalls {
		blkIdx, ok := t.toolBlocks[tc.Index]
		if !ok {
			blkIdx = t.nextBlock
			t.toolBlocks[tc.Index] = blkIdx
			blk := &Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: map[string]any{}}
			t.openBlock("tool_use", blk)
		}
		if tc.Function.Arguments != "" {
			t.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blkIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
			})
		}
	}
	if fr := chunk.Choices[0].FinishReason; fr != "" {
		t.stopReason = chatFinishToMsg(fr)
	}
	return nil
}

// Done 收尾：关闭打开块，输出 message_delta/message_stop。
func (t *chatToMsgsStream) Done() []byte {
	t.start()
	if t.curBlock >= 0 {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.curBlock})
		t.curBlock = -1
		t.textOpen = false
	}
	if t.stopReason == "" {
		t.stopReason = "end_turn"
	}
	t.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": t.stopReason},
		"usage": map[string]any{"output_tokens": 0},
	})
	t.emit("message_stop", map[string]any{"type": "message_stop"})
	out := t.out
	t.out = nil
	return out
}

// ---------- 流式翻译：上游 Messages SSE → 客户端 Chat SSE ----------

// msgsToChatStream 同样是无状态推进器。
type msgsToChatStream struct {
	started   bool
	stopChat  string
	toolIdx   map[int]int // anthropic 块索引 → chat tool index
	toolCount int
	out       []byte
}

func newMsgsToChatStream() *msgsToChatStream {
	return &msgsToChatStream{toolIdx: map[int]int{}}
}

func (t *msgsToChatStream) emitChunk(delta ChatMessage, finish string) {
	chunk := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	b, _ := json.Marshal(chunk)
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

// Feed 处理一条上游 anthropic SSE 事件（name/data 成对）。
func (t *msgsToChatStream) Feed(ev sseEvent) error {
	if string(bytes.TrimSpace(ev.Data)) == "" {
		return nil
	}
	var e msgEvent
	if err := json.Unmarshal(ev.Data, &e); err != nil {
		return fmt.Errorf("翻译上游 messages 事件: %w", err)
	}
	if e.Type == "" && ev.Name != "" {
		e.Type = ev.Name
	}
	switch e.Type {
	case "message_start":
		if !t.started {
			t.started = true
			t.emitChunk(ChatMessage{Role: "assistant"}, "")
		}
	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			idx := t.toolCount
			t.toolCount++
			t.toolIdx[e.Index] = idx
			t.emitChunk(ChatMessage{ToolCalls: []ChatToolCall{{
				Index: idx, ID: e.ContentBlock.ID, Type: "function",
				Function: ChatFunction{Name: e.ContentBlock.Name},
			}}}, "")
		}
	case "content_block_delta":
		switch e.Delta.Type {
		case "text_delta":
			if e.Delta.Text != "" {
				t.emitChunk(ChatMessage{Content: e.Delta.Text}, "")
			}
		case "input_json_delta":
			if idx, ok := t.toolIdx[e.Index]; ok && e.Delta.PartialJSON != "" {
				t.emitChunk(ChatMessage{ToolCalls: []ChatToolCall{{
					Index: idx, Type: "function",
					Function: ChatFunction{Arguments: e.Delta.PartialJSON},
				}}}, "")
			}
		}
	case "message_delta":
		if e.Delta.StopReason != "" {
			t.stopChat = msgStopToChat(e.Delta.StopReason)
		}
	case "ping":
		// 忽略
	case "error":
		return fmt.Errorf("上游流式错误: %s", func() string {
			if e.Error != nil {
				return e.Error.Message
			}
			return "unknown"
		}())
	}
	return nil
}

// Done 收尾：输出 finish_reason 与 [DONE]。
func (t *msgsToChatStream) Done() []byte {
	if !t.started {
		t.emitChunk(ChatMessage{Role: "assistant"}, "")
	}
	if t.stopChat == "" {
		t.stopChat = "stop"
	}
	t.emitChunk(ChatMessage{}, t.stopChat)
	t.out = append(t.out, []byte("data: [DONE]\n\n")...)
	out := t.out
	t.out = nil
	return out
}
