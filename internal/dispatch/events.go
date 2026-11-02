// 事件级实时（M6-W3）：stdout 字节流喂入的 jsonl 事件解析器。词表依据
// M6-W1 Claude Code stream-json 真实冒烟核实（dispatch.md §五）：
//
//	{"type":"system","subtype":"init",…}          会话初始化
//	{"type":"assistant","message":{content:[…]}}  文本块 / 工具调用
//	{"type":"user",…}                             工具轮次回传
//	{"type":"result","subtype":"success|error",…} 终态
//
// 读不懂=未知：无法识别为已知类型的行原样保留为 raw 事件（内容零丢失，
// 视图退化为纯文本——容错纪律不变）。事件仅是输出视图的再加工，不改变
// Result.Output 留存与 ExtractResult 语义。
package dispatch

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// EventKind 是事件的归类词表（控制台着色/折叠依据）。
type EventKind string

const (
	EventInit    EventKind = "init"       // system/init：会话初始化（进行态起点）
	EventText    EventKind = "text"       // assistant 文本块
	EventTool    EventKind = "tool"       // tool_use 工具调用（视图折叠展示）
	EventToolRes EventKind = "toolresult" // 工具轮次回传（user 行）
	EventResult  EventKind = "result"     // 终态 result（Final=true）
	EventRaw     EventKind = "raw"        // 读不懂=未知：原样保留（回退纯文本）
)

// Event 是派发输出的一个结构化事件（JSON 直出控制台/CLI 渲染）。
type Event struct {
	Kind    EventKind `json:"kind"`
	Text    string    `json:"text,omitempty"`    // 文本/结果/工具摘要
	Tool    string    `json:"tool,omitempty"`    // 工具名（tool/toolresult）
	Subtype string    `json:"subtype,omitempty"` // init/result 的子类型
	Final   bool      `json:"final,omitempty"`   // 终态事件
}

// 单事件留存上限（字节）与事件条数护栏（内存纪律：与尾部 64KB 同量级）。
const (
	textEventCap = 8 << 10
	toolInputCap = 512
	toolResCap   = 2 << 10
	eventsKeep   = 512
)

// EventParser 是流式 jsonl 行解析器：单写者 Feed，完整行逐条归类；
// 半行暂存至 Flush（运行期视图读取不冲刷，避免半行与后续分块重复）。
type EventParser struct {
	buf    []byte
	events []Event
}

func NewEventParser() *EventParser { return &EventParser{} }

// Feed 喂入任意分块字节；返回本次新解析出的事件。
func (p *EventParser) Feed(b []byte) []Event {
	p.buf = append(p.buf, b...)
	var out []Event
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i]
		p.buf = p.buf[i+1:]
		out = append(out, classifyLine(string(line))...)
	}
	p.events = append(p.events, out...)
	p.trim()
	return out
}

// Flush 冲出行缓冲残留（无换行的最后半行）；会话终态时调用一次。
func (p *EventParser) Flush() []Event {
	if len(p.buf) == 0 {
		return nil
	}
	line := string(p.buf)
	p.buf = nil
	out := classifyLine(line)
	p.events = append(p.events, out...)
	p.trim()
	return out
}

// Events 返回留存事件快照（旧→新）。调用方负责并发保护。
func (p *EventParser) Events() []Event {
	return append([]Event(nil), p.events...)
}

// trim 环形护栏：超出 eventsKeep 丢最旧。
func (p *EventParser) trim() {
	if len(p.events) > eventsKeep {
		p.events = p.events[len(p.events)-eventsKeep:]
	}
}

// ParseEvents 非流式便捷入口：整段输出解析为事件（恢复会话/存量尾部
// 现场解析用；无尾换行的最后一行经 Flush 收口）。返回留存快照（封顶）；
// 需要全量事件流的场景直接用 Feed 返回值（流式渲染语义，不封顶）。
func ParseEvents(output string) []Event {
	p := NewEventParser()
	p.Feed([]byte(output))
	p.Flush()
	return p.Events()
}

// classifyLine 按词表归类一行；不认识的原样回退（读不懂=未知，不猜）。
func classifyLine(line string) []Event {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(line), &obj) != nil {
		return []Event{{Kind: EventRaw, Text: line}}
	}
	switch typ, _ := obj["type"].(string); typ {
	case "system":
		sub, _ := obj["subtype"].(string)
		return []Event{{Kind: EventInit, Subtype: sub, Text: sub}}
	case "assistant":
		return assistantEvents(obj, line)
	case "user":
		return userEvents(obj, line)
	case "result":
		return resultEvents(obj)
	default:
		return []Event{{Kind: EventRaw, Text: line}}
	}
}

