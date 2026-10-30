// W5 测试：订阅窗口/余额喂路由（SwitchWhenSpent）、按 key 并发上限、
// 余额轮询降级。全程 httptest，不碰网络。
package gateway

import (
	"context"
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
