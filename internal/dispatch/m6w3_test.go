// M6-W3 事件级实时测试：解析器词表（W1 核实形状）、流式切分等价、
// 留存护栏、LiveBuffer/Session 接入、引擎全链路。假 Agent 复用
// runner_test 的 TestHelperProcess（jsonl 行为）。
package dispatch

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// w3Sample 覆盖 W1 词表全部分支 + 未知类型 + 非 JSON（读不懂=未知 → raw）。
const w3Sample = `{"type":"system","subtype":"init","session_id":"s-1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"开始处理"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -la"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"file.txt"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"完成"}]}}
{"type":"result","subtype":"success","result":"答案是 42"}
这行不是 JSON
{"type":"mystery","x":1}
`

func kindsOf(evs []Event) []EventKind {
	out := make([]EventKind, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}

// 词表归类 + 关键字段 + raw 原样保留（解析失败不丢内容）。
func TestEventParserVocabulary(t *testing.T) {
	evs := ParseEvents(w3Sample)
	wantKinds := []EventKind{
		EventInit, EventText, EventTool, EventToolRes, EventText, EventResult,
		EventRaw, EventRaw,
	}
	if got := kindsOf(evs); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("事件序列不符: %v", got)
	}
	if evs[0].Subtype != "init" {
		t.Fatalf("init 子类型: %+v", evs[0])
	}
	if evs[2].Tool != "Bash" || !strings.Contains(evs[2].Text, `"command":"ls -la"`) {
		t.Fatalf("工具事件: %+v", evs[2])
	}
	if evs[3].Text != "file.txt" {
		t.Fatalf("工具回传: %+v", evs[3])
	}
	last := evs[5]
	if !last.Final || last.Subtype != "success" || last.Text != "答案是 42" {
		t.Fatalf("终态事件: %+v", last)
	}
	if evs[6].Text != "这行不是 JSON" || evs[7].Text != `{"type":"mystery","x":1}` {
		t.Fatalf("raw 事件未原样保留: %q / %q", evs[6].Text, evs[7].Text)
	}
}

// 逐字节喂入与整段喂入等价（流式切分正确性）；半行经 Flush 恰好收口一次。
func TestEventParserByteByByte(t *testing.T) {
	whole := ParseEvents(w3Sample)

	p := NewEventParser()
	var streamed []Event
	b := []byte(w3Sample)
	for i := 0; i < len(b); i++ {
		streamed = append(streamed, p.Feed(b[i:i+1])...)
	}
	if got := p.Flush(); len(got) != 0 {
		t.Fatalf("完整流 Flush 应为空: %v", got)
	}
	if !reflect.DeepEqual(kindsOf(streamed), kindsOf(whole)) {
		t.Fatalf("逐字节流事件序列不符: %v", kindsOf(streamed))
	}
	if !reflect.DeepEqual(streamed, whole) {
		t.Fatalf("逐字节流事件内容不符")
	}

	// 半行：无尾换行的最后一段由 Flush 收口，恰一次；Flush 后无新喂入
	// 再冲为空（幂等）；Feed 仅在运行期、Flush 仅在终态——两者不交错。
	p2 := NewEventParser()
	p2.Feed([]byte(`{"type":"result","subtype":"success"`))
	mid := p2.Flush()
	if len(mid) != 1 || mid[0].Kind != EventRaw || !strings.Contains(mid[0].Text, `"subtype":"success"`) {
		t.Fatalf("半行收口: %+v", mid)
	}
	if again := p2.Flush(); len(again) != 0 {
		t.Fatalf("Flush 应幂等: %v", again)
	}
}

