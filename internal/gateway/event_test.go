// M5-W1 事件层单测：四协议 decoder/encoder 的事件对照表（等价迁移的
// 单元级证明；矩阵级等价由既有 4×4 流式测试原样通过保证）。
package gateway

import (
	"strings"
	"testing"
)

// evs 格式化事件序列为可读对照行（kind + 关键字段）。
func evs(list []gwEvent) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		switch e.kind {
		case evStart:
			s := "start"
			if e.msgID != "" {
				s += ":" + e.msgID
			}
			if e.model != "" {
				s += "/" + e.model
			}
			out = append(out, s)
		case evText:
			out = append(out, "text:"+e.text)
		case evToolStart:
			out = append(out, "tool:"+itoa(e.idx)+":"+e.id+":"+e.name)
		case evToolArgs:
			out = append(out, "args:"+itoa(e.idx)+":"+e.text)
		case evToolDone:
			out = append(out, "tooldone:"+itoa(e.idx))
		case evStop:
			out = append(out, "stop:"+e.stop)
		case evUsage:
			out = append(out, "usage:"+itoa(e.usage.prompt)+"/"+itoa(e.usage.completion))
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func assertLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("事件序列不符:\n got  %v\n want %v", got, want)
	}
}

// runDecoder 对一段 SSE 文本逐事件驱动 decoder（含 Done）。
func runDecoder(t *testing.T, d eventDecoder, sse string) []string {
	t.Helper()
	var got []string
	for _, block := range strings.Split(strings.TrimRight(sse, "\n"), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				ev.Data = []byte(after)
			}
			if after, ok := strings.CutPrefix(line, "event: "); ok {
				ev.Name = after
			}
		}
		if ev.Data == nil {
			continue
		}
		out, err := d.Feed(ev)
		if err != nil {
			t.Fatalf("Feed 出错: %v", err)
		}
		got = append(got, evs(out)...)
	}
	got = append(got, evs(d.Done())...)
	return got
}

// ---------- decoder 对照表 ----------

func TestChatDecoderEventTable(t *testing.T) {
	d := newChatDecoder()
	got := runDecoder(t, d,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"up-m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":""}]}`+"\n\n"+
			`data: {"id":"c1","choices":[{"delta":{"content":"你"}}]}`+"\n\n"+
			`data: {"id":"c1","choices":[{"delta":{"content":"好"}}]}`+"\n\n"+
			`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":""}}]}}]}`+"\n\n"+
			`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`+"\n\n"+
			`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`+"\n\n"+
			"data: [DONE]\n\n")
	assertLines(t, got,
		"start:c1/up-m", "text:你", "text:好",
		"tool:0:call_1:ls", "args:0:{\"a\":1}",
		"usage:4/5", "stop:tool_calls")
}

