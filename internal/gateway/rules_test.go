// W5 规则引擎测试：条件求值、首条命中、hits 落盘与热重载、调用方
// key 提取、输入统计。
package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agenthub/internal/provider"
)

func writeRules(t *testing.T, json string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRuleEngineFirstMatchWins(t *testing.T) {
	path := writeRules(t, `{"rules":[
		{"id":"r2","intent":"qa","target":"prov/b"},
		{"id":"r1","intent":"qa","target":"prov/a"},
		{"id":"r3","target":"prov/c"}
	]}`)
	e := NewRuleEngine(path)
	// 首条命中：两个 intent=qa 规则中文件序在前的 r2 生效
	r, ok := e.Match("gw-x", "qa", "hello", 100)
	if !ok || r.ID != "r2" || r.Target != "prov/b" {
		t.Fatalf("got (%+v, %v), want r2", r, ok)
	}
	// 条件不满足继续向下：兜底规则
	r, ok = e.Match("gw-x", "coding", "hello", 100)
	if !ok || r.ID != "r3" {
		t.Fatalf("got (%+v, %v), want r3", r, ok)
	}
}

func TestRuleEngineConditions(t *testing.T) {
	path := writeRules(t, `{"rules":[
		{"id":"tok","max_tokens":100,"target":"prov/t"},
		{"id":"caller","caller_key":"gw-cline","target":"prov/c"},
		{"id":"re","regex":"周报\\s*\\d{4}","target":"prov/r"},
		{"id":"badre","regex":"([","target":"prov/x"},
		{"id":"notarget","intent":"qa"}
	]}`)
	e := NewRuleEngine(path)
	if r, ok := e.Match("", "", "hi", 50); !ok || r.ID != "tok" {
		t.Fatalf("token 条件（≤100）应命中: %+v %v", r, ok)
	}
	if _, ok := e.Match("", "", "hi", 150); ok {
		t.Fatal("token 超限不应命中 tok，也不应命中后续 caller 条件")
	}
	if r, ok := e.Match("gw-cline", "", "", 150); !ok || r.ID != "caller" {
		t.Fatalf("caller 条件应命中: %+v %v", r, ok)
	}
	if _, ok := e.Match("gw-other", "", "", 150); ok {
		t.Fatal("caller 不匹配不应命中")
	}
	if r, ok := e.Match("", "", "提交 周报 2026-10-29", 150); !ok || r.ID != "re" {
		t.Fatalf("正则条件应命中: %+v %v", r, ok)
	}
	if _, ok := e.Match("", "", "随便聊聊", 150); ok {
		t.Fatal("无效正则与无目标规则都不可命中")
	}
}

func TestRuleEngineHitsPersistAndHotReload(t *testing.T) {
	path := writeRules(t, `{"rules":[{"id":"r","target":"prov/a"}]}`)
	e := NewRuleEngine(path)
	if _, ok := e.Match("k", "", "", 0); !ok {
		t.Fatal("应命中")
	}
	snap := e.Snapshot()
	if len(snap) != 1 || snap[0]["hits"].(int64) != 1 {
		t.Fatalf("hits 应为 1: %+v", snap)
	}
	// 外部改文件（模拟 CLI 编辑）：mtime 变化触发重载，按 ID 保留 hits
	if err := os.WriteFile(path, []byte(`{"rules":[{"id":"r","target":"prov/b"},{"id":"n","target":"prov/n"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	snap = e.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("热重载后应有 2 条规则: %+v", snap)
	}
	if snap[0]["hits"].(int64) != 1 {
		t.Fatalf("重载应按 ID 保留 hits: %+v", snap[0])
	}
	if r, ok := e.Match("k", "", "", 0); !ok || r.Target != "prov/b" {
		t.Fatalf("重载后应命中新 target: %+v %v", r, ok)
	}
	// 落盘校验：hits 写穿
	f, err := LoadRulesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Rules[0].Hits != 2 {
		t.Fatalf("hits 应写穿为 2, got %d", f.Rules[0].Hits)
	}
}

func TestRuleEngineMissingFileIsEmpty(t *testing.T) {
	e := NewRuleEngine(filepath.Join(t.TempDir(), "none.json"))
	if _, ok := e.Match("k", "", "", 0); ok {
		t.Fatal("无规则文件不应命中")
	}
}

func TestCallerKeyOf(t *testing.T) {
	mk := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	if got := callerKeyOf(mk(map[string]string{"Authorization": "Bearer gw-cline"})); got != "gw-cline" {
		t.Fatalf("Bearer 提取 = %q", got)
	}
	if got := callerKeyOf(mk(map[string]string{"X-Api-Key": "gw-aider"})); got != "gw-aider" {
		t.Fatalf("x-api-key 提取 = %q", got)
	}
	if got := callerKeyOf(mk(nil)); got != "" {
		t.Fatalf("无凭证 = %q", got)
	}
	if got := callerKeyOf(mk(map[string]string{"Authorization": "bearer gw-x"})); got != "" {
		t.Fatalf("非 Bearer 前缀不提取（大小写敏感）: %q", got)
	}
}

func TestInputStats(t *testing.T) {
	cr := &ChatRequest{Messages: []ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "第一个问题"},
		{Role: "assistant", Content: "回答"},
		{Role: "user", Content: "帮我写个 go 脚本"},
	}}
	last, tokens := inputStats(cr)
	if last != "帮我写个 go 脚本" {
		t.Fatalf("lastUser = %q", last)
	}
	// runes: 3+5+2+10=20 → tokens=10
	if tokens != 10 {
		t.Fatalf("tokens = %d, want 10", tokens)
	}
	mr := &MessagesRequest{
		System: "you are helpful",
		Messages: []MessagesMessage{
			{Role: "user", Content: []any{map[string]any{"type": "text", "text": "块文本"}}},
		},
	}
	last, _ = inputStats(mr)
	if last != "块文本" {
		t.Fatalf("messages lastUser = %q", last)
	}
	if _, tokens = inputStats(&ChatRequest{}); tokens != 0 {
		t.Fatalf("空请求 tokens = %d", tokens)
	}
}

// TestRulesEndpointOverridesModel 端到端：规则命中改道 + X-Gw-Rule 头。
func TestRulesEndpointOverridesModel(t *testing.T) {
	upA := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if !strings.Contains(rec.body, `"model":"m-a"`) {
			t.Errorf("上游 A 收到模型名错误: %s", rec.body)
		}
		return 200, "application/json", `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from-a"},"finish_reason":"stop"}]}`
	})
	upB := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if !strings.Contains(rec.body, `"model":"m-b"`) {
			t.Errorf("上游 B 收到模型名错误: %s", rec.body)
		}
		return 200, "application/json", `{"id":"2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}]}`
	})
	rulesPath := writeRules(t, `{"rules":[{"id":"big-code","intent":"coding","target":"prov-b/m-b"}]}`)
	pa := chatProvider(upA.srv.URL)
	pa.ID = "prov-a"
	pb := chatProvider(upB.srv.URL)
	pb.ID = "prov-b"
	s := &Server{
		Planner: NewPlanner([]provider.Provider{pa, pb}, nil),
		Version: "test",
		Rules:   NewRuleEngine(rulesPath),
		Intents: NewIntentChain(filepath.Join(t.TempDir(), "none.json"), "", 0, nil),
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	// 消息含文件名特征 → 内置 L0 判为 coding → 规则命中改道
	body := `{"model":"prov-a/m-a","messages":[{"role":"user","content":"fix this bug in main.go"}]}`
	resp := post(t, gw.URL, "/v1/chat/completions", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Gw-Rule"); got != "big-code" {
		t.Fatalf("X-Gw-Rule = %q, want big-code", got)
	}
	if got := resp.Header.Get("X-Gw-Provider"); got != "prov-b" {
		t.Fatalf("规则应改道到 prov-b, got %q", got)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "from-b") {
		t.Fatalf("body = %s", b)
	}
}