// 留存护栏：超出 eventsKeep 丢最旧、保最新。
func TestEventParserBounded(t *testing.T) {
	var sb strings.Builder
	const total = eventsKeep + 88
	for i := 0; i < total; i++ {
		sb.WriteString("junk-")
		sb.WriteString(strings.Repeat("x", 3))
		sb.WriteString("\n")
	}
	evs := ParseEvents(sb.String())
	if len(evs) != eventsKeep {
		t.Fatalf("事件条数应封顶 %d: %d", eventsKeep, len(evs))
	}
	lastText := evs[len(evs)-1].Text
	wantLast := "junk-xxx" // 全部同形；只验条数护栏与最新在列
	if lastText != wantLast {
		t.Fatalf("最新事件应保留: %q", lastText)
	}
}

// LiveBuffer 接入：jsonl 模式留存事件且尾部照旧；text 模式恒 nil（不猜形状）。
func TestLiveBufferEvents(t *testing.T) {
	lb := NewLiveBuffer(nil, "jsonl")
	if _, err := lb.Write([]byte(w3Sample)); err != nil {
		t.Fatal(err)
	}
	lb.Flush()
	evs := lb.Events()
	if len(evs) != 8 {
		t.Fatalf("事件条数: %d", len(evs))
	}
	if !strings.Contains(lb.Tail(), "答案是 42") {
		t.Fatalf("尾部留存受损: %q", lb.Tail())
	}
	if text := NewLiveBuffer(nil, "text"); text.Events() != nil {
		t.Fatal("text 模式不应产生事件")
	}
	if empty := NewLiveBuffer(nil, ""); empty.Events() != nil {
		t.Fatal("缺省模式不应产生事件")
	}
}

// Session.Events 恢复兜底：无 live 时从留存尾部现场解析（仅 jsonl 模式）。
func TestSessionEventsRestoredFallback(t *testing.T) {
	restored := Session{OutputMode: "jsonl", Result: &Result{Output: w3Sample}}
	if got := len(restored.Events()); got != 8 {
		t.Fatalf("恢复会话事件条数: %d", got)
	}
	textMode := Session{OutputMode: "text", Result: &Result{Output: w3Sample}}
	if textMode.Events() != nil {
		t.Fatal("text 模式恢复会话不应有事件")
	}
	noResult := Session{OutputMode: "jsonl"}
	if noResult.Events() != nil {
		t.Fatal("无结果会话不应有事件")
	}
}

// 引擎全链路：jsonl agent 会话记录 OutputMode 并产出事件（含半行 raw 收口，
// 证明终态 Flush 生效）；text agent 不产出。
func TestEngineJSONLEndToEnd(t *testing.T) {
	spec := helperAgent(t, "jsonl")
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{
		{ID: "fake", Command: spec.Command, Env: spec.Env, Output: "jsonl"},
	}})
	sess, err := e.Submit(context.Background(), e.Agents()[0], "任务", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.Wait()
	snap, ok := e.Snapshot(sess.ID)
	if !ok || snap.Status != "done" {
		t.Fatalf("会话状态: %+v ok=%v", snap, ok)
	}
	if snap.OutputMode != "jsonl" {
		t.Fatalf("OutputMode 未记录: %q", snap.OutputMode)
	}
	evs := snap.Events()
	if len(evs) == 0 {
		t.Fatal("jsonl 会话应产出事件")
	}
	final := evs[len(evs)-1]
	if final.Kind != EventRaw || !strings.Contains(final.Text, "半行收口") {
		t.Fatalf("末事件应为半行 raw（Flush 收口）: %+v", final)
	}
	var sawResult bool
	for _, ev := range evs {
		if ev.Kind == EventResult && ev.Subtype == "success" && ev.Text == "答案是 42" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("终态 result 事件缺失: %+v", evs)
	}

	// text 模式同输出：无事件。
	e2 := mustSpec(t, DispatchFile{Agents: []AgentSpec{
		{ID: "fake", Command: spec.Command, Env: spec.Env},
	}})
	sess2, err := e2.Submit(context.Background(), e2.Agents()[0], "任务", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	sess2.Wait()
	snap2, _ := e2.Snapshot(sess2.ID)
	if snap2.Events() != nil {
		t.Fatal("text 会话不应产出事件")
	}
}
