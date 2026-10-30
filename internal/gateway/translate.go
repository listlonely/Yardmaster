// 跨协议翻译：Chat ↔ Messages 的请求、非流响应与双向流式状态机。
// W2 覆盖文本 + 工具定义/调用（tool_use ↔ tool_calls、tool_result ↔
// role:"tool"）；thinking/多模态块不在 W2 范围（收到即忽略文本以外的
// 块并记 TODO(W5)）。
package gateway

import (
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
		// anthropic 出口：input_tokens 不含 cache_read（cached 含于上游
		// prompt，展开为两列——M6-W1 校准）。
		var cached int
		if cc.Usage.PromptTokensDetails != nil {
			cached = cc.Usage.PromptTokensDetails.CachedTokens
		}
		ms.Usage.InputTokens = cc.Usage.PromptTokens - cached
		ms.Usage.OutputTokens = cc.Usage.CompletionTokens
		ms.Usage.CacheReadInputTokens = cached
	}
	if ms.ID == "" {
		ms.ID = "msg_yardmaster"
	}
	return ms
}

// ---------- 流式翻译：事件 → 客户端 Messages SSE（M5-W1 encoder） ----------

// msgsEncoder 把 gwEvent 编码为 anthropic SSE 事件（承接 M4 chatToMsgsStream
// 的块状态机：text/tool_use 块开关、块索引、stop_reason 映射）。
type msgsEncoder struct {
	out       *sseOut
	model     string
	started   bool
	curBlock  int // 当前打开的 anthropic 块索引；-1 无
	curIsText bool
	textOpen  bool
	nextBlock int
	toolBlks  map[int]int // 事件工具顺序号 → anthropic 块索引
	stopMsg   string
	usage     gwUsage // 真实用量（evUsage 携带；M6-W1 校准：此前硬编码 0）
}

func newMsgsEncoder(out *sseOut) *msgsEncoder {
	return &msgsEncoder{out: out, curBlock: -1, toolBlks: map[int]int{}}
}

func (t *msgsEncoder) emit(name string, v any) { t.out.emit(name, v) }

func (t *msgsEncoder) openBlock(kind string, blk *Block) {
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

func (t *msgsEncoder) start() {
	if t.started {
		return
	}
	t.started = true
	msg := map[string]any{"type": "message", "role": "assistant", "content": []any{}, "model": t.model}
	t.emit("message_start", map[string]any{"type": "message_start", "message": msg})
}

func (t *msgsEncoder) Encode(ev gwEvent) {
	switch ev.kind {
	case evStart:
		if t.model == "" {
			t.model = ev.model
		}
		t.start()
	case evText:
		t.start() // 惰性启动：上游无显式开始事件时（resp/gemini），首个内容前发 message_start
		if !t.textOpen || !t.curIsText {
			t.openBlock("text", nil)
		}
		t.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": t.curBlock,
			"delta": map[string]any{"type": "text_delta", "text": ev.text},
		})
	case evToolStart:
		t.start() // 惰性启动：工具调用先于文本时同样需要 message_start 在首
		if _, ok := t.toolBlks[ev.idx]; !ok {
			t.toolBlks[ev.idx] = t.nextBlock
			blk := &Block{Type: "tool_use", ID: ev.id, Name: ev.name, Input: map[string]any{}}
			t.openBlock("tool_use", blk)
		}
	case evToolArgs:
		t.start()
		if blkIdx, ok := t.toolBlks[ev.idx]; ok && ev.text != "" {
			t.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": blkIdx,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.text},
			})
		}
	case evStop:
		if ev.stop != "" {
			t.stopMsg = chatFinishToMsg(ev.stop)
		}
	case evUsage:
		t.usage = ev.usage
	}
}

// Finish 收尾：关闭打开块，输出 message_delta/message_stop。
func (t *msgsEncoder) Finish() {
	t.start()
	if t.curBlock >= 0 {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.curBlock})
		t.curBlock = -1
		t.textOpen = false
	}
	if t.stopMsg == "" {
		t.stopMsg = "end_turn"
	}
	// anthropic 出口语义：input_tokens 不含 cache_read（openai/gemini 的
	// cached 含于 prompt，此处展开为「未命中 + 命中」两列）。
	usage := map[string]any{"input_tokens": t.usage.prompt - t.usage.cached, "output_tokens": t.usage.completion}
	if t.usage.cached > 0 {
		usage["cache_read_input_tokens"] = t.usage.cached
	}
	t.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": t.stopMsg},
		"usage": usage,
	})
	t.emit("message_stop", map[string]any{"type": "message_stop"})
}