func TestMsgsDecoderEventTable(t *testing.T) {
	d := newMsgsDecoder()
	got := runDecoder(t, d,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"up-m\"}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"ls\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	assertLines(t, got,
		"start/up-m", "tool:0:t1:ls",
		"args:0:{\"a\":", "args:0:1}",
		"stop:tool_calls")
}

func TestRespDecoderEventTable(t *testing.T) {
	d := newRespDecoder()
	got := runDecoder(t, d,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_0\",\"delta\":\"o\"}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_0\",\"delta\":\"k\"}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_9\",\"name\":\"ls\",\"arguments\":\"{}\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n")
	assertLines(t, got,
		"text:o", "text:k",
		"tool:0:call_9:ls", "args:0:{}", "tooldone:0",
		"stop:tool_calls", "usage:7/3")
}

func TestGeminiDecoderEventTable(t *testing.T) {
	d := newGeminiDecoder()
	got := runDecoder(t, d,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`+"\n\n"+
			`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"ls","args":{"path":"."}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`+"\n\n")
	assertLines(t, got,
		"text:ok",
		"tool:0:gemini-ls:ls", "args:0:{\"path\":\".\"}",
		"stop:tool_calls", "usage:3/2")
}

// ---------- encoder 对照表 ----------

// testSink 是 sseOut 的测试输出端（聚合全部写出）。
type testSink struct{ b strings.Builder }

func (s *testSink) Write(p []byte) (int, error) { return s.b.Write(p) }

// runEncoder 以事件驱动某协议 encoder，返回聚合输出。
func runEncoder(t *testing.T, p ClientProto, events ...gwEvent) string {
	t.Helper()
	sink := &testSink{}
	e := newEventEncoder(p, sink, nil)
	for _, ev := range events {
		e.Encode(ev)
	}
	e.Finish()
	return sink.b.String()
}

func TestChatEncoderEventTable(t *testing.T) {
	out := runEncoder(t, ProtoChat,
		gwEvent{kind: evStart, msgID: "c1", model: "up-m"},
		gwEvent{kind: evText, text: "hi"},
		gwEvent{kind: evToolStart, idx: 0, id: "call_1", name: "ls"},
		gwEvent{kind: evToolArgs, idx: 0, text: `{"a":1}`},
		gwEvent{kind: evToolDone, idx: 0},
		gwEvent{kind: evStop, stop: "tool_calls"},
		gwEvent{kind: evUsage, usage: gwUsage{prompt: 4, completion: 5}},
	)
	for _, want := range []string{
		`"role":"assistant"`, `"content":"hi"`, `"name":"ls"`, `"arguments":"{\"a\":1}"`,
		`"finish_reason":"tool_calls"`, `"prompt_tokens":4`, "data: [DONE]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("chat encoder 输出缺 %q:\n%s", want, out)
		}
	}
}

func TestMsgsEncoderEventTable(t *testing.T) {
	out := runEncoder(t, ProtoMessages,
		gwEvent{kind: evStart, model: "up-m"},
		gwEvent{kind: evText, text: "ok"},
		gwEvent{kind: evToolStart, idx: 0, id: "t1", name: "ls"},
		gwEvent{kind: evToolArgs, idx: 0, text: `{"a":1}`},
		gwEvent{kind: evStop, stop: "tool_calls"},
	)
	for _, want := range []string{
		"event: message_start", `"model":"up-m"`,
		"event: content_block_start", `"type":"tool_use"`, `"id":"t1"`, `"name":"ls"`,
		"event: content_block_delta", `"input_json_delta"`, `"partial_json":"{\"a\":1}"`,
		"event: message_delta", `"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("msgs encoder 输出缺 %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "event: message_start") {
		t.Fatalf("首个事件应为 message_start:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: {\"type\":\"message_stop\"}") {
		t.Fatalf("末个事件应为 message_stop:\n%s", out)
	}
}

func TestRespEncoderEventTable(t *testing.T) {
	out := runEncoder(t, ProtoResponses,
		gwEvent{kind: evStart, msgID: "resp_9"},
		gwEvent{kind: evText, text: "ok"},
		gwEvent{kind: evToolStart, idx: 0, id: "call_9", name: "ls"},
		gwEvent{kind: evToolArgs, idx: 0, text: `{"a":1}`},
		gwEvent{kind: evToolDone, idx: 0},
		gwEvent{kind: evStop, stop: "tool_calls"},
		gwEvent{kind: evUsage, usage: gwUsage{prompt: 4, completion: 5}},
	)
	for _, want := range []string{
		"event: response.output_item.added", "event: response.output_text.delta",
		"event: response.function_call_arguments.delta", `"delta":"{\"a\":1}"`,
		"event: response.function_call_arguments.done", `"arguments":"{\"a\":1}"`,
		"event: response.output_item.done", `"call_id":"call_9"`, `"name":"ls"`,
		"event: response.completed", `"id":"resp_9"`, `"input_tokens":4`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("resp encoder 输出缺 %q:\n%s", want, out)
		}
	}
}

func TestGeminiEncoderEventTable(t *testing.T) {
	out := runEncoder(t, ProtoGemini,
		gwEvent{kind: evStart},
		gwEvent{kind: evText, text: "ok"},
		gwEvent{kind: evToolStart, idx: 0, name: "ls"},
		gwEvent{kind: evToolArgs, idx: 0, text: `{"path":"."}`},
		gwEvent{kind: evToolDone, idx: 0},
		gwEvent{kind: evStop, stop: "tool_calls"},
		gwEvent{kind: evUsage, usage: gwUsage{prompt: 3, completion: 2}},
	)
	for _, want := range []string{
		`"text":"ok"`, `"functionCall"`, `"name":"ls"`, `"path":"."`,
		`"finishReason":"STOP"`, `"promptTokenCount":3`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("gemini encoder 输出缺 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("gemini encoder 不应输出 [DONE]:\n%s", out)
	}
}

// 名字先到、流戛然而止：Finish 补空参 part（防丢调用）。
func TestGeminiEncoderFlushesToolWithoutArgs(t *testing.T) {
	out := runEncoder(t, ProtoGemini,
		gwEvent{kind: evToolStart, idx: 0, id: "call_1", name: "ls"},
		gwEvent{kind: evStop, stop: "stop"},
	)
	if !strings.Contains(out, `"name":"ls"`) || !strings.Contains(out, `"functionCall"`) {
		t.Fatalf("无参工具未补发 functionCall part:\n%s", out)
	}
}
