// 派发引擎测试（M5-W4）：fail-closed 校验、匹配顺序、并发闸、取消、
// 会话表、jsonl 提取。假 Agent 复用 runner_test 的 TestHelperProcess。
package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustSpec(t *testing.T, f DispatchFile) *Engine {
	t.Helper()
	if err := ValidateDispatch(f); err != nil {
		t.Fatalf("校验意外失败: %v", err)
	}
	return NewEngine(&f)
}

func TestValidateDispatchFailClosed(t *testing.T) {
	cases := []struct {
		name string
		f    DispatchFile
		want string
	}{
		{"空 id", DispatchFile{Agents: []AgentSpec{{ID: "", Command: []string{"a"}}}}, "id 不能为空"},
		{"id 重复", DispatchFile{Agents: []AgentSpec{{ID: "x", Command: []string{"a"}}, {ID: "x", Command: []string{"b"}}}}, "重复"},
		{"空 command", DispatchFile{Agents: []AgentSpec{{ID: "x"}}}, "command 不能为空"},
		{"非法超时", DispatchFile{Agents: []AgentSpec{{ID: "x", Command: []string{"a"}, Timeout: "abc"}}}, "timeout 非法"},
		{"非法 output", DispatchFile{Agents: []AgentSpec{{ID: "x", Command: []string{"a"}, Output: "xml"}}}, "output 仅支持"},
		{"双 default", DispatchFile{Agents: []AgentSpec{{ID: "a", Command: []string{"x"}, Default: true}, {ID: "b", Command: []string{"y"}, Default: true}}}, "至多一个"},
		{"非法正则", DispatchFile{Agents: []AgentSpec{{ID: "x", Command: []string{"a"}, Match: &Match{PromptRegex: "["}}}}, "正则非法"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDispatch(c.f)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应报 %q，got %v", c.want, err)
			}
		})
	}
}

func TestLoadEngineFailClosedOnBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.json")
	if err := os.WriteFile(path, []byte(`{"agents":[{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEngine(path); err == nil || !strings.Contains(err.Error(), "派发不可用") {
		t.Fatalf("损坏文件应 fail-closed: %v", err)
	}
	// 不存在文件 = 无 agent 引擎（非错误）
	e, err := LoadEngine(filepath.Join(dir, "none.json"))
	if err != nil || len(e.Agents()) != 0 {
		t.Fatalf("不存在文件应返回空引擎: %v", err)
	}
	if _, err := e.Select("any", "t", "", ""); err == nil {
		t.Fatal("空引擎显式指定应报错")
	}
}

func TestSelectOrderExplicitMatchDefault(t *testing.T) {
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{
		{ID: "coder", Command: []string{"a"}, Match: &Match{Intent: "coding"}},
		{ID: "writer", Command: []string{"b"}, Match: &Match{PromptRegex: `(?i)翻译`}},
		{ID: "fallback", Command: []string{"c"}, Default: true},
	}})
	if a, _ := e.Select("writer", "", "", ""); a.ID != "writer" {
		t.Fatalf("显式指定失效: %+v", a)
	}
	if a, _ := e.Select("", "写一个排序算法", "coding", ""); a.ID != "coder" {
		t.Fatalf("intent 匹配失效: %+v", a)
	}
	if a, _ := e.Select("", "帮我翻译这段话", "", ""); a.ID != "writer" {
		t.Fatalf("prompt 正则失效: %+v", a)
	}
	if a, _ := e.Select("", "随便什么", "", ""); a.ID != "fallback" {
		t.Fatalf("default 兜底失效: %+v", a)
	}
	if _, err := e.Select("nope", "", "", ""); err == nil {
		t.Fatal("未知 id 应报错")
	}
}

func TestSelectNoMatchNoDefaultFails(t *testing.T) {
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{{ID: "x", Command: []string{"a"}, Match: &Match{Intent: "coding"}}}})
	if _, err := e.Select("", "task", "", ""); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("无匹配无兜底应 fail-closed: %v", err)
	}
}

func TestSubmitRunsAndRecordsSession(t *testing.T) {
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{{ID: "fake", Command: helperAgent(t, "ok").Command, Env: helperAgent(t, "ok").Env}}})
	s, err := e.Submit(context.Background(), e.Agents()[0], "任务", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	sessions := e.Sessions()
	if len(sessions) != 1 || sessions[0].Status != "done" || sessions[0].Result == nil {
		t.Fatalf("会话状态异常: %+v", sessions)
	}
	if sessions[0].Result.Outcome != OutcomeOK || !strings.Contains(sessions[0].Result.Output, "42") {
		t.Fatalf("结果异常: %+v", sessions[0].Result)
	}
}

func TestSubmitConcurrencyCapRejects(t *testing.T) {
	spec := helperAgent(t, "slow")
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{{ID: "fake", Command: spec.Command, Env: spec.Env}}})
	s1, err := e.Submit(context.Background(), e.Agents()[0], "t1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel(s1.ID)
	if _, err := e.Submit(context.Background(), e.Agents()[0], "t2", "", nil); err == nil || !strings.Contains(err.Error(), "并发已满") {
		t.Fatalf("并发闸应拒绝: %v", err)
	}
	e.Cancel(s1.ID)
	s1.Wait()
	if s1.Status != "cancelled" {
		t.Fatalf("取消后状态异常: %s", s1.Status)
	}
	// 取消归还名额后可再次提交
	s3, err := e.Submit(context.Background(), e.Agents()[0], "t3", "", nil)
	if err != nil {
		t.Fatalf("名额未归还: %v", err)
	}
	e.Cancel(s3.ID)
	s3.Wait()
}

func TestSessionRingCap(t *testing.T) {
	spec := helperAgent(t, "ok")
	e := mustSpec(t, DispatchFile{Agents: []AgentSpec{{ID: "fake", Command: spec.Command, Env: spec.Env, MaxConcurrent: 8}}})
	e.now = func() time.Time { return time.Now() }
	for i := 0; i < SessionRingCap+5; i++ {
		s, err := e.Submit(context.Background(), e.Agents()[0], "t", "", nil)
		if err != nil {
			t.Fatalf("第 %d 次提交失败: %v", i, err)
		}
		s.Wait()
	}
	if got := len(e.Sessions()); got != SessionRingCap {
		t.Fatalf("环形留存应为 %d: %d", SessionRingCap, got)
	}
}

func TestExtractResultModes(t *testing.T) {
	jsonl := `{"type":"system","text":"init"}` + "\n" + `{"type":"result","result":"最终答案"}` + "\n"
	if got := ExtractResult(jsonl, "jsonl"); got != "最终答案" {
		t.Fatalf("jsonl 提取异常: %q", got)
	}
	mixed := "not json\n" + `{"text":"后半"}` + "\n"
	if got := ExtractResult(mixed, "jsonl"); got != "后半" {
		t.Fatalf("容错解析异常: %q", got)
	}
	// 全部不可解析 → 退回全文（读不懂=未知，不丢内容）
	if got := ExtractResult("plain", "jsonl"); got != "plain" {
		t.Fatalf("兜底应回退全文: %q", got)
	}
	if got := ExtractResult("plain", "text"); got != "plain" {
		t.Fatalf("text 模式应原样: %q", got)
	}
}

func TestSuggestionsShape(t *testing.T) {
	if err := ValidateDispatch(DispatchFile{Agents: Suggestions()}); err != nil {
		t.Fatalf("建议条目应过校验: %v", err)
	}
}
