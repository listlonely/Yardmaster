// M2-W4 行为测试：额度感知路由、usage/weight 策略、成本报告与
// pricing 管理 API。还原旧代码即失败的断言见各用例注释。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/provider"
)

// quotaPlanner 构造一个 provider 两账号（a/b）的 Planner，注入额度与
// 当日用量字段；时钟固定在 2026-10-29（与用例中的 UsageDay 对齐）。
func quotaPlanner(t *testing.T, a, b provider.Account) *Planner {
	t.Helper()
	prov := provider.Provider{
		ID: "prov", Name: "P", Protocol: provider.ProtocolChat, BaseURL: "https://up.test",
		Models: []provider.Model{{ID: "m"}},
		Accounts: []provider.Account{
			{Name: "a", APIKey: "ka", Status: provider.AccountAvailable,
				QuotaTokensDay: a.QuotaTokensDay, UsedTokens: a.UsedTokens, UsageDay: a.UsageDay},
			{Name: "b", APIKey: "kb", Status: provider.AccountAvailable,
				QuotaTokensDay: b.QuotaTokensDay, UsedTokens: b.UsedTokens, UsageDay: b.UsageDay},
		},
	}
	p := NewPlanner([]provider.Provider{prov}, nil)
	p.SetNow(func() time.Time { return time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local) })
	return p
}

// 还原旧代码（耗尽账号不进休眠段）即失败：耗尽账号会仍排首位。
func TestQuotaExhaustedSkipsToNextAccount(t *testing.T) {
	p := quotaPlanner(t,
		provider.Account{QuotaTokensDay: 100, UsedTokens: 100, UsageDay: "2026-10-29"},
		provider.Account{})
	cands, err := p.Plan("prov/m", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("应有两个候选（耗尽不删除，只靠后）: %d", len(cands))
	}
	if cands[0].Account.Name != "b" {
		t.Fatalf("耗尽的 a 应让位给 b，得到首候选 %s", cands[0].Account.Name)
	}
	if cands[1].Account.Name != "a" {
		t.Fatalf("耗尽的 a 应排末段，得到 %s", cands[1].Account.Name)
	}
	// 抬高额度后 a 恢复 order 策略的首位
	provs := p.Providers()
	provs[0].Accounts[0].QuotaTokensDay = 1000
	p.Reload(provs, nil)
	cands2, err := p.Plan("prov/m", "")
	if err != nil {
		t.Fatal(err)
	}
	if cands2[0].Account.Name != "a" {
		t.Fatalf("额度抬升后 a 应恢复首选，得到 %s", cands2[0].Account.Name)
	}
	// Exhausted() 反映读侧状态
	if ex := p.Exhausted(); len(ex) != 0 {
		t.Fatalf("抬额后不应有耗尽账号: %v", ex)
	}
}

// 还原旧代码（usage 策略缺省）即失败：默认 order 会按声明顺序返回。
func TestUsageStrategySortsByUsedToday(t *testing.T) {
	prov := provider.Provider{
		ID: "p", Protocol: provider.ProtocolChat, BaseURL: "https://u.test",
		Models: []provider.Model{{ID: "m"}},
		Accounts: []provider.Account{
			{Name: "a1", UsedTokens: 300, UsageDay: "2026-10-29"},
			{Name: "a2", UsedTokens: 100, UsageDay: "2026-10-29"},
			{Name: "a3", UsedTokens: 200, UsageDay: "2026-10-29"},
		},
	}
	p := NewPlanner([]provider.Provider{prov},
		[]provider.Group{{ID: "g", Strategy: provider.StrategyUsage, Members: []string{"p/m"}}})
	p.SetNow(func() time.Time { return time.Date(2026, 10, 29, 10, 0, 0, 0, time.Local) })
	cands, err := p.Plan("group/g", "")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{cands[0].Account.Name, cands[1].Account.Name, cands[2].Account.Name}
	want := []string{"a2", "a3", "a1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("usage 策略应按当日用量升序: got %v want %v", got, want)
		}
	}
}

// 还原旧代码（weight 策略缺省）即失败：smooth WRR 的 3:1 序列为 a,a,b,a。
func TestWeightStrategySmoothWRR(t *testing.T) {
	prov := provider.Provider{
		ID: "p", Protocol: provider.ProtocolChat, BaseURL: "https://u.test",
		Models: []provider.Model{{ID: "m"}},
		Accounts: []provider.Account{
			{Name: "a", APIKey: "ka"}, {Name: "b", APIKey: "kb"},
		},
	}
	g := provider.Group{
		ID: "g", Strategy: provider.StrategyWeight, Members: []string{"p/m"},
		Weights: map[string]int{"p/a": 3}, // a 账号粒度权重 3，b 缺省 1
	}
	p := NewPlanner([]provider.Provider{prov}, []provider.Group{g})
	var picks []string
	for i := 0; i < 4; i++ {
		cands, err := p.Plan("group/g", "")
		if err != nil {
			t.Fatal(err)
		}
		picks = append(picks, cands[0].Account.Name)
	}
	if got, want := strings.Join(picks, ""), "aaba"; got != want {
		t.Fatalf("smooth WRR 序列不符: got %s want %s", got, want)
	}
}

