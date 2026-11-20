// 组员可用性审计测试（2026-11-20）：判定器真值表 + 假上游实测 +
// 移除应用（含空组保底）+ 留底写回。判定从严：401/429/5xx/传输层
// 错误一律保留（暂时态交给既有休眠机制），仅确定性不可用才移除。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

func TestMemberUnavailableVerdict(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string // 非空=应移除
	}{
		{"403 权限拒绝", http.StatusForbidden, `{"error":"forbidden"}`, "403"},
		{"404 模型不存在", http.StatusNotFound, `not found`, "404"},
		{"401 凭证失败保留", http.StatusUnauthorized, `invalid api key`, ""},
		{"429 真限速保留", http.StatusTooManyRequests, `rate limited`, ""},
		{"429 壳带套餐语义移除（智谱 1311 实证）", http.StatusTooManyRequests, `1311 当前订阅套餐暂未开放GLM-5.3-FlashX权限`, "套餐"},
		{"401 即使带语义也保留（凭证态交既有机制）", http.StatusUnauthorized, `套餐未开放`, ""},
		{"5xx 上游故障保留", http.StatusInternalServerError, `internal error`, ""},
		{"2xx 可调用", http.StatusOK, `{}`, ""},
		{"传输层失败保留", 0, "", ""},
		{"400 智谱套餐语义", http.StatusBadRequest, `1113 当前订阅套餐暂未开放权限`, "套餐"},
		{"400 余额语义", http.StatusBadRequest, `余额不足或无可用资源包`, "资源包"}, // 词表顺序：资源包 先命中
		{"400 英文 entitlement", http.StatusBadRequest, `model is not entitled for this account`, "not entitled"},
		{"400 无语义保留", http.StatusBadRequest, `messages 数组格式错误`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := memberUnavailable(c.status, c.body)
			if c.want == "" && got != "" {
				t.Fatalf("应保留，却判定移除: %q", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("应移除（含 %q），实际: %q", c.want, got)
			}
		})
	}
}

// TestProbeModelProtocols 验证四协议的端点/认证头/最小请求形状。
func TestProbeModelProtocols(t *testing.T) {
	var gotPath, gotAuth, gotVer, gotKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVer = r.Header.Get("anthropic-version")
		gotKey = r.Header.Get("x-goog-api-key")
		dec := json.NewDecoder(r.Body)
		_ = dec.Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	base := func(p provider.Protocol) *provider.Provider {
		return &provider.Provider{ID: "p", Protocol: p, BaseURL: srv.URL, AuthStyle: "bearer"}
	}
	acc := provider.Account{Name: "a", APIKey: "sk-test"}

	// openai-chat：Bearer + chat/completions + model/max_tokens
	if status, _, err := ProbeModel(context.Background(), base(provider.ProtocolChat), acc, "m1", nil, ""); err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer sk-test" ||
		gotBody["model"] != "m1" || gotBody["max_tokens"] != float64(16) {
		t.Fatalf("chat 形状不符: path=%s auth=%s body=%v", gotPath, gotAuth, gotBody)
	}
	// anthropic-messages：bearer 覆盖 + 版本头 + /v1/messages
	if _, _, err := ProbeModel(context.Background(), base(provider.ProtocolMessages), acc, "m1", nil, "bearer"); err != nil {
		t.Fatalf("messages: %v", err)
	}
	if gotPath != "/v1/messages" || gotAuth != "Bearer sk-test" || gotVer == "" {
		t.Fatalf("messages 形状不符: path=%s auth=%s ver=%s", gotPath, gotAuth, gotVer)
	}
	// gemini：x-goog-api-key + /v1beta/models/{m}:generateContent
	if _, _, err := ProbeModel(context.Background(), base(provider.ProtocolGemini), acc, "m1", nil, ""); err != nil {
		t.Fatalf("gemini: %v", err)
	}
	if gotPath != "/v1beta/models/m1:generateContent" || gotKey != "sk-test" {
		t.Fatalf("gemini 形状不符: path=%s key=%s", gotPath, gotKey)
	}
}

// TestProbeModelTransportError 传输层失败返回 err 与 status 0（判定保留）。
func TestProbeModelTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler) // 直接断连
	}))
	defer srv.Close()
	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolChat, BaseURL: srv.URL}
	_, body, err := ProbeModel(context.Background(), prov, provider.Account{APIKey: "k"}, "m", nil, "")
	if err == nil {
		t.Fatalf("应返回传输层错误")
	}
	_ = body
}

// auditFakeUpstream 按模型名返回预置（状态码, 响应体）；断言请求体模型名。
func auditFakeUpstream(t *testing.T, behave map[string][2]any, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		*seen = append(*seen, req.Model)
		be, ok := behave[req.Model]
		if !ok {
			t.Errorf("未预期的模型请求: %q", req.Model)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(be[0].(int))
		_, _ = w.Write([]byte(be[1].(string)))
	}))
}

