// W5 测试：订阅窗口/余额喂路由（SwitchWhenSpent）、按 key 并发上限、
// 余额轮询降级。全程 httptest，不碰网络。
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

func TestParseWindowHeaders(t *testing.T) {
	// Anthropic unified：restricted + RFC3339 重置
	reset := time.Now().Add(3 * time.Minute).Truncate(time.Second)
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "restricted")
	h.Set("anthropic-ratelimit-unified-5m_reset", reset.UTC().Format(time.RFC3339))
	st, at := parseWindowHeaders(h)
	if st != "restricted" || at != reset.Unix() {
		t.Fatalf("anthropic: %q %d", st, at)
	}
	// OpenAI 风格：remaining=0 + Go duration
	h2 := http.Header{}
	h2.Set("x-ratelimit-remaining-requests", "0")
	h2.Set("x-ratelimit-reset-requests", "6m0s")
	st2, at2 := parseWindowHeaders(h2)
	if st2 != "restricted" || at2 <= time.Now().Unix() {
		t.Fatalf("openai: %q %d", st2, at2)
	}
	// 无头 → 空
	if st3, at3 := parseWindowHeaders(http.Header{}); st3 != "" || at3 != 0 {
		t.Fatalf("无头应空: %q %d", st3, at3)
	}
}

func TestExtractBalance(t *testing.T) {
	cases := []struct {
		body string
		want float64
	}{
		{`{"code":200,"data":{"total_balance":"123.45"}}`, 123.45},
		{`{"balance": 0}`, 0},
		{`{"result":{"credit": 7.5}}`, 7.5},
	}
	for _, tc := range cases {
		got, ok := extractBalance([]byte(tc.body))
		if !ok || got != tc.want {
			t.Fatalf("%s → %v, %v; want %v", tc.body, got, ok, tc.want)
		}
	}
	if _, ok := extractBalance([]byte(`{"msg":"no numbers here"}`)); ok {
		t.Fatal("无数值字段应返回 false")
	}
}

// 订阅窗口形态（GLM /api/monitor/usage/quota/limit，真机采样样本
// [M9-W3]）：取 limits[] 最小 remaining，与数组顺序无关。
func TestExtractPlanQuota(t *testing.T) {
	realSample := `{"code":200,"msg":"操作成功","success":true,"data":{"level":"pro","limits":[
		{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":12000,"currentValue":945,
		 "remaining":11054,"percentage":7,"nextResetTime":1793690514565},
		{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":60000,"currentValue":14331,
		 "remaining":45668,"percentage":23,"nextResetTime":1793933920971}]}}`
	got, ok := extractPlanQuota([]byte(realSample))
	if !ok || got != 11054 {
		t.Fatalf("真机样本应取最小 remaining 11054: %v %v", got, ok)
	}
	// 顺序翻转：仍取最紧窗口（不依赖数组顺序——通用 extractBalance 的
	// "remaining" 键命中会取数组首个，语义错误）
	reversed := `{"success":true,"data":{"limits":[
		{"remaining":45668,"percentage":23},{"remaining":11054,"percentage":7}]}}`
	if got, ok := extractPlanQuota([]byte(reversed)); !ok || got != 11054 {
		t.Fatalf("顺序无关: %v %v", got, ok)
	}
	// 通用余额形态不应被 plan 解析器吞掉（probeBalance 回退路径）
	if _, ok := extractPlanQuota([]byte(`{"code":200,"data":{"total_balance":"123.45"}}`)); ok {
		t.Fatal("余额形态应返回 false 走回退")
	}
	if _, ok := extractPlanQuota([]byte(`{"success":false,"msg":"denied"}`)); ok {
		t.Fatal("无 limits 应返回 false")
	}
}

