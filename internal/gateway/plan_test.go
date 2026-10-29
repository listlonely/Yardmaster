// W5 计划器测试：三策略、会话粘性、组跨 provider 展开、裸 id 自动组。
package gateway

import (
	"testing"
	"time"

	"yardmaster/internal/provider"
)

// twoProviders 构造两个各含两账号的 chat provider（账号名全局唯一便于断言）。
func twoProviders() []provider.Provider {
	a := chatProvider("http://a") // prov/one, prov/two
	b := chatProvider("http://b") // prov/one, prov/two
	b.ID = "prov-b"
	a.Accounts[0].Name, a.Accounts[1].Name = "a1", "a2"
	b.Accounts[0].Name, b.Accounts[1].Name = "b1", "b2"
	return []provider.Provider{a, b}
}

func candKeys(cands []Candidate) []string {
	keys := make([]string, 0, len(cands))
	for _, c := range cands {
		keys = append(keys, c.Provider.ID+"/"+c.Account.Name+"|"+c.UpstreamModel)
	}
	return keys
}

func TestPlanOrderDefault(t *testing.T) {
	provs := twoProviders()
	p := NewPlanner(provs, nil)
	cands, err := p.Plan("prov/up-m", "")
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	want := []string{"prov/a1|up-m", "prov/a2|up-m"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v, want 前缀 %v", got, want)
		}
	}
}

func TestPlanRotateAdvances(t *testing.T) {
	provs := twoProviders()
	g := provider.Group{ID: "g", Strategy: provider.StrategyRotate,
		Members: []string{"prov/up-m", "prov-b/up-m"}} // 4 个候选
	p := NewPlanner(provs, []provider.Group{g})

	first, err := p.Plan("group/g", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := p.Plan("group/g", "")
	third, _ := p.Plan("group/g", "")
	fourth, _ := p.Plan("group/g", "")
	if candKeys(first)[0] != "prov/a1|up-m" || candKeys(second)[0] != "prov/a2|up-m" ||
		candKeys(third)[0] != "prov-b/b1|up-m" || candKeys(fourth)[0] != "prov-b/b2|up-m" {
		t.Fatalf("rotate 应逐候选轮换:\n1=%v\n2=%v\n3=%v\n4=%v",
			candKeys(first), candKeys(second), candKeys(third), candKeys(fourth))
	}
}

func TestPlanSmartSortsByRemainingRatio(t *testing.T) {
	provs := twoProviders()
	// provs 是切片，元素为副本；直接改切片内的账号用量
	provs[0].Accounts[0].QuotaTokensDay = 1000 // a1 剩 20%
	provs[0].Accounts[0].UsedTokens = 800
	provs[0].Accounts[1].QuotaTokensDay = 1000 // a2 剩 90%
	provs[0].Accounts[1].UsedTokens = 100
	provs[1].Accounts[0].QuotaTokensDay = 1000 // b1 剩 50%
	provs[1].Accounts[0].UsedTokens = 500
	provs[1].Accounts[1].QuotaTokensDay = 1000 // b2 剩 0（用尽）
	provs[1].Accounts[1].UsedTokens = 1000

	g := provider.Group{ID: "g", Strategy: provider.StrategySmart,
		Members: []string{"prov/up-m", "prov-b/up-m"}}
	p := NewPlanner(provs, []provider.Group{g})
	cands, err := p.Plan("group/g", "")
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	want := []string{"prov/a2|up-m", "prov-b/b1|up-m", "prov/a1|up-m", "prov-b/b2|up-m"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("smart 排序 = %v, want %v", got, want)
		}
	}
}

func TestPlanSmartWithoutUsageFallsBackToRotate(t *testing.T) {
	provs := twoProviders()
	g := provider.Group{ID: "g", Strategy: provider.StrategySmart, Members: []string{"prov/up-m"}}
	p := NewPlanner(provs, []provider.Group{g})
	first, _ := p.Plan("group/g", "")
	second, _ := p.Plan("group/g", "")
	if candKeys(first)[0] != "prov/a1|up-m" || candKeys(second)[0] != "prov/a2|up-m" {
		t.Fatalf("无用量数据时 smart 应回落 rotate: %v / %v", candKeys(first), candKeys(second))
	}
}

func TestPlanGroupCrossProviderMembers(t *testing.T) {
	provs := twoProviders()
	g := provider.Group{ID: "mix", Strategy: provider.StrategyOrder,
		Members: []string{"prov/up-m", "prov-b/other"}}
	p := NewPlanner(provs, []provider.Group{g})
	cands, err := p.Plan("group/mix", "")
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	want := []string{"prov/a1|up-m", "prov/a2|up-m", "prov-b/b1|other", "prov-b/b2|other"}
	if len(got) != len(want) {
		t.Fatalf("候选数 = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("跨 provider 组展开 = %v, want %v", got, want)
		}
	}
}

