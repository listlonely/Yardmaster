package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yardmaster/internal/provider"
)

func TestProbeAccountChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-ok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m-a"},{"id":"m-b"}]}`))
	}))
	defer srv.Close()

	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolChat, BaseURL: srv.URL}
	models, err := ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "sk-ok"}, "", nil, "")
	if err != nil {
		t.Fatalf("ProbeAccount: %v", err)
	}
	if len(models) != 2 || models[0].ID != "m-a" {
		t.Fatalf("models = %+v", models)
	}

	_, err = ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "sk-bad"}, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
}

func TestProbeAccountMessagesPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") == "" || r.Header.Get("Anthropic-Version") == "" {
			t.Error("缺少 anthropic 认证头")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"}]}`))
	}))
	defer srv.Close()

	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolMessages, BaseURL: srv.URL}
	models, err := ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "sk"}, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "claude-x" {
		t.Fatalf("models = %+v", models)
	}
}

// W4 认证头自动学习：authStyle=bearer 时 Anthropic 协议用 Bearer 而非
// x-api-key。
func TestProbeAccountAuthStyleBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk" || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("bearer 形态错误: auth=%q xapi=%q", r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"}]}`))
	}))
	defer srv.Close()

	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolMessages, BaseURL: srv.URL}
	if _, err := ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "sk"}, "", nil, "bearer"); err != nil {
		t.Fatal(err)
	}
}

func TestProbeWithCustomRoundTripper(t *testing.T) {
	// 验证 ProbeAccount 尊重调用方传入的 RoundTripper（继承链上的
	// 共享 Transport 即由此注入）。真实 CONNECT 代理的端到端验证
	// 在 W3 后续补（需要自实现 CONNECT 假代理）。
	var viaCustom bool
	custom := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		viaCustom = true
		rec := httptest.NewRecorder()
		rec.WriteString(`{"data":[{"id":"m"}]}`)
		return rec.Result(), nil
	})
	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolChat, BaseURL: "http://upstream.invalid"}
	models, err := ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "k"}, "", custom, "")
	if err != nil {
		t.Fatal(err)
	}
	if !viaCustom || len(models) != 1 {
		t.Fatalf("viaCustom=%v models=%+v", viaCustom, models)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResolveProxyRefChain(t *testing.T) {
	cases := []struct {
		account, prov, global string
		want, wantSrc         string
	}{
		{"socks5://a", "http://p", "http://g", "socks5://a", "账号"},
		{"", "http://p", "http://g", "http://p", "Provider"},
		{"", "", "http://g", "http://g", "全局"},
		{"direct", "http://p", "", "direct", "账号（显式直连）"},
		{"", "direct", "http://g", "direct", "Provider（显式直连）"},
		{"", "", "", "", "系统代理/直连"},
	}
	for _, c := range cases {
		got, src := ResolveProxyRef(c.account, c.prov, c.global)
		if got != c.want || src != c.wantSrc {
			t.Fatalf("Resolve(%q,%q,%q) = (%q,%q), want (%q,%q)",
				c.account, c.prov, c.global, got, src, c.want, c.wantSrc)
		}
	}
}

// [2026-11-09-probe-all] openai-responses 与 chat 同源 /models + Bearer。
func TestProbeAccountResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-r" {
			t.Errorf("缺 Bearer 认证")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.2"},{"id":"o4-mini"}]}`))
	}))
	defer srv.Close()

	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolResponses, BaseURL: srv.URL}
	models, err := ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "sk-r"}, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[1].ID != "o4-mini" {
		t.Fatalf("models = %+v", models)
	}
}

// gemini：/v1beta/models + x-goog-api-key；剥 models/ 前缀，仅保留
// generateContent 模型（embedding 等不进目录）。
func TestProbeAccountGemini(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Goog-Api-Key") != "gk-1" {
			t.Errorf("缺 x-goog-api-key 认证头")
		}
		_, _ = w.Write([]byte(`{"models":[
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent","countTokens"]},
			{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},
			{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}
		]}`))
	}))
	defer srv.Close()

	prov := &provider.Provider{ID: "p", Protocol: provider.ProtocolGemini, BaseURL: srv.URL}
	models, err := ProbeAccount(context.Background(), prov,
		provider.Account{Name: "a", APIKey: "gk-1"}, "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if len(ids) != 2 || ids[0] != "gemini-2.5-flash" || ids[1] != "gemini-2.5-pro" {
		t.Fatalf("models = %v（应剥前缀且过滤非对话模型）", ids)
	}
}
