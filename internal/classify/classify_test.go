package classify

import (
	"strings"
	"testing"
)

func TestUserStrategyKeywordHitCaseInsensitive(t *testing.T) {
	s := &UserStrategy{Intents: []IntentDef{{
		ID: "team/research", Label: "科研调研", Keywords: []string{"文献综述", "Paper"},
	}}}
	v, ok := s.EvaluateL0("帮我整理一下 PAPER 阅读清单")
	if !ok || v.Intent != "team/research" {
		t.Fatalf("got (%+v, %v), want team/research hit", v, ok)
	}
}

func TestUserStrategyPriorityWins(t *testing.T) {
	s := &UserStrategy{Intents: []IntentDef{
		{ID: "low", Keywords: []string{"周报"}, Priority: 1},
		{ID: "high", Keywords: []string{"周报", "汇总"}, Priority: 9},
	}}
	v, ok := s.EvaluateL0("把周报汇总一下")
	if !ok || v.Intent != "high" {
		t.Fatalf("got (%+v, %v), want priority=high", v, ok)
	}
}

func TestUserStrategyRegexMatchesOriginalCase(t *testing.T) {
	s := &UserStrategy{Intents: []IntentDef{{
		ID: "team/weekly", Regexes: []string{`周报\s*\d{4}-\d{2}-\d{2}`},
	}}}
	v, ok := s.EvaluateL0("提交 周报 2026-10-29 的草稿")
	if !ok || v.Intent != "team/weekly" {
		t.Fatalf("got (%+v, %v), want team/weekly", v, ok)
	}
	if _, ok := s.EvaluateL0("随便聊聊"); ok {
		t.Fatal("unrelated message must not hit")
	}
}

func TestUserStrategyInvalidRegexIgnoredNotBlocking(t *testing.T) {
	// §4.3/F8：无效正则只忽略该条，同意图其余特征仍生效。
	s := &UserStrategy{Intents: []IntentDef{
		{
			ID:       "team/weekly",
			Regexes:  []string{"周报(["}, // 非法正则
			Keywords: []string{"周报"},
		},
	}}
	v, ok := s.EvaluateL0("写周报")
	if !ok || v.Intent != "team/weekly" {
		t.Fatalf("got (%+v, %v), want keyword fallback still hits", v, ok)
	}
}

func TestUserStrategyNilSafeAndNoIntents(t *testing.T) {
	var s *UserStrategy
	if _, ok := s.EvaluateL0(" anything "); ok {
		t.Fatal("nil strategy must not hit")
	}
	if _, ok := (&UserStrategy{}).EvaluateL0(" anything "); ok {
		t.Fatal("empty strategy must not hit")
	}
}

func TestBuildL1PromptContainsBuiltinCustomAndExamples(t *testing.T) {
	custom := []IntentDef{{
		ID: "team/research", Label: "科研调研",
		Examples: []string{"调研一下 RAG 方案", "总结这批论文"},
	}}
	p := BuildL1Prompt([]string{"coding", "qa"}, custom, "调研 RAG")
	for _, want := range []string{"coding", "- team/research（科研调研）", "例：调研一下 RAG 方案", "调研 RAG"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestBuildL1PromptCapsExamplesAtFive(t *testing.T) {
	def := IntentDef{ID: "x", Label: "X"}
	for i := 0; i < 9; i++ {
		def.Examples = append(def.Examples, "示例句")
	}
	p := BuildL1Prompt(nil, []IntentDef{def}, "m")
	if got := strings.Count(p, "例："); got != maxExamplesPerIntent {
		t.Fatalf("example lines = %d, want %d", got, maxExamplesPerIntent)
	}
}
