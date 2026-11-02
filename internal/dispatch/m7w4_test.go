// M7-W4 派发解析插件单测：EventParser.Hook 优先、ok=false 行内回退
// 内置词表；LiveBuffer.UseParser 挂钩后 Write 即经 Hook（outputMode
// 非 jsonl 时 UseParser 会启用解析器）。
package dispatch

import (
	"strings"
	"testing"
)

func TestEventParserHook(t *testing.T) {
	p := NewEventParser()
	p.Hook = func(line string) ([]Event, bool) {
		if strings.Contains(line, "插件行") {
			return []Event{{Kind: EventText, Text: "插件解析"}}, true
		}
		return nil, false // 回退内置词表
	}
	evs := p.Feed([]byte("插件行\n"))
	if len(evs) != 1 || evs[0].Kind != EventText || evs[0].Text != "插件解析" {
		t.Fatalf("Hook 优先: %+v", evs)
	}
	// ok=false：该行走内置词表（未知 JSON → raw 回退，读不懂=未知）。
	evs = p.Feed([]byte("not json line\n"))
	if len(evs) != 1 || evs[0].Kind != EventRaw {
		t.Fatalf("回退内置: %+v", evs)
	}
	if len(p.Events()) != 2 {
		t.Fatalf("留存: %+v", p.Events())
	}
}

func TestLiveBufferUseParser(t *testing.T) {
	// 非 jsonl 模式构造：UseParser 应启用解析器并挂钩。
	lb := NewLiveBuffer(nil, "plain")
	lb.UseParser(func(line string) ([]Event, bool) {
		return []Event{{Kind: EventTool, Tool: "插件工具"}}, true
	})
	if _, err := lb.Write([]byte("任意行\n")); err != nil {
		t.Fatal(err)
	}
	evs := lb.Events()
	if len(evs) != 1 || evs[0].Kind != EventTool || evs[0].Tool != "插件工具" {
		t.Fatalf("UseParser 后 Write 事件: %+v", evs)
	}
	// 换回 ok=false：回退内置词表。
	lb.UseParser(func(line string) ([]Event, bool) { return nil, false })
	if _, err := lb.Write([]byte("plain text\n")); err != nil {
		t.Fatal(err)
	}
	evs = lb.Events()
	if len(evs) != 2 || evs[1].Kind != EventRaw {
		t.Fatalf("回退: %+v", evs)
	}
}