// TestKeysDefaultModelBinding 端到端：model=default 时按调用方绑定路由。
func TestKeysDefaultModelBinding(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		if !strings.Contains(rec.body, `"model":"m-x"`) {
			t.Errorf("上游收到模型名错误: %s", rec.body)
		}
		return 200, "application/json", `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	})
	keysPath := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(keysPath, []byte(`{"bindings":[{"key":"gw-cline","default_model":"prov/m-x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Planner: NewPlanner([]provider.Provider{chatProvider(up.srv.URL)}, nil),
		Version: "test",
		Keys:    NewKeyStore(keysPath),
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	req, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer gw-cline")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, b)
	}
	if got := resp.Header.Get("X-Gw-Model"); got != "m-x" {
		t.Fatalf("绑定默认模型应生效, X-Gw-Model = %q", got)
	}

	// 无绑定调用方 + default → 404
	req2, _ := http.NewRequest("POST", gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("未绑定 key 的 default 应 404, got %d", resp2.StatusCode)
	}
}

// TestGroupCrossProviderFailover 端到端：组成员 A 全失败 → B 接管。
func TestGroupCrossProviderFailover(t *testing.T) {
	upA := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 500, "application/json", `{"error":{"message":"down"}}`
	})
	upB := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "application/json", `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from-b"},"finish_reason":"stop"}]}`
	})
	pa := chatProvider(upA.srv.URL)
	pa.ID = "prov-a"
	pa.Accounts = pa.Accounts[:1] // 只留 one
	pb := chatProvider(upB.srv.URL)
	pb.ID = "prov-b"
	g := provider.Group{ID: "main", Strategy: provider.StrategyOrder,
		Members: []string{"prov-a/m", "prov-b/m"}}
	s := &Server{Planner: NewPlanner([]provider.Provider{pa, pb}, []provider.Group{g}), Version: "test"}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("group/main", false))
	if resp.StatusCode != 200 {
		t.Fatalf("组内 failover 后应 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Gw-Provider"); got != "prov-b" {
		t.Fatalf("应切到 prov-b, got %q", got)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "from-b") {
		t.Fatalf("body = %s", b)
	}
	if len(upA.calls()) != 1 || len(upB.calls()) != 1 {
		t.Fatalf("A 打 1 次、B 打 1 次: %d / %d", len(upA.calls()), len(upB.calls()))
	}
}
