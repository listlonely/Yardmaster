// Agent 专属模型链测试（2026-11-12）：cfgDispatch PUT 联动落地保留前缀
// 组 agent-<id>（members=链、order）——建/改/删全周期 + 成员存在性
// fail-closed + Planner 热替换生效；Match.Intents 多意图命中与单值兼容。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// newModelsServer 起带 providers 快照与派发引擎的测试网关（dispatch 可
// 选预置 seed）。
func newModelsServer(t *testing.T, dispatchSeed string) (*Server, *httptest.Server) {
	t.Helper()
	appDir := t.TempDir()
	seed := store.ProvidersFile{Providers: []provider.Provider{{
		ID: "prov", Name: "T", Protocol: provider.ProtocolChat, BaseURL: "https://up.test",
		Models:   []provider.Model{{ID: "m1"}, {ID: "m2"}},
		Accounts: []provider.Account{{Name: "main", APIKey: "k"}},
	}}}
	provPath := filepath.Join(appDir, "providers.json")
	if err := store.SaveProvidersFile(provPath, seed); err != nil {
		t.Fatal(err)
	}
	s := &Server{Planner: NewPlanner(seed.Providers, nil), Version: "test", AppDir: appDir,
		ProvidersPath: provPath}
	if dispatchSeed != "" {
		if err := os.WriteFile(filepath.Join(appDir, "dispatch.json"), []byte(dispatchSeed), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	de, derr := dispatch.LoadEngine(filepath.Join(appDir, "dispatch.json"))
	if derr != nil {
		t.Fatal(derr)
	}
	s.AttachDispatch(de)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

// TestDispatchModelsSyncGroup：链 → agent-<id> 组建/改/删全周期 + Planner 热生效。
func TestDispatchModelsSyncGroup(t *testing.T) {
	s, srv := newModelsServer(t, `{"agents":[{"id":"cc","command":["x"]}]}`)

	put := func(body string) (int, []byte) {
		return doPut(t, srv.URL, "/api/config/dispatch", body)
	}
	// 建：双成员链
	if code, body := put(`{"agents":[{"id":"cc","command":["x"],"models":["prov/m1","prov/m2"]}]}`); code != http.StatusOK {
		t.Fatalf("建链 PUT: HTTP %d: %s", code, body)
	}
	assertGroupMembers(t, s, []string{"prov/m1", "prov/m2"})
	// Planner 热生效：Plan("group/agent-cc") 可解析出候选
	cands, err := s.Planner.Plan("group/agent-cc", "sticky-x")
	if err != nil || len(cands) == 0 {
		t.Fatalf("热替换后组不可解析: err=%v cands=%d", err, len(cands))
	}
	// 改：单成员链
	if code, _ := put(`{"agents":[{"id":"cc","command":["x"],"models":["prov/m2"]}]}`); code != http.StatusOK {
		t.Fatalf("改链 PUT: HTTP %d", code)
	}
	assertGroupMembers(t, s, []string{"prov/m2"})
	// 删：清链 → 组消失
	if code, _ := put(`{"agents":[{"id":"cc","command":["x"]}]}`); code != http.StatusOK {
		t.Fatalf("清链 PUT: HTTP %d", code)
	}
	assertGroupMembers(t, s, nil)
}

// TestDispatchModelsUnknownMember：成员不存在 → 400 且 providers.json 组未变。
func TestDispatchModelsUnknownMember(t *testing.T) {
	s, srv := newModelsServer(t, `{"agents":[]}`)
	if code, body := doPut(t, srv.URL, "/api/config/dispatch",
		`{"agents":[{"id":"cc","command":["x"],"models":["prov/m1","prov/nope"]}]}`); code != http.StatusBadRequest {
		t.Fatalf("未知成员应 400: HTTP %d: %s", code, body)
	}
	assertGroupMembers(t, s, nil) // 未产生组
}

// TestDispatchModelsIntentsMatch：Match.Intents 多意图任一命中；单值 Intent 并存兼容。
func TestDispatchModelsIntentsMatch(t *testing.T) {
	s, _ := newModelsServer(t,
		`{"agents":[{"id":"cc","command":["x"],"match":{"intents":["coding","qa"]}},{"id":"wr","command":["y"],"match":{"intent":"writing"}}]}`)
	for _, tc := range []struct{ intent, want string }{
		{"coding", "cc"}, {"qa", "cc"}, {"writing", "wr"},
	} {
		a, err := s.Dispatch.Select("", "任务", tc.intent, "")
		if err != nil || a.ID != tc.want {
			t.Fatalf("intent=%s 应命中 %s: got=%v err=%v", tc.intent, tc.want, a.ID, err)
		}
	}
	// 非侧重意图不命中（无 default 兜底 → fail-closed 报错）
	if _, err := s.Dispatch.Select("", "任务", "vision", ""); err == nil {
		t.Fatal("vision 不在任何 Agent 侧重内，应 fail-closed 报错")
	}
	// Runtime：链非空 → AgentModel 填专属组 id（runner env 注入依据）
	spec, rerr := dispatch.AgentSpec{ID: "cc", Command: []string{"x"}, Models: []string{"prov/m1"}}.Runtime()
	if rerr != nil || spec.AgentModel != "agent-cc" {
		t.Fatalf("Runtime 应填 AgentModel=agent-cc: got=%q err=%v", spec.AgentModel, rerr)
	}
	spec2, _ := dispatch.AgentSpec{ID: "cc", Command: []string{"x"}}.Runtime()
	if spec2.AgentModel != "" {
		t.Fatalf("无链时 AgentModel 应为空: %q", spec2.AgentModel)
	}
}

// assertGroupMembers 断言 providers.json 里 agent-cc 组的成员（nil=组应不存在）。
// 组 id 恒为 agent-cc（本文件所有用例只有该 Agent 配链），不设参数（unparam）。
func assertGroupMembers(t *testing.T, s *Server, want []string) {
	t.Helper()
	const groupID = "agent-cc"
	var pf store.ProvidersFile
	if err := store.LoadJSON(s.ProvidersPath, &pf); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range pf.Groups {
		if g.ID == groupID {
			got = g.Members
		}
	}
	if want == nil {
		if got != nil {
			t.Fatalf("组 %s 应已删除，实际 %v", groupID, got)
		}
		return
	}
	if len(got) != len(want) {
		t.Fatalf("组 %s 成员应 %v，实际 %v", groupID, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("组 %s 成员[%d] 应 %s，实际 %s", groupID, i, want[i], got[i])
		}
	}
}
