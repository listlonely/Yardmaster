package credential

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRefreshSuccess(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600,"scope":"a b"}`))
	}))
	defer srv.Close()

	spec := RefreshSpec{TokenURL: srv.URL, ClientID: "cid"}
	tr, err := Refresh(context.Background(), srv.Client(), spec, "rt-old")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tr.AccessToken != "at-new" || tr.RefreshToken != "rt-new" || tr.ExpiresIn != 3600 {
		t.Fatalf("响应: %+v", tr)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "rt-old" || gotForm.Get("client_id") != "cid" {
		t.Fatalf("表单: %v", gotForm)
	}
	// 合并：token/过期以新值为准，refresh_token 轮换生效
	now := time.Unix(1893456000, 0)
	next := MergeRefresh(OAuth{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1, Extra: map[string]string{"account_id": "a"}}, tr, now)
	if next.AccessToken != "at-new" || next.RefreshToken != "rt-new" ||
		next.ExpiresAt != now.Add(time.Hour).Unix() || next.Extra["account_id"] != "a" {
		t.Fatalf("合并: %+v", next)
	}
}

func TestRefreshKeepsRefreshTokenWhenNotRotated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at-new","expires_in":60}`))
	}))
	defer srv.Close()
	tr, err := Refresh(context.Background(), srv.Client(), RefreshSpec{TokenURL: srv.URL, ClientID: "c"}, "rt-old")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	next := MergeRefresh(OAuth{RefreshToken: "rt-old"}, tr, time.Now())
	if next.RefreshToken != "rt-old" {
		t.Fatalf("不轮换时应保留旧 refresh_token: %+v", next)
	}
}

func TestRefreshFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"invalid_grant", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}, "刷新被拒绝"},
		{"非 JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not-json`))
		}, "解析失败"},
		{"缺 access_token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"expires_in":60}`))
		}, "刷新被拒绝"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := Refresh(context.Background(), srv.Client(), RefreshSpec{TokenURL: srv.URL, ClientID: "c"}, "rt")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want 含 %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "at-new") || strings.Contains(err.Error(), "sk-") {
				t.Fatalf("错误信息不得含 token: %v", err)
			}
		})
	}
}

func TestRefreshInputValidation(t *testing.T) {
	if _, err := Refresh(context.Background(), nil, RefreshSpec{ClientID: "c"}, "rt"); err == nil {
		t.Fatal("缺 token_url 应拒绝")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	if _, err := Refresh(context.Background(), srv.Client(), RefreshSpec{TokenURL: srv.URL}, ""); err == nil {
		t.Fatal("缺 refresh_token 应拒绝")
	}
	if _, ok := RefreshSpecFor("claude-code"); !ok {
		t.Fatal("claude-code 刷新规格应内置")
	}
	if _, ok := RefreshSpecFor("codex"); !ok {
		t.Fatal("codex 刷新规格应内置")
	}
	if _, ok := RefreshSpecFor("gemini-cli"); !ok {
		t.Fatal("gemini-cli 刷新规格应内置")
	}
	if _, ok := RefreshSpecFor("nope"); ok {
		t.Fatal("未知格式不应有刷新规格")
	}
}
