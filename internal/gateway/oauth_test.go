// W3 OAuth 运行时测试：token 注入、主动/被动刷新、single-flight、
// 失败冷却。全程 httptest，不碰网络、不碰真机凭证。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yardmaster/internal/credential"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// newOAuthServer 装配带凭证库与 providers.json 的测试网关。
func newOAuthServer(t *testing.T) (*Server, string) {
	t.Helper()
	appDir := t.TempDir()
	paths := store.Paths{AppDir: appDir}
	prov := provider.Provider{
		ID: "anthropic", Name: "Anthropic", Protocol: provider.ProtocolMessages,
		BaseURL: "https://upstream.example",
		Accounts: []provider.Account{{
			Name: "main", Status: provider.AccountAvailable,
			Auth: provider.AuthOAuth, OAuth: &provider.OAuthMeta{Format: "claude-code", ExpiresAt: 1893456000},
		}},
	}
	f := store.ProvidersFile{Providers: []provider.Provider{prov}}
	if err := store.SaveProvidersFile(paths.ProvidersPath(), f); err != nil {
		t.Fatal(err)
	}
	cs := &credential.Store{Path: paths.CredentialsPath(), BackupDir: paths.BackupDir()}
	ref, _ := credential.NewRef("anthropic", "main")
	if err := cs.Save(ref, credential.OAuth{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1893456000}); err != nil {
		t.Fatal(err)
	}
	planner := NewPlanner(f.Providers, nil)
	srv := &Server{Planner: planner, AppDir: appDir, ProvidersPath: paths.ProvidersPath(), Creds: cs}
	return srv, appDir
}

// injectRefreshSpec 把刷新端点指向本地 httptest。
func injectRefreshSpec(t *testing.T, srv *Server, hit *atomic.Int64, respFunc func() string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respFunc()))
	}))
	t.Cleanup(ts.Close)
	srv.SetRefreshSpec("claude-code", credential.RefreshSpec{TokenURL: ts.URL, ClientID: "cid"})
}

func TestUpstreamTokenAPIKeyPassthrough(t *testing.T) {
	srv, _ := newOAuthServer(t)
	cand := Candidate{
		Provider: &provider.Provider{ID: "p", Accounts: []provider.Account{{Name: "a", APIKey: "sk-x"}}},
		Account:  provider.Account{Name: "a", APIKey: "sk-x"},
	}
	tok, err := srv.upstreamToken(cand)
	if err != nil || tok != "sk-x" {
		t.Fatalf("api_key 应原样: %q, %v", tok, err)
	}
}

func TestUpstreamTokenOAuthFromStore(t *testing.T) {
	srv, _ := newOAuthServer(t)
	cands, err := srv.Planner.Plan("anthropic/claude-x", "")
	if err != nil || len(cands) != 1 {
		t.Fatalf("Plan: %v, %d", err, len(cands))
	}
	tok, err := srv.upstreamToken(cands[0])
	if err != nil || tok != "at-old" {
		t.Fatalf("oauth token 应从凭证库读取: %q, %v", tok, err)
	}
	// 二次读取走缓存（行为即结果）
	if tok2, _ := srv.upstreamToken(cands[0]); tok2 != "at-old" {
		t.Fatalf("缓存读取: %q", tok2)
	}
}

func TestUpstreamTokenOAuthMissingIsError(t *testing.T) {
	srv, appDir := newOAuthServer(t)
	// 凭证库损坏 = 读不懂：绝不返回空 token
	csPath := store.Paths{AppDir: appDir}.CredentialsPath()
	if err := os.WriteFile(csPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.oauth.cache = nil // 清缓存强制重读
	cands, _ := srv.Planner.Plan("anthropic/claude-x", "")
	if _, err := srv.upstreamToken(cands[0]); err == nil {
		t.Fatal("损坏凭证库应报错")
	}
}

func TestRefreshAccountSuccess(t *testing.T) {
	srv, appDir := newOAuthServer(t)
	var hit atomic.Int64
	injectRefreshSpec(t, srv, &hit, func() string {
		return `{"access_token":"at-new","expires_in":3600}`
	})
	if err := srv.refreshAccount("anthropic", "main"); err != nil {
		t.Fatalf("refreshAccount: %v", err)
	}
	if hit.Load() != 1 {
		t.Fatalf("token 端点应命中 1 次: %d", hit.Load())
	}
	// 凭证库新值（不轮换时保留旧 refresh_token）
	paths := store.Paths{AppDir: appDir}
	cs := credential.Store{Path: paths.CredentialsPath()}
	o, err := cs.Load("anthropic/main")
	if err != nil || o.AccessToken != "at-new" || o.RefreshToken != "rt-old" {
		t.Fatalf("凭证库: %+v, %v", o, err)
	}
	if o.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("过期时间应前移: %d", o.ExpiresAt)
	}
	// providers.json 元数据更新（不含 token）
	raw, _ := os.ReadFile(paths.ProvidersPath())
	meta := string(raw)
	if !strings.Contains(meta, `"refreshed_at"`) || strings.Contains(meta, "at-new") {
		t.Fatalf("元数据应更新且不含 token: %s", meta)
	}
	// 休眠解除
	if _, resting := srv.Planner.Resting()["anthropic/main"]; resting {
		t.Fatal("刷新成功应解除休眠")
	}
	// 后续请求用新 token（缓存已更新）
	cands, _ := srv.Planner.Plan("anthropic/claude-x", "")
	if tok, _ := srv.upstreamToken(cands[0]); tok != "at-new" {
		t.Fatalf("缓存应更新为新 token: %q", tok)
	}
}

