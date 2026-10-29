// W6 决策追溯测试：TraceLog 落盘/读取/清理/内存缓冲、usage 提取
// （流式与非流式）、当日用量累计与落盘。
package gateway

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agenthub/internal/provider"
	"agenthub/internal/store"
)

func newTestTraceLog(t *testing.T) *TraceLog {
	t.Helper()
	tl := NewTraceLog(filepath.Join(t.TempDir(), "trace"))
	t.Cleanup(tl.Close)
	return tl
}

func TestTraceLogRecordAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trace")
	tl := NewTraceLog(dir)
	tl.Record(Trace{ID: "t-1", Time: "2026-10-29T10:00:00+08:00", Caller: "gw-cline",
		RequestModel: "group/main", RoutePath: "request", Target: "group/main",
		Outcome: "ok", Served: "prov/one", ServedModel: "m", TTFBMS: 120,
		Candidates: []TraceCandidate{
			{Provider: "prov", Account: "one", Model: "m", Result: CandOK, DurMS: 118},
		}})
	tl.Record(Trace{ID: "t-2", Outcome: "fail", HTTPStatus: 502,
		Candidates: []TraceCandidate{
			{Provider: "prov", Account: "one", Result: CandServer, Status: 500, Err: "boom"},
			{Provider: "prov", Account: "two", Result: CandServer, Status: 503, Err: "down"},
		}})
	tl.Close() // 冲刷

	loaded, err := LoadTraceFiles(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("加载 %d 条, want 2", len(loaded))
	}
	// LoadTraceFiles 从新到旧：后写入的在前
	if loaded[0].ID != "t-2" || loaded[1].ID != "t-1" {
		t.Fatalf("顺序错误: %s, %s", loaded[0].ID, loaded[1].ID)
	}
	if len(loaded[1].Candidates) != 1 || loaded[1].Candidates[0].Result != CandOK {
		t.Fatalf("候选链错误: %+v", loaded[1].Candidates)
	}
	if len(loaded[0].Candidates) != 2 || loaded[0].Candidates[0].Err != "boom" {
		t.Fatalf("失败链错误: %+v", loaded[0].Candidates)
	}
	// maxLines 截断
	few, err := LoadTraceFiles(dir, 1)
	if err != nil || len(few) != 1 {
		t.Fatalf("maxLines=1 应返回 1 条: %d %v", len(few), err)
	}
	// 单行损坏不中断
	path := filepath.Join(dir, "trace-"+time.Now().Format("20060102")+".jsonl")
	if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := LoadTraceFiles(dir, 0)
	if err != nil || len(again) != 0 {
		t.Fatalf("损坏文件应被跳过: %d %v", len(again), err)
	}
}

func TestTraceLogRecentFilters(t *testing.T) {
	tl := newTestTraceLog(t)
	tl.Record(Trace{ID: "a", Caller: "gw-cline", Outcome: "ok"})
	tl.Record(Trace{ID: "b", Caller: "gw-codex", Outcome: "fail"})
	tl.Record(Trace{ID: "c", Caller: "gw-cline", Outcome: "ok", Switched: true})
	if got := tl.Recent(0, "", ""); len(got) != 3 {
		t.Fatalf("全部 = %d", len(got))
	}
	if got := tl.Recent(0, "gw-cline", ""); len(got) != 2 || got[0].ID != "c" {
		t.Fatalf("agent 过滤 = %+v", got)
	}
	if got := tl.Recent(0, "", "fail"); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("fail 过滤 = %+v", got)
	}
	if got := tl.Recent(0, "", "switch"); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("switch 过滤 = %+v", got)
	}
	if got := tl.Recent(1, "", ""); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("limit 应取最新 = %+v", got)
	}
}

func TestTraceLogCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	mk := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("trace-20260101.jsonl", strings.Repeat("x", 300)) // 30 天前 → 清
	mk("trace-20261028.jsonl", strings.Repeat("y", 300)) // 昨天 → 留
	mk("other.jsonl", "zzz")                             // 非 trace 文件 → 不动
	tl := &TraceLog{dir: dir, now: func() time.Time { return now }, keepDays: 30, maxBytes: 500}
	tl.cleanup()
	for _, name := range []string{"trace-20261028.jsonl", "other.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 不应被清理: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "trace-20260101.jsonl")); !os.IsNotExist(err) {
		t.Fatal("超过保留期的 trace 应被删除")
	}
	// 超量：maxBytes=100 时 300 字节文件应被删（从旧到新）
	tl2 := &TraceLog{dir: dir, now: func() time.Time { return now }, keepDays: 30, maxBytes: 100}
	tl2.cleanup()
	if _, err := os.Stat(filepath.Join(dir, "trace-20261028.jsonl")); !os.IsNotExist(err) {
		t.Fatal("超总量的最旧 trace 应被删除")
	}
}

func TestUsageScanner(t *testing.T) {
	// chat 非流式
	sc := &usageScanner{}
	_, _ = sc.Write([]byte(`{"choices":[...],"usage":{"prompt_tokens":120,"completion_tokens":45}}`))
	if u := sc.usageOf(); u == nil || u.Prompt != 120 || u.Completion != 45 {
		t.Fatalf("chat usage = %+v", sc.usageOf())
	}
	// messages 字段名
	sc2 := &usageScanner{}
	_, _ = sc2.Write([]byte(`{"usage":{"input_tokens":7,"output_tokens":9}}`))
	if u := sc2.usageOf(); u == nil || u.Prompt != 7 || u.Completion != 9 {
		t.Fatalf("messages usage = %+v", sc2.usageOf())
	}
	// 流式多 chunk：取最后一次出现
	sc3 := &usageScanner{}
	for _, chunk := range []string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		_, _ = sc3.Write([]byte(chunk))
	}
	if u := sc3.usageOf(); u != nil {
		t.Fatalf("无 usage 流不应产出: %+v", u)
	}
	sc4 := &usageScanner{}
	_, _ = sc4.Write([]byte(`data: {"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n"))
	_, _ = sc4.Write([]byte(`data: {"usage":{"prompt_tokens":10,"completion_tokens":20}}` + "\n\n"))
	if u := sc4.usageOf(); u == nil || u.Prompt != 10 || u.Completion != 20 {
		t.Fatalf("流式 usage 应取最后值: %+v", sc4.usageOf())
	}
	// 无用量返回 nil
	sc5 := &usageScanner{}
	_, _ = sc5.Write([]byte("hello"))
	if sc5.usageOf() != nil {
		t.Fatal("无用量应返回 nil")
	}
}

// TestTraceEndToEndFailoverChain 端到端：候选切换链、Served、Switched、
// usage 提取全部落进 trace。
func TestTraceEndToEndFailoverChain(t *testing.T) {
	upA := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 500, "application/json", `{"error":{"message":"down"}}`
	})
	upB := newFakeUpstream(t, func(rec recordedReq, n int) (int, string, string) {
		return 200, "application/json", `{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`
	})
	pa := chatProvider(upA.srv.URL)
	pa.ID = "prov-a"
	pa.Accounts = pa.Accounts[:1]
	pb := chatProvider(upB.srv.URL)
	pb.ID = "prov-b"
	pb.Accounts = pb.Accounts[:1]
	g := provider.Group{ID: "main", Members: []string{"prov-a/m", "prov-b/m"}}

	tl := newTestTraceLog(t)
	p := NewPlanner([]provider.Provider{pa, pb}, []provider.Group{g})
	s := &Server{Planner: p, Version: "test", Traces: tl}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)

	resp := post(t, gw.URL, "/v1/chat/completions", chatReqBody("group/main", false))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	traces := tl.Recent(1, "", "")
	if len(traces) != 1 {
		t.Fatalf("应有 1 条 trace: %d", len(traces))
	}
	tr := traces[0]
	if tr.Outcome != "ok" || tr.Served != "prov-b/one" || tr.ServedModel != "m" {
		t.Fatalf("终态错误: %+v", tr)
	}
	if !tr.Switched {
		t.Fatal("候选切换应标记 Switched")
	}
	if len(tr.Candidates) != 2 ||
		tr.Candidates[0].Result != CandServer || tr.Candidates[0].Status != 500 ||
		tr.Candidates[1].Result != CandOK {
		t.Fatalf("尝试链错误: %+v", tr.Candidates)
	}
	if tr.RoutePath != "request" || tr.Target != "group/main" {
		t.Fatalf("路由路径错误: %+v", tr)
	}
	if tr.Usage == nil || tr.Usage.Prompt != 11 || tr.Usage.Completion != 5 {
		t.Fatalf("usage 未提取: %+v", tr.Usage)
	}
	if tr.TTFBMS <= 0 || tr.TotalMS <= 0 {
		t.Fatalf("耗时未记录: ttfb=%d total=%d", tr.TTFBMS, tr.TotalMS)
	}
	// 用量累计进账号（smart 数据源）
	provB, _, err := p.ResolveModel("prov-b/m")
	if err != nil {
		t.Fatal(err)
	}
	if got := provB.Accounts[0].UsedTokens; got != 16 {
		t.Fatalf("UsedTokens = %d, want 16", got)
	}
}

// TestNoteUsageDayReset 跨日重置：日期变化后累计清零重新开始。
func TestNoteUsageDayReset(t *testing.T) {
	provs := []provider.Provider{chatProvider("http://x")}
	p := NewPlanner(provs, nil)
	day := time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local)
	clock := day
	p.SetNow(func() time.Time { return clock })

	prov, _, _ := p.ResolveModel("prov/up-m")
	cands := p.Candidates(prov, "up-m")
	p.NoteUsage(cands[0], 100)
	if prov.Accounts[0].UsedTokens != 100 {
		t.Fatalf("累计 = %d", prov.Accounts[0].UsedTokens)
	}
	p.NoteUsage(cands[0], 50)
	if prov.Accounts[0].UsedTokens != 150 {
		t.Fatalf("累计 = %d", prov.Accounts[0].UsedTokens)
	}
	// 跨日
	clock = day.Add(24 * time.Hour)
	p.NoteUsage(cands[0], 30)
	if prov.Accounts[0].UsedTokens != 30 || prov.Accounts[0].UsageDay != "2026-10-30" {
		t.Fatalf("跨日应重置: used=%d day=%s", prov.Accounts[0].UsedTokens, prov.Accounts[0].UsageDay)
	}
}

// TestFlushUsageMergesWithoutClobbering 落盘合并：只更新跟踪账号，
// 期间 CLI 的其他编辑（如加账号）不被覆盖。
func TestFlushUsageMergesWithoutClobbering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	provs := []provider.Provider{chatProvider("http://x")}
	p := NewPlanner(provs, nil)

	prov, _, _ := p.ResolveModel("prov/up-m")
	cands := p.Candidates(prov, "up-m")
	p.NoteUsage(cands[0], 77)

	// 模拟 CLI 在累计期间新增账号
	if _, err := store.LoadProvidersFile(path); err == nil {
		t.Fatal("初始应无文件")
	}
	merged := store.ProvidersFile{Providers: []provider.Provider{chatProvider("http://x")}}
	merged.Providers[0].Accounts = append(merged.Providers[0].Accounts,
		provider.Account{Name: "new", APIKey: "k", Status: provider.AccountAvailable})
	if err := store.SaveProvidersFile(path, merged); err != nil {
		t.Fatal(err)
	}

	if err := p.FlushUsage(path); err != nil {
		t.Fatal(err)
	}
	f, err := store.LoadProvidersFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Providers[0].Accounts) != 3 {
		t.Fatalf("CLI 新增账号不应被覆盖: %d", len(f.Providers[0].Accounts))
	}
	if f.Providers[0].Accounts[0].UsedTokens != 77 {
		t.Fatalf("UsedTokens 应落盘 77, got %d", f.Providers[0].Accounts[0].UsedTokens)
	}
	if f.Providers[0].Accounts[0].UsageDay == "" {
		t.Fatal("UsageDay 应落盘")
	}
	// 非 dirty 时 FlushUsage 不写
	before, _ := os.Stat(path)
	if err := p.FlushUsage(path); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("非 dirty 不应重写文件")
	}
	// NoteUsage(token=0) 不累计
	p2 := NewPlanner([]provider.Provider{chatProvider("http://y")}, nil)
	p2.NoteUsage(Candidate{}, 0)
	if err := p2.FlushUsage(filepath.Join(dir, "none.json")); err != nil {
		t.Fatalf("无 dirty 应为空操作: %v", err)
	}
}