// SwitchWhenSpent：窗口 restricted 的候选排末段，健康候选接管服务。
func TestWindowRestrictedFailsOver(t *testing.T) {
	appDir := t.TempDir()
	paths := newPaths(appDir)
	spentUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "restricted")
		w.Header().Set("anthropic-ratelimit-unified-5m_reset", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"content":"from-spent"}}]}`))
	}))
	defer spentUp.Close()
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"2","choices":[{"message":{"content":"from-healthy"}}]}`))
	}))
	defer okUp.Close()

	provA := provider.Provider{ID: "pa", Protocol: provider.ProtocolChat, BaseURL: spentUp.URL,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1", Status: provider.AccountAvailable}}}
	provB := provider.Provider{ID: "pb", Protocol: provider.ProtocolChat, BaseURL: okUp.URL,
		Accounts: []provider.Account{{Name: "b", APIKey: "k2", Status: provider.AccountAvailable}}}
	groups := []provider.Group{{ID: "g", Strategy: provider.StrategyOrder, Members: []string{"pa/m", "pb/m"}}}
	planner := NewPlanner([]provider.Provider{provA, provB}, groups)
	srv := &Server{Planner: planner, AppDir: appDir, ProvidersPath: paths.ProvidersPath()}
	cands, err := planner.Plan("group/g", "")
	if err != nil || len(cands) != 2 {
		t.Fatalf("Plan: %v, %d", err, len(cands))
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	// 第一轮：restricted 尚未学习，候选 A 正常服务并从响应头学习窗口
	srv.serveWithFailover(w, r, ProtoChat, nil, []byte(`{"model":"m","messages":[]}`), false, cands, "", nil, time.Now())
	acc := planner.Providers()[0].Accounts[0]
	if acc.WindowStatus != "restricted" || acc.WindowResetAt <= time.Now().Unix() {
		t.Fatalf("首轮应学习窗口: %+v", acc)
	}
	// 第二轮：restricted 候选排末段，健康候选接管
	w2 := httptest.NewRecorder()
	cands2, _ := planner.Plan("group/g", "")
	srv.serveWithFailover(w2, r, ProtoChat, nil, []byte(`{"model":"m","messages":[]}`), false, cands2, "", nil, time.Now())
	if !strings.Contains(w2.Body.String(), "from-healthy") {
		t.Fatalf("健康候选应接管: %s", w2.Body.String())
	}
	if cands2[0].Key() == "pa/a" {
		t.Fatal("restricted 候选不应排在首位")
	}
	// 窗口重置过后恢复（注入时钟越过 reset）
	for j := range planner.Providers()[0].Accounts {
		planner.Providers()[0].Accounts[j].WindowResetAt = time.Now().Add(-time.Second).Unix()
	}
	cands3, _ := planner.Plan("group/g", "")
	if cands3[0].Key() != "pa/a" {
		t.Fatalf("窗口重置后应恢复候选首段: %s", cands3[0].Key())
	}
}

// 余额轮询：数值落账；耗尽账号排末段；失败只降级。
func TestQuotaPollerFeedsRouting(t *testing.T) {
	appDir := t.TempDir()
	paths := newPaths(appDir)
	var balanceResp string
	balSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if balanceResp == "" {
			balanceResp = `{"code":200,"data":{"total_balance":"12.30"}}`
		}
		_, _ = w.Write([]byte(balanceResp))
	}))
	defer balSrv.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()

	prov := provider.Provider{ID: "glm", Protocol: provider.ProtocolChat, BaseURL: up.URL,
		QuotaProbe: &provider.QuotaProbe{URL: balSrv.URL},
		Accounts:   []provider.Account{{Name: "main", APIKey: "k", Status: provider.AccountAvailable}}}
	planner := NewPlanner([]provider.Provider{prov}, nil)
	srv := &Server{Planner: planner, AppDir: appDir, ProvidersPath: paths.ProvidersPath()}
	srv.pollQuotas(context.Background())
	acc := planner.Providers()[0].Accounts[0]
	if acc.Balance != 12.30 || acc.BalanceAt == 0 {
		t.Fatalf("余额应学习: %+v", acc)
	}
	// 耗尽 → 末段
	planner.Providers()[0].Accounts[0].Balance = 0
	cands, _ := planner.Plan("glm/glm-x", "")
	if cands[0].Key() != "glm/main" {
		t.Fatalf("唯一候选仍在（末段）: %s", cands[0].Key())
	}
	if c := planner.Providers()[0].Accounts[0]; !accountSpent(c, time.Now()) {
		t.Fatal("余额 0 应判 spent")
	}
	// 轮询失败（端点 500）→ 保留上次值（降级）
	balanceResp = "boom"
	srv.pollQuotas(context.Background())
	if acc2 := planner.Providers()[0].Accounts[0]; acc2.Balance != 0 || acc2.BalanceAt == 0 {
		t.Fatalf("失败不应清除既有值: %+v", acc2)
	}
}