// assistantEvents 拆分 content 块：text → 文本事件、tool_use → 工具事件；
// 无可识别块时整行原样回退（thinking 等未知块不猜语义）。
func assistantEvents(obj map[string]any, line string) []Event {
	msg, _ := obj["message"].(map[string]any)
	blocks, _ := msg["content"].([]any)
	var out []Event
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bt, _ := bm["type"].(string); bt {
		case "text":
			txt, _ := bm["text"].(string)
			out = append(out, Event{Kind: EventText, Text: cutTail(txt, textEventCap)})
		case "tool_use":
			name, _ := bm["name"].(string)
			out = append(out, Event{Kind: EventTool, Tool: name, Text: toolSummary(bm["input"])})
		}
	}
	if len(out) == 0 {
		return []Event{{Kind: EventRaw, Text: line}}
	}
	return out
}

// userEvents 是工具轮次回传（W1 词表）：content 为字符串或 tool_result
// 块数组；形状读不懂时整行回退。
func userEvents(obj map[string]any, line string) []Event {
	msg, _ := obj["message"].(map[string]any)
	switch c := msg["content"].(type) {
	case string:
		return []Event{{Kind: EventToolRes, Text: cutTail(c, toolResCap)}}
	case []any:
		var out []Event
		for _, b := range c {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bt, _ := bm["type"].(string); bt == "tool_result" {
				out = append(out, Event{Kind: EventToolRes, Text: toolResultSummary(bm)})
			}
		}
		if len(out) == 0 {
			return []Event{{Kind: EventRaw, Text: line}}
		}
		return out
	default:
		return []Event{{Kind: EventRaw, Text: line}}
	}
}

// resultEvents 提取终态：W1 实测结果在 result 字段；text/content 兜底
// 与 ExtractResult 键表对齐。
func resultEvents(obj map[string]any) []Event {
	sub, _ := obj["subtype"].(string)
	txt, _ := obj["result"].(string)
	if txt == "" {
		for _, k := range []string{"text", "content"} {
			if s, ok := obj[k].(string); ok && s != "" {
				txt = s
				break
			}
		}
	}
	return []Event{{Kind: EventResult, Subtype: sub, Text: cutTail(txt, textEventCap), Final: true}}
}

// toolSummary 把 tool_use 的 input 折叠为单行 JSON 摘要（原样最稳）。
func toolSummary(input any) string {
	if input == nil {
		return ""
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "" // JSON 解码产物恒可再序列化；防御性兜底
	}
	return cutHead(strings.TrimSpace(string(b)), toolInputCap)
}

// toolResultSummary 提取 tool_result 的文本内容（串或 text 块数组）。
func toolResultSummary(bm map[string]any) string {
	switch c := bm["content"].(type) {
	case string:
		return cutTail(c, toolResCap)
	case []any:
		var parts []string
		for _, b := range c {
			if t, ok := b.(map[string]any); ok {
				if s, ok := t["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return cutTail(strings.Join(parts, "\n"), toolResCap)
	default:
		return ""
	}
}

// RenderEvent 把事件渲染为 CLI 单行文本（--events 实时流；raw 原样回显
// ——非 jsonl 输出自然退化为透传）。
func RenderEvent(ev Event) string {
	switch ev.Kind {
	case EventInit:
		if ev.Subtype != "" {
			return "[init] " + ev.Subtype
		}
		return "[init]"
	case EventText:
		return ev.Text
	case EventTool:
		return "[tool] " + oneLine(ev.Tool+": "+ev.Text)
	case EventToolRes:
		return "[tool-result] " + oneLine(ev.Text)
	case EventResult:
		tag := "result"
		if ev.Subtype != "" {
			tag += "·" + ev.Subtype
		}
		return "[" + tag + "] " + oneLine(ev.Text)
	default: // EventRaw：原样
		return ev.Text
	}
}

// oneLine 把多行摘要折叠为单行（CLI 事件流保持行结构 = 事件边界）。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cutHead 保头部截断（工具入参摘要：键名在前更有用）；UTF-8 安全。
func cutHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// cutTail 保尾部截断（文本/回传：结尾是最新进展，与尾部留存同语义）。
func cutTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	for len(cut) > 0 {
		r, sz := utf8.DecodeRuneInString(cut)
		if r != utf8.RuneError || sz != 1 {
			break
		}
		cut = cut[sz:]
	}
	return "…" + cut
}