func TestPlanRestingGoesLastAcrossMembers(t *testing.T) {
	provs := twoProviders()
	g := provider.Group{ID: "mix", Members: []string{"prov/up-m", "prov-b/up-m"}}
	p := NewPlanner(provs, []provider.Group{g})
	// 让 prov-b 的两个账号休眠：它们应排在所有醒着候选之后
	p.Rest(Candidate{Provider: &provs[1], Account: provs[1].Accounts[0], UpstreamModel: "up-m"}, time.Minute)
	p.Rest(Candidate{Provider: &provs[1], Account: provs[1].Accounts[1], UpstreamModel: "up-m"}, time.Minute)
	cands, err := p.Plan("group/mix", "")
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	for i, k := range got[:2] {
		if k != "prov/a1|up-m" && k != "prov/a2|up-m" {
			t.Fatalf("醒着候选应在前，got[%d]=%s", i, k)
		}
	}
	for i, k := range got[2:] {
		if k != "prov-b/b1|up-m" && k != "prov-b/b2|up-m" {
			t.Fatalf("休眠候选应在末尾，got[%d]=%s", i+2, k)
		}
	}
}

func TestPlanAffinityMovesAwakeCandidateFirst(t *testing.T) {
	provs := twoProviders()
	p := NewPlanner(provs, nil)
	// 请求一次（模拟成功后）记录亲和
	p.NoteAffinity("sess-1", "prov/a2")
	cands, err := p.Plan("prov/up-m", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := candKeys(cands)[0]; got != "prov/a2|up-m" {
		t.Fatalf("亲和候选应提前, got %s (%v)", got, candKeys(cands))
	}
	// 其他会话不受影响
	cands2, _ := p.Plan("prov/up-m", "sess-2")
	if got := candKeys(cands2)[0]; got != "prov/a1|up-m" {
		t.Fatalf("无亲和会话应维持原序, got %s", got)
	}
}

func TestPlanAffinityRestingNotPromoted(t *testing.T) {
	provs := twoProviders()
	p := NewPlanner(provs, nil)
	p.NoteAffinity("sess-1", "prov/a1")
	p.Rest(Candidate{Provider: &provs[0], Account: provs[0].Accounts[0], UpstreamModel: "up-m"}, time.Minute)
	cands, err := p.Plan("prov/up-m", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := candKeys(cands)[0]; got != "prov/a2|up-m" {
		t.Fatalf("休眠的亲和候选不应提前, got %s", got)
	}
}

func TestPlanBareProviderAutoGroup(t *testing.T) {
	provs := []provider.Provider{{
		ID: "sf", Protocol: provider.ProtocolChat, BaseURL: "http://sf",
		Models:   []provider.Model{{ID: "m1"}, {ID: "m2"}},
		Accounts: []provider.Account{{Name: "a1", APIKey: "k", Status: provider.AccountAvailable}},
	}}
	p := NewPlanner(provs, nil)
	cands, err := p.Plan("sf", "")
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	want := []string{"sf/a1|m1", "sf/a1|m2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("自动组展开 = %v, want %v", got, want)
		}
	}
	// 未预检的 provider 不能按裸 id 路由
	p2 := NewPlanner([]provider.Provider{chatProvider("http://x")}, nil)
	if _, err := p2.Plan("prov", ""); err == nil {
		t.Fatal("无模型清单的裸 id 应报错")
	}
}

func TestPlanErrors(t *testing.T) {
	provs := twoProviders()
	p := NewPlanner(provs, []provider.Group{
		{ID: "mix", Members: []string{"prov/up-m", "nope/m"}},
	})
	if _, err := p.Plan("group/nope", ""); err == nil {
		t.Fatal("未知组应报错")
	}
	if _, err := p.Plan("group/mix", ""); err == nil {
		t.Fatal("组内未知 provider 应报错（配置错误要可见）")
	}
	if _, err := p.Plan("nope/m", ""); err == nil {
		t.Fatal("未知 provider 应报错")
	}
}

func TestPlanDisabledProviderYieldsEmpty(t *testing.T) {
	provs := []provider.Provider{chatProvider("http://x")}
	provs[0].Disabled = true
	p := NewPlanner(provs, nil)
	cands, err := p.Plan("prov/up-m", "")
	if err != nil || len(cands) != 0 {
		t.Fatalf("停用 provider 应返回空候选而非错误: %v %v", cands, err)
	}
}