func TestRefreshAccountFailsThenBadCred(t *testing.T) {
	srv, appDir := newOAuthServer(t)
	var hit atomic.Int64
	injectRefreshSpec(t, srv, &hit, func() string {
		return `{"error":"invalid_grant"}`
	})
	for i := 1; i <= 3; i++ {
		if err := srv.refreshAccount("anthropic", "main"); err == nil {
			t.Fatalf("#%d 应失败", i)
		}
	}
	if hit.Load() != 3 {
		t.Fatalf("应刷新 3 次: %d", hit.Load())
	}
	// 连续失败超限 → bad-credential（不自动重试）
	if srv.Planner.Providers()[0].Accounts[0].Status != provider.AccountBadCred {
		t.Fatalf("应标记 bad-credential: %q", srv.Planner.Providers()[0].Accounts[0].Status)
	}
	// 元数据 LastError（脱敏，无 token）
	raw, _ := os.ReadFile(filepath.Join(appDir, "providers.json"))
	if !strings.Contains(string(raw), "last_error") {
		t.Fatalf("应记录 last_error: %s", raw)
	}
	if strings.Contains(string(raw), "rt-old") || strings.Contains(string(raw), "at-old") {
		t.Fatal("元数据不得含 token")
	}
}

func TestTriggerRefreshSingleFlight(t *testing.T) {
	srv, _ := newOAuthServer(t)
	var hit atomic.Int64
	release := make(chan struct{})
	injectRefreshSpec(t, srv, &hit, func() string {
		<-release // 阻塞首个刷新，制造并发窗口
		return `{"access_token":"at-new","expires_in":3600}`
	})
	cands, _ := srv.Planner.Plan("anthropic/claude-x", "")
	f1 := srv.triggerRefresh(cands[0])
	f2 := srv.triggerRefresh(cands[0])
	if f1 == nil || f2 == nil || f1 != f2 {
		t.Fatal("并发触发应共享同一 flight")
	}
	close(release)
	<-f1.done
	if f1.err != nil {
		t.Fatalf("flight err: %v", f1.err)
	}
	if hit.Load() != 1 {
		t.Fatalf("single-flight 应只打 1 次端点: %d", hit.Load())
	}
}

func TestRefreshDueAccounts(t *testing.T) {
	srv, _ := newOAuthServer(t)
	var hit atomic.Int64
	injectRefreshSpec(t, srv, &hit, func() string {
		return `{"access_token":"at-new","expires_in":3600}`
	})
	// 未到期 → 不刷新
	srv.refreshDueAccounts()
	if hit.Load() != 0 {
		t.Fatalf("未到期不应刷新: %d", hit.Load())
	}
	// 到期临近 → 刷新
	soon := time.Now().Add(5 * time.Minute).Unix()
	for _, p := range srv.Planner.Providers() {
		for j := range p.Accounts {
			p.Accounts[j].OAuth.ExpiresAt = soon
		}
	}
	srv.refreshDueAccounts()
	if hit.Load() != 1 {
		t.Fatalf("临近到期应刷新 1 次: %d", hit.Load())
	}
	// 元数据 State=ok、过期前移
	f, err := store.LoadProvidersFile(srv.ProvidersPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Providers[0].Accounts[0].OAuth; got.State != provider.OAuthStateOK || got.ExpiresAt <= soon {
		t.Fatalf("元数据: %+v", got)
	}
}

func TestOAuthCandidate401TriggersRefresh(t *testing.T) {
	srv, appDir := newOAuthServer(t)
	// 上游：401；token 端点正常
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"expired"}`))
	}))
	defer upSrv.Close()
	var hit atomic.Int64
	injectRefreshSpec(t, srv, &hit, func() string {
		return `{"access_token":"at-new","expires_in":3600}`
	})
	for i := range srv.Planner.Providers() {
		srv.Planner.Providers()[i].BaseURL = upSrv.URL
	}
	cands, _ := srv.Planner.Plan("anthropic/claude-x", "")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	srv.serveWithFailover(w, r, ProtoMessages, nil, []byte(`{}`), false, cands, "", nil, time.Now())
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("全败应回上游状态: %d", w.Code)
	}
	// 被动刷新已异步触发并完成
	waitOAuthRefreshed(t, srv)
	if hit.Load() != 1 {
		t.Fatalf("401 应触发刷新: %d", hit.Load())
	}
	paths := store.Paths{AppDir: appDir}
	o, err := credential.Store{Path: paths.CredentialsPath()}.Load("anthropic/main")
	if err != nil || o.AccessToken != "at-new" {
		t.Fatalf("刷新应落库: %+v, %v", o, err)
	}
}

// waitOAuthRefreshed 轮询等待异步刷新完成（测试辅助）。
func waitOAuthRefreshed(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 200; i++ {
		paths := store.Paths{AppDir: s.AppDir}
		o, err := credential.Store{Path: paths.CredentialsPath()}.Load("anthropic/main")
		if err == nil && o.AccessToken == "at-new" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("刷新未在限时内完成")
}