func TestCostReportCompute(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local)
	mk := func(dayOffset int, served, model string, in, out int64, outcome string) Trace {
		return Trace{
			Time: now.AddDate(0, 0, -dayOffset).Format(time.RFC3339),
			Served: served, ServedModel: model, Outcome: outcome,
			Usage: &TraceUsage{Prompt: in, Completion: out},
		}
	}
	traces := []Trace{
		mk(0, "good/main", "quick-chat", 1_000_000, 1_000_000, "ok"),   // 0.27+1.10
		mk(1, "good/main", "quick-chat", 2_000_000, 0, "ok"),           // 0.54
		mk(1, "other/x", "mystery", 1_000_000, 0, "ok"),                // 无单价 → Unpriced
		mk(1, "good/main", "quick-chat", 5_000_000, 5_000_000, "fail"), // 失败不计费
		mk(9, "good/main", "quick-chat", 9_000_000, 0, "ok"),           // 7 天窗外
	}
	prices := []PriceEntry{{Model: "good/quick-chat", InputPerM: 0.27, OutputPerM: 1.10}}
	rep := ComputeCost(traces, prices, 7, now)
	if rep.From != "2026-10-23" || rep.To != "2026-10-29" {
		t.Fatalf("窗口不符: %s ~ %s", rep.From, rep.To)
	}
	acct := rep.ByAccount["good/main"]
	if acct.Calls != 2 || acct.InputTokens != 3_000_000 || acct.OutputTokens != 1_000_000 {
		t.Fatalf("账号桶不符: %+v", acct)
	}
	// 成本 = 1M*0.27 + 1M*1.10 + 2M*0.27 = 1.91（浮点容差）
	if acct.Cost < 1.9099 || acct.Cost > 1.9101 {
		t.Fatalf("成本计算不符: %v", acct.Cost)
	}
	if rep.ByModel["quick-chat"].Calls != 2 {
		t.Fatalf("模型桶不符: %+v", rep.ByModel)
	}
	if rep.Unpriced != 1 {
		t.Fatalf("未计价调用应为 1: %d", rep.Unpriced)
	}
	keys := SortedCostKeys(rep.ByAccount)
	if len(keys) != 1 || keys[0] != "good/main" {
		t.Fatalf("排序键不符: %v", keys)
	}
}

func TestPricingConfigAPIAndCostEndpoint(t *testing.T) {
	appDir := t.TempDir()
	s := &Server{
		Planner:       NewPlanner(nil, nil),
		Version:       "test",
		AppDir:        appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json"),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// PUT 合法单价表
	st, body := doPut(t, srv.URL, "/api/config/pricing",
		`{"prices":[{"model":"good/quick-chat","input_per_m":0.27,"output_per_m":1.1}]}`)
	if st != http.StatusOK {
		t.Fatalf("PUT pricing: HTTP %d: %s", st, body)
	}
	// GET 回读
	rsp, _ := http.Get(srv.URL + "/api/config/pricing")
	b, _ := io.ReadAll(rsp.Body)
	rsp.Body.Close()
	if !strings.Contains(string(b), "good/quick-chat") {
		t.Fatalf("GET pricing 未回读: %s", b)
	}
	// 重复 model 拒绝（校验失败不落盘）
	st, _ = doPut(t, srv.URL, "/api/config/pricing",
		`{"prices":[{"model":"a","input_per_m":1,"output_per_m":1},{"model":"a","input_per_m":2,"output_per_m":2}]}`)
	if st != http.StatusBadRequest {
		t.Fatalf("重复 model 应 400，得到 %d", st)
	}
	// /api/cost 可用（无 trace 时零报告形态完整）
	rsp2, _ := http.Get(srv.URL + "/api/cost?days=3")
	b2, _ := io.ReadAll(rsp2.Body)
	rsp2.Body.Close()
	var rep CostReport
	if err := json.Unmarshal(b2, &rep); err != nil {
		t.Fatalf("cost 响应解析失败: %s", b2)
	}
	if rep.Days != 3 || rep.ByAccount == nil || rep.ByModel == nil {
		t.Fatalf("cost 报告形态不符: %+v", rep)
	}
}

// 还原旧代码（/api/state 不暴露耗尽账号）即失败。
func TestStateExposesExhaustedAccounts(t *testing.T) {
	seed := seedProvidersFile()
	seed.Providers[0].Accounts[0].QuotaTokensDay = 100
	seed.Providers[0].Accounts[0].UsedTokens = 100
	seed.Providers[0].Accounts[0].UsageDay = time.Now().Format("2006-01-02")
	_, srv := newConfigServer(t, &seed)
	st, body := doGet(t, srv.URL+"/api/state")
	if st != http.StatusOK {
		t.Fatalf("GET /api/state: HTTP %d", st)
	}
	if !strings.Contains(string(body), `"prov/one"`) {
		t.Fatalf("state 应包含耗尽账号 prov/one: %s", body)
	}
	if !strings.Contains(string(body), "exhausted") {
		t.Fatalf("state 应有 exhausted 字段: %s", body)
	}
}