func TestAuditGroupsRemoval(t *testing.T) {
	var seen []string
	srv := auditFakeUpstream(t, map[string][2]any{
		"ok":   {http.StatusOK, `{}`},
		"bad":  {http.StatusForbidden, `1313 当前订阅套餐暂未开放权限`},
		"rate": {http.StatusTooManyRequests, `rate limited`},
		"err5": {http.StatusInternalServerError, `oops`},
		"bad2": {http.StatusBadRequest, `1113 余额不足或无可用资源包`},
	}, &seen)
	defer srv.Close()

	f := &store.ProvidersFile{
		Providers: []provider.Provider{{
			ID: "p", Protocol: provider.ProtocolChat, BaseURL: srv.URL,
			Accounts: []provider.Account{
				{Name: "main", APIKey: "k1"},
				{Name: "seat1", APIKey: "k2"},
				{Name: "oauth", Auth: provider.AuthOAuth}, // OAuth 不实测
				{Name: "off", APIKey: "k3", Disabled: true},
			},
		}},
		Groups: []provider.Group{
			{ID: "g1", Members: []string{"p/ok", "p/bad", "p/rate", "p/err5"}},
			{ID: "g2", Members: []string{"p/bad2@seat1", "p/ok"}},
			{ID: "g3", Members: []string{"p/bad@oauth"}}, // 席位为 OAuth 账号 → 跳过实测，组不动
		},
	}
	probe := func(prov *provider.Provider, acc provider.Account, model string) (int, string, error) {
		return ProbeModel(context.Background(), prov, acc, model, srv.Client().Transport, "")
	}
	rep, changed := AuditGroups(f, "p", probe)
	if !changed {
		t.Fatalf("应发生成员变更")
	}
	if rep.Removed != 2 {
		t.Fatalf("应移除 2 个成员，实际 %d：%+v", rep.Removed, rep.Items)
	}
	got := strings.Join(f.Groups[0].Members, ",")
	if got != "p/ok,p/rate,p/err5" {
		t.Fatalf("g1 成员不符: %s", got)
	}
	if strings.Join(f.Groups[1].Members, ",") != "p/ok" {
		t.Fatalf("g2 席位成员未移除: %v", f.Groups[1].Members)
	}
	if strings.Join(f.Groups[2].Members, ",") != "p/bad@oauth" {
		t.Fatalf("g3 席位跳过组不应改动: %v", f.Groups[2].Members)
	}
	for _, it := range rep.Items {
		if it.Ref == "p/bad@oauth" && it.Skipped == "" {
			t.Fatalf("OAuth 席位成员应跳过实测: %+v", it)
		}
	}
	// 普通成员应挑首个可用账号 main（main 在 off/oauth 之前但顺序为 main,seat1,…）
	for _, it := range rep.Items {
		if it.Ref == "p/bad" && !it.Removed {
			t.Fatalf("p/bad 应移除: %+v", it)
		}
		if it.Ref == "p/rate" && it.Removed {
			t.Fatalf("429 应保留: %+v", it)
		}
	}
}

// TestAuditGroupsEmptyGuard 单成员组全被判定不可用时保守保留（validateProviders
// 要求组至少一个成员，宁可少删）。
func TestAuditGroupsEmptyGuard(t *testing.T) {
	var seen []string
	srv := auditFakeUpstream(t, map[string][2]any{"bad": {http.StatusNotFound, `not found`}}, &seen)
	defer srv.Close()
	f := &store.ProvidersFile{
		Providers: []provider.Provider{{ID: "p", Protocol: provider.ProtocolChat, BaseURL: srv.URL,
			Accounts: []provider.Account{{Name: "a", APIKey: "k"}}}},
		Groups: []provider.Group{{ID: "solo", Members: []string{"p/bad"}}},
	}
	probe := func(prov *provider.Provider, acc provider.Account, model string) (int, string, error) {
		return ProbeModel(context.Background(), prov, acc, model, srv.Client().Transport, "")
	}
	rep, changed := AuditGroups(f, "", probe)
	if changed || rep.Removed != 0 {
		t.Fatalf("空组保底应保留成员: changed=%v removed=%d", changed, rep.Removed)
	}
	if strings.Join(f.Groups[0].Members, ",") != "p/bad" {
		t.Fatalf("成员不应变动: %v", f.Groups[0].Members)
	}
	if !strings.Contains(rep.Items[0].Reason, "保守保留") {
		t.Fatalf("报告应标注保守保留: %+v", rep.Items[0])
	}
}

// TestApplyAuditWithBackup 覆写前留底：写回成功且 backup 目录留有快照。
func TestApplyAuditWithBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	orig := []byte(`{"providers":[{"id":"p","accounts":[{"name":"a","api_key":"k"}]}]}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &store.ProvidersFile{Providers: []provider.Provider{{ID: "p2", Accounts: []provider.Account{{Name: "a", APIKey: "k"}}}}}
	if err := ApplyAuditWithBackup(path, dir, f); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(after), `"p2"`) {
		t.Fatalf("写回失败: err=%v content=%s", err, after)
	}
	ents, err := os.ReadDir(filepath.Join(dir, "backup"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("留底文件应存在且仅 1 份: err=%v n=%d", err, len(ents))
	}
	keep, err := os.ReadFile(filepath.Join(dir, "backup", ents[0].Name()))
	if err != nil || !strings.Contains(string(keep), `"p"`) {
		t.Fatalf("留底应含覆写前内容: err=%v", err)
	}
}

// TestAuditGroupsScope 限定 provider 范围时其他引用不实测。
func TestAuditGroupsScope(t *testing.T) {
	f := &store.ProvidersFile{
		Providers: []provider.Provider{{ID: "p", Protocol: provider.ProtocolChat,
			Accounts: []provider.Account{{Name: "a", APIKey: "k"}}}},
		Groups: []provider.Group{{ID: "g", Members: []string{"p/m", "other/m"}}},
	}
	calls := 0
	rep, changed := AuditGroups(f, "p", func(prov *provider.Provider, acc provider.Account, model string) (int, string, error) {
		calls++
		return 0, "", errors.New("boom")
	})
	// 范围外成员（other/m）不进报告；范围内成员传输层失败 → 保留并说明
	if calls != 1 || changed {
		t.Fatalf("范围外成员不应实测: calls=%d changed=%v", calls, changed)
	}
	if len(rep.Items) != 1 || !strings.Contains(rep.Items[0].Reason, "传输层失败") {
		t.Fatalf("传输层失败成员应保留并说明: %+v", rep.Items)
	}
}
