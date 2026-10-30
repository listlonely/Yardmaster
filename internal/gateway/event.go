// M5-W1 Event 化中间流：以内存事件 gwEvent 为唯一中间表示，替代
// M4 的「chat chunk 中间流」（原方案经 SSE 字节序列化再拆包，存在
// JSON round-trip 损耗，splitSSEData 已删）。设计参考 Magpie ir.go 的
// Event/EventKind（按本项目七类裁剪）：四协议各一对 decoder/encoder
// 适配器，translateStream 变为「上游 decoder → 逐事件 → 客户端
// encoder」。停止原因与工具索引在事件层使用 chat 词表/顺序号，协议
// 特有词表（end_turn/tool_use、completed、STOP…）由各 encoder 就地映射。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
)

// ---------- 事件模型 ----------

// gwEventKind 是流式事件类别（七类）。
type gwEventKind int

const (
	evStart     gwEventKind = iota // 流开始/元信息更新：msgID、model（可重复，ID 更新时再发）
	evText                         // 文本增量：text
	evToolStart                    // 工具调用开始：idx、id、name（idx 已见时为名字补记）
	evToolArgs                     // 工具参数增量：idx、text（partial JSON）
	evToolDone                     // 单个工具调用收束：idx（协议无对应事件时可忽略）
	evStop                         // 流终止：stop（chat 词表 stop/length/tool_calls/content_filter）
	evUsage                        // 用量快照：usage
)

// gwEvent 是一条内存事件（decoder 与 encoder 之间的唯一中间表示）。
type gwEvent struct {
	kind  gwEventKind
	msgID string // evStart：响应 ID
	model string // evStart：模型名
	text  string // evText/evToolArgs：增量文本
	idx   int    // 工具调用顺序号（decoder 已归一）
	id    string // evToolStart：工具调用 ID
	name  string // evToolStart：工具名
	stop  string // evStop：chat 词表停止原因
	usage gwUsage
}

// gwUsage 是事件层用量快照。
type gwUsage struct {
	prompt     int
	completion int
}

// ---------- 适配器接口 ----------

// eventDecoder 把上游协议 SSE 解码为事件。Feed 每条上游事件调用一次；
// 流结束后调 Done 产出收尾事件（终止/兜底）；错误仅记录并丢弃当批事件
// （沿用 M4 行为：单条坏事件不中断流）。
type eventDecoder interface {
	Feed(ev sseEvent) ([]gwEvent, error)
	Done() []gwEvent
}

// eventEncoder 把事件编码为客户端协议 SSE。Finish 输出协议收尾
// （chat 的终止 chunk+[DONE]、messages 的 message_delta/message_stop、
// responses 的 response.completed、gemini 的终止事件）。
type eventEncoder interface {
	Encode(ev gwEvent)
	Finish()
}

// newEventDecoder 按上游协议构造 decoder。
func newEventDecoder(p ClientProto) eventDecoder {
	switch p {
	case ProtoMessages:
		return newMsgsDecoder()
	case ProtoResponses:
		return newRespDecoder()
	case ProtoGemini:
		return newGeminiDecoder()
	default:
		return newChatDecoder()
	}
}

// newEventEncoder 按客户端协议构造 encoder。
func newEventEncoder(p ClientProto, w io.Writer, flush func()) eventEncoder {
	out := &sseOut{w: w, flush: flush}
	switch p {
	case ProtoMessages:
		return newMsgsEncoder(out)
	case ProtoResponses:
		return newRespEncoder(out)
	case ProtoGemini:
		return newGeminiEncoder(out)
	default:
		return newChatEncoder(out)
	}
}

// sseOut 聚合写与冲刷（encoder 逐事件写、逐事件 flush）。
type sseOut struct {
	w     io.Writer
	flush func()
}

func (o *sseOut) emit(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	o.raw(name, b)
}

func (o *sseOut) raw(name string, b []byte) {
	buf := appendSSE(nil, name, b)
	_, _ = o.w.Write(buf)
	if o.flush != nil {
		o.flush()
	}
}

// ---------- Chat decoder：上游 chat chunk → 事件 ----------

// chatDecoder 解析上游 chat chunk（原 translateStream 的透传分支，
// M5-W1 显式建模；chat→chat 同协议走 io.Copy 透传，不经此）。
type chatDecoder struct {
	started  bool
	startID  string // 首个 evStart 携带的响应 ID（中途更新判重）
	model    string
	seenTool map[int]bool
	stop     string
	hadStop  bool
}

func newChatDecoder() *chatDecoder { return &chatDecoder{seenTool: map[int]bool{}} }

