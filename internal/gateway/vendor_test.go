// W4 厂商特判测试：200 伪装拒绝识别、GLM 端点纠偏、认证头学习链路。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

func TestErrorReplyZhipuDisguised(t *testing.T) {
	// Zhipu 1113：以 200 返回的余额错误 → 402（额度/余额类）
	mapped, ok := errorReply([]byte(`{"error":{"code":"1113","message":"余额不足或无可用资源包,请充值。"}}`))
	if !ok || mapped != http.StatusPaymentRequired {
		t.Fatalf("1113 应映射 402: %d, %v", mapped, ok)
	}
	// 限频 → 429
	mapped, ok = errorReply([]byte(`{"error":{"message":"Rate limit exceeded, please try again later","type":"rate_limit_error"}}`))
	if !ok || mapped != http.StatusTooManyRequests {
		t.Fatalf("限频应映射 429: %d, %v", mapped, ok)
	}
	// Anthropic 形态的流中错误事件
	mapped, ok = sseErrorEvent([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"too many requests\"}}\n\n"))
	if !ok || mapped != http.StatusTooManyRequests {
		t.Fatalf("SSE 首事件错误应识别: %d, %v", mapped, ok)
	}
}

func TestErrorReplyIgnoresNormalReplies(t *testing.T) {
	cases := []string{
		`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
		`{"content":[{"type":"text","text":"hi"}]}`,
		`{"id":"1","object":"chat.completion","choices":[]}`,
		`not-json`,
		``,
		`data: {"error":"x"}`, // 非 JSON 起始（SSE 由 sseErrorEvent 处理）
	}
	for _, body := range cases {
		if _, ok := errorReply([]byte(body)); ok {
			t.Fatalf("正常回复不应判为错误: %s", body)
		}
	}
	// SSE 首个 data 是正常事件 → 不判错
	if _, ok := sseErrorEvent([]byte("data: {\"id\":\"1\"}\n\ndata: {\"error\":\"x\"}\n\n")); ok {
		t.Fatal("首个正常事件后的错误不参与 failover（响应已开始）")
	}
}

func TestGLMCorrectedBase(t *testing.T) {
	cases := []struct {
		base string
		want string
		ok   bool
	}{
		{"https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/coding/paas/v4", true},
		{"https://api.z.ai/api/paas/v4", "https://api.z.ai/api/coding/paas/v4", true},
		{"http://127.0.0.1:1234/api/paas/v4", "http://127.0.0.1:1234/api/coding/paas/v4", true},
		{"https://open.bigmodel.cn/api/coding/paas/v4", "", false}, // 已是 coding 端点
		{"https://open.bigmodel.cn/api/anthropic", "", false},      // messages 即 Plan 端点
		{"https://api.deepseek.com", "", false},
	}
	for _, tc := range cases {
		got, ok := glmCorrectedBase(tc.base, []byte(`{"error":{"code":"1113","message":"余额不足或无可用资源包,请充值。"}}`))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("glmCorrectedBase(%s) = %q, %v; want %q, %v", tc.base, got, ok, tc.want, tc.ok)
		}
	}
	// 非 1113 不纠偏
	if _, ok := glmCorrectedBase("https://open.bigmodel.cn/api/paas/v4", []byte(`{"error":{"code":"1002"}}`)); ok {
		t.Fatal("非 1113 不应纠偏")
	}
}

// TestGLMQuotaReset：429 错误体重置时刻解析（M6-W1 实测 1308 文案）。
func TestGLMQuotaReset(t *testing.T) {
	// M6-W1 冒烟实测样本（智谱 Coding Plan 1308）。
	got, ok := glmQuotaReset([]byte(`{"error":{"code":"1308","message":"已达到 5 小时的使用上限。您的限额将在 2026-10-30 16:23:38 重置。"}}`))
	if !ok {
		t.Fatal("1308 样本应解析出重置时刻")
	}
	want := time.Date(2026, 10, 30, 16, 23, 38, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("解析 = %v, want %v", got, want)
	}
	// 秒缺省形态
	got, ok = glmQuotaReset([]byte(`{"error":{"code":"1308","message":"您的限额将在 2026-10-30 16:23 重置"}}`))
	if !ok || got != time.Date(2026, 10, 30, 16, 23, 0, 0, time.Local) {
		t.Fatalf("秒缺省 = %v, %v", got, ok)
	}
	// 读不懂=未知：无重置时间 / 英文文案 / 畸形日期
	for _, body := range []string{
		`{"error":{"message":"quota"}}`,
		`{"error":{"message":"Rate limit exceeded, please try again later"}}`,
		`{"error":{"code":"1308","message":"您的限额将在明天重置"}}`,
		`{"error":{"code":"1308","message":"限额将在 2026-13-99 99:99:99 重置"}}`,
	} {
		if _, ok := glmQuotaReset([]byte(body)); ok {
			t.Fatalf("不应解析出时刻: %s", body)
		}
	}
}

func TestCorrectBaseURLPersists(t *testing.T) {
	appDir := t.TempDir()
	paths := store.Paths{AppDir: appDir}
	f := store.ProvidersFile{Providers: []provider.Provider{{
		ID: "glm", Protocol: provider.ProtocolChat, BaseURL: "https://open.bigmodel.cn/api/paas/v4",
		Accounts: []provider.Account{{Name: "main", APIKey: "k"}},
	}}}
	if err := store.SaveProvidersFile(paths.ProvidersPath(), f); err != nil {
		t.Fatal(err)
	}
	p := NewPlanner(f.Providers, nil)
	if err := p.CorrectBaseURL("glm", "https://open.bigmodel.cn/api/coding/paas/v4", paths.ProvidersPath()); err != nil {
		t.Fatalf("CorrectBaseURL: %v", err)
	}
	// 内存与文件均已更新
	if got := p.Providers()[0].BaseURL; got != "https://open.bigmodel.cn/api/coding/paas/v4" {
		t.Fatalf("内存: %s", got)
	}
	f2, err := store.LoadProvidersFile(paths.ProvidersPath())
	if err != nil || f2.Providers[0].BaseURL != "https://open.bigmodel.cn/api/coding/paas/v4" {
		t.Fatalf("落盘: %+v, %v", f2.Providers[0].BaseURL, err)
	}
	// 写前备份存在
	snaps, _ := filepath.Glob(filepath.Join(paths.BackupDir(), "providers-*.json"))
	if len(snaps) == 0 {
		t.Fatal("纠偏落盘应先留底")
	}
}

// TestAttemptGLMCorrectionEndToEnd：按量端点回 200+1113 → 纠偏到 coding
// 端点重试成功，base_url 落盘更新。
func TestAttemptGLMCorrectionEndToEnd(t *testing.T) {
	appDir := t.TempDir()
	paths := store.Paths{AppDir: appDir}
	prov := provider.Provider{
		ID: "glm", Protocol: provider.ProtocolChat,
		BaseURL:  "https://open.bigmodel.cn/api/paas/v4",
		Accounts: []provider.Account{{Name: "main", APIKey: "k", Status: provider.AccountAvailable}},
	}
	if err := store.SaveProvidersFile(paths.ProvidersPath(), store.ProvidersFile{Providers: []provider.Provider{prov}}); err != nil {
		t.Fatal(err)
	}
	var codingHit atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/paas/v4/"):
			// 伪装拒绝：200 + 1113
			_, _ = w.Write([]byte(`{"error":{"code":"1113","message":"余额不足或无可用资源包,请充值。"}}`))
		case strings.HasPrefix(r.URL.Path, "/api/coding/paas/v4/"):
			codingHit.Add(1)
			_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		default:
			t.Errorf("意外路径 %q", r.URL.Path)
		}
	}))
	defer up.Close()

	planner := NewPlanner([]provider.Provider{prov}, nil)
	// 把 base 指向本地 fake（保持 /api/paas/v4 形状）
	planner.Providers()[0].BaseURL = up.URL + "/api/paas/v4"
	srv := &Server{Planner: planner, AppDir: appDir, ProvidersPath: paths.ProvidersPath()}
	cands, err := planner.Plan("glm/glm-x", "")
	if err != nil || len(cands) != 1 {
		t.Fatalf("Plan: %v, %d", err, len(cands))
	}
	res := srv.attempt(t.Context(), ProtoChat, nil, []byte(`{"model":"glm-x","messages":[]}`), false, cands[0])
	if res.err != nil || res.resp == nil {
		t.Fatalf("纠偏重试应成功: %+v", res)
	}
	if codingHit.Load() != 1 {
		t.Fatalf("应命中 coding 端点 1 次: %d", codingHit.Load())
	}
	if got := planner.Providers()[0].BaseURL; !strings.Contains(got, "/api/coding/paas/v4") {
		t.Fatalf("内存 base 应纠偏: %s", got)
	}
	f2, _ := store.LoadProvidersFile(paths.ProvidersPath())
	if !strings.Contains(f2.Providers[0].BaseURL, "/api/coding/paas/v4") {
		t.Fatalf("落盘 base 应纠偏: %s", f2.Providers[0].BaseURL)
	}
}

// TestAttempt200ErrorFailsOver：候选 A 回 200+限频错误 → 按上游失败切换
// 候选 B（跨 provider 路由组）正常服务。
func TestAttempt200ErrorFailsOver(t *testing.T) {
	appDir := t.TempDir()
	paths := store.Paths{AppDir: appDir}
	errUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`))
	}))
	defer errUp.Close()
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer okUp.Close()
	provA := provider.Provider{ID: "pa", Protocol: provider.ProtocolChat, BaseURL: errUp.URL,
		Accounts: []provider.Account{{Name: "a", APIKey: "k1", Status: provider.AccountAvailable}}}
	provB := provider.Provider{ID: "pb", Protocol: provider.ProtocolChat, BaseURL: okUp.URL,
		Accounts: []provider.Account{{Name: "b", APIKey: "k2", Status: provider.AccountAvailable}}}
	groups := []provider.Group{{ID: "g", Strategy: provider.StrategyOrder, Members: []string{"pa/m", "pb/m"}}}
	if err := store.SaveProvidersFile(paths.ProvidersPath(), store.ProvidersFile{Providers: []provider.Provider{provA, provB}, Groups: groups}); err != nil {
		t.Fatal(err)
	}
	planner := NewPlanner([]provider.Provider{provA, provB}, groups)
	srv := &Server{Planner: planner, AppDir: appDir, ProvidersPath: paths.ProvidersPath()}
	cands, err := planner.Plan("group/g", "")
	if err != nil || len(cands) != 2 {
		t.Fatalf("Plan: %v, %d", err, len(cands))
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	srv.serveWithFailover(w, r, ProtoChat, nil, []byte(`{"model":"m","messages":[]}`), false, cands, "", nil, time.Now())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("候选 B 应接管服务: %d %s", w.Code, w.Body.String())
	}
	// 候选 A 被休眠（429 语义）
	if _, resting := planner.Resting()["pa/a"]; !resting {
		t.Fatal("伪装错误的候选 A 应被休眠")
	}
}