// ---------- 流式翻译：上游 Messages SSE → 事件（M5-W1 decoder） ----------

// msgsDecoder 解析上游 anthropic SSE 事件（承接 M4 msgsToChatStream 的
// 块索引→顺序号归一与 stop 映射）。
type msgsDecoder struct {
	started    bool
	stopChat   string
	toolIdx    map[int]int // anthropic 块索引 → 事件工具顺序号
	toolCount  int
	outTokens  int         // message_delta 累计 output_tokens（usage 出现在流尾）
	sawInUsage bool        // message_start 已带输入侧 usage
	inUsage    gwUsage
}

func newMsgsDecoder() *msgsDecoder { return &msgsDecoder{toolIdx: map[int]int{}} }

// Feed 处理一条上游 anthropic SSE 事件（name/data 成对）。
func (d *msgsDecoder) Feed(ev sseEvent) ([]gwEvent, error) {
	if len(ev.Data) == 0 {
		return nil, nil
	}
	var e msgEvent
	if err := json.Unmarshal(ev.Data, &e); err != nil {
		return nil, fmt.Errorf("翻译上游 messages 事件: %w", err)
	}
	if e.Type == "" && ev.Name != "" {
		e.Type = ev.Name
	}
	var out []gwEvent
	switch e.Type {
	case "message_start":
		if !d.started {
			d.started = true
			model := ""
			if e.Message != nil {
				model = e.Message.Model
				// 输入侧 usage（input_tokens 不含 cache_read；统一折叠为
				// 「prompt 含命中」口径）。
				if u := e.Message.Usage; u != nil {
					d.sawInUsage = true
					d.inUsage = gwUsage{
						prompt: u.InputTokens + u.CacheReadInputTokens,
						cached: u.CacheReadInputTokens,
					}
				}
			}
			out = append(out, gwEvent{kind: evStart, model: model})
		}
	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			idx := d.toolCount
			d.toolCount++
			d.toolIdx[e.Index] = idx
			out = append(out, gwEvent{kind: evToolStart, idx: idx, id: e.ContentBlock.ID, name: e.ContentBlock.Name})
		}
	case "content_block_delta":
		switch e.Delta.Type {
		case "text_delta":
			if e.Delta.Text != "" {
				out = append(out, gwEvent{kind: evText, text: e.Delta.Text})
			}
		case "input_json_delta":
			if idx, ok := d.toolIdx[e.Index]; ok && e.Delta.PartialJSON != "" {
				out = append(out, gwEvent{kind: evToolArgs, idx: idx, text: e.Delta.PartialJSON})
			}
		}
	case "message_delta":
		if e.Delta.StopReason != "" {
			d.stopChat = msgStopToChat(e.Delta.StopReason)
		}
		if e.Usage != nil && e.Usage.OutputTokens > 0 {
			d.outTokens = e.Usage.OutputTokens
		}
	case "ping":
		// 忽略
	case "error":
		return nil, fmt.Errorf("上游流式错误: %s", func() string {
			if e.Error != nil {
				return e.Error.Message
			}
			return "unknown"
		}())
	}
	return out, nil
}

// Done 收尾：未启动则补 start，输出终止事件（stop 兜底 stop）与输入侧
// usage 快照（输出侧 token 在 message_delta 已随 evUsage 携带则合并）。
func (d *msgsDecoder) Done() []gwEvent {
	var out []gwEvent
	if !d.started {
		d.started = true
		out = append(out, gwEvent{kind: evStart})
	}
	if d.stopChat == "" {
		d.stopChat = "stop"
	}
	out = append(out, gwEvent{kind: evStop, stop: d.stopChat})
	if d.sawInUsage || d.outTokens > 0 {
		u := d.inUsage
		u.completion = d.outTokens
		out = append(out, gwEvent{kind: evUsage, usage: u})
	}
	return out
}