// 手动触发余额探测 API（M9-W3）：与后台 ticker 同路径，响应携带
// 各账号探测结果 + 写后 state（余额已落账）。
func TestQuotaProbeAPI(t *testing.T) {
	appDir := t.TempDir()
	paths := newPaths(appDir)
	balSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"total_balance":"42.5"}}`))
	}))
	defer balSrv.Close()

	prov := provider.Provider{ID: "glm", Protocol: provider.ProtocolChat, BaseURL: "http://up.invalid",
		QuotaProbe: &provider.QuotaProbe{URL: balSrv.URL},
		Accounts:   []provider.Account{{Name: "main", APIKey: "k", Status: provider.AccountAvailable}}}
	srv := &Server{Planner: NewPlanner([]provider.Provider{prov}, nil), AppDir: appDir, ProvidersPath: paths.ProvidersPath()}
	api := httptest.NewServer(srv.Handler())
	defer api.Close()

	resp, err := http.Post(api.URL+"/api/quota/probe", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		OK     bool `json:"ok"`
		Probes []struct {
			Provider string  `json:"provider"`
			Account  string  `json:"account"`
			Balance  float64 `json:"balance"`
			Err      string  `json:"error"`
		} `json:"probes"`
		State struct {
			Accounts []struct {
				Key     string  `json:"key"`
				Balance float64 `json:"balance"`
			} `json:"accounts"`
		} `json:"state"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("解析响应: %v\n%s", err, b)
	}
	if !out.OK || len(out.Probes) != 1 || out.Probes[0].Balance != 42.5 || out.Probes[0].Err != "" {
		t.Fatalf("探测结果异常: %+v", out.Probes)
	}
	// 写后 state 携带余额（喂路由可观测；账号在顶层扁平 accounts）
	found := false
	for _, a := range out.State.Accounts {
		if a.Key == "glm/main" && a.Balance == 42.5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("state 应携带余额: %s", b)
	}
	// 内存态已更新（后续 Plan 直接可见）
	if acc := srv.Planner.Providers()[0].Accounts[0]; acc.Balance != 42.5 || acc.BalanceAt == 0 {
		t.Fatalf("Planner 应落账: %+v", acc)
	}
}

// 按 key 并发上限：满载账号排末段，其余候选先服务。
func TestMaxConcurrentCap(t *testing.T) {
	prov := provider.Provider{ID: "p", Protocol: provider.ProtocolChat, BaseURL: "http://up.invalid",
		Accounts: []provider.Account{
			{Name: "busy", APIKey: "k1", Status: provider.AccountAvailable, MaxConcurrent: 1},
			{Name: "idle", APIKey: "k2", Status: provider.AccountAvailable},
		}}
	planner := NewPlanner([]provider.Provider{prov}, nil)
	planner.Enter("p/busy") // busy 账号 1 路在途，达到上限
	cands, err := planner.Plan("p/m", "")
	if err != nil || len(cands) != 2 {
		t.Fatalf("Plan: %v, %d", err, len(cands))
	}
	if cands[0].Key() != "p/idle" {
		t.Fatalf("满载账号应排末段: %s", cands[0].Key())
	}
	planner.Exit("p/busy")
	cands2, _ := planner.Plan("p/m", "")
	if cands2[0].Key() != "p/busy" {
		t.Fatalf("归还后应恢复: %s", cands2[0].Key())
	}
}

// newPaths 返回测试数据目录路径。
func newPaths(appDir string) store.Paths { return store.Paths{AppDir: appDir} }