func (d *chatDecoder) Feed(ev sseEvent) ([]gwEvent, error) {
	if string(ev.Data) == "[DONE]" {
		return nil, nil
	}
	var cc ChatChunk
	if err := json.Unmarshal(ev.Data, &cc); err != nil {
		return nil, fmt.Errorf("解析上游 chat 块: %w", err)
	}
	var out []gwEvent
	if d.model == "" && cc.Model != "" {
		d.model = cc.Model
	}
	for _, ch := range cc.Choices {
		if !d.started {
			d.started = true
			d.startID = cc.ID
			out = append(out, gwEvent{kind: evStart, msgID: cc.ID, model: d.model})
		}
		if s, ok := ch.Delta.Content.(string); ok && s != "" {
			out = append(out, gwEvent{kind: evText, text: s})
		}
		for _, tc := range ch.Delta.ToolCalls {
			out = append(out, d.toolCall(tc)...)
		}
		if ch.FinishReason != "" {
			d.stop, d.hadStop = ch.FinishReason, true
		}
	}
	// ID 在流中途更新（少见）：补发 evStart，encoder 侧仅 resp 取用。
	if d.started && cc.ID != "" && cc.ID != d.startID {
		d.startID = cc.ID
		out = append(out, gwEvent{kind: evStart, msgID: cc.ID})
	}
	if cc.Usage != nil {
		out = append(out, gwEvent{kind: evUsage, usage: gwUsage{
			prompt: cc.Usage.PromptTokens, completion: cc.Usage.CompletionTokens,
		}})
	}
	return out, nil
}

// toolCall 归一一条 tool_calls 增量：新 index 开工具调用，已知 index
// 只推进参数（名字后到时补发 evStart 形式的 evToolStart 供 encoder 补记）。
func (d *chatDecoder) toolCall(tc ChatToolCall) []gwEvent {
	if !d.seenTool[tc.Index] {
		d.seenTool[tc.Index] = true
		return []gwEvent{{kind: evToolStart, idx: tc.Index, id: tc.ID, name: tc.Function.Name}}
	}
	var out []gwEvent
	if tc.ID == "" && tc.Function.Name != "" {
		out = append(out, gwEvent{kind: evToolStart, idx: tc.Index, name: tc.Function.Name})
	}
	if tc.Function.Arguments != "" {
		out = append(out, gwEvent{kind: evToolArgs, idx: tc.Index, text: tc.Function.Arguments})
	}
	return out
}

func (d *chatDecoder) Done() []gwEvent {
	if d.hadStop {
		return []gwEvent{{kind: evStop, stop: d.stop}}
	}
	return nil // 无终止 chunk：由 encoder Finish 兜底 stop
}

// ---------- Chat encoder：事件 → 客户端 chat chunk SSE ----------

// chatEncoder 输出 chat.completion.chunk 流；终止 chunk 与 usage 合并在
// Finish 输出，尾随 [DONE]。
type chatEncoder struct {
	out     *sseOut
	started bool
	stop    string
	usage   *gwUsage
}

func newChatEncoder(out *sseOut) *chatEncoder { return &chatEncoder{out: out} }

func (e *chatEncoder) chunk(delta ChatMessage, finish string, usage *ChatUsage) {
	chunk := map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	e.out.emit("", chunk)
}

func (e *chatEncoder) Encode(ev gwEvent) {
	switch ev.kind {
	case evStart:
		if !e.started {
			e.started = true
			e.chunk(ChatMessage{Role: "assistant"}, "", nil)
		}
	case evText:
		e.chunk(ChatMessage{Content: ev.text}, "", nil)
	case evToolStart:
		e.chunk(ChatMessage{ToolCalls: []ChatToolCall{{
			Index: ev.idx, ID: ev.id, Type: "function", Function: ChatFunction{Name: ev.name},
		}}}, "", nil)
	case evToolArgs:
		e.chunk(ChatMessage{ToolCalls: []ChatToolCall{{
			Index: ev.idx, Type: "function", Function: ChatFunction{Arguments: ev.text},
		}}}, "", nil)
	case evStop:
		e.stop = ev.stop
	case evUsage:
		u := ev.usage
		e.usage = &u
	}
}

func (e *chatEncoder) Finish() {
	if !e.started {
		e.chunk(ChatMessage{Role: "assistant"}, "", nil)
	}
	stop := orDefault(e.stop, "stop")
	var cu *ChatUsage
	if e.usage != nil {
		cu = &ChatUsage{PromptTokens: e.usage.prompt, CompletionTokens: e.usage.completion}
	}
	e.chunk(ChatMessage{}, stop, cu)
	e.out.raw("", []byte("[DONE]"))
}
