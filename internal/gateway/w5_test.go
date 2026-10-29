// M2-W5 行为测试：访问令牌鉴权、安全基线校验、settings 令牌脱敏往返。
// 还原旧代码即失败的断言见各用例注释。
package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/store"
)

// 还原旧代码（非回环监听不强制令牌）即失败。
func TestValidateSecurity(t *testing.T) {
	cases := []struct {
		listen, token string
		ok            bool
	}{
		{"127.0.0.1:8317", "", true},  // 默认本机
		{"localhost:8317", "", true},  // localhost 视为回环
		{"", "", true},                // 空 host 视为回环
		{"[::1]:8317", "", true},      // IPv6 回环
		{"0.0.0.0:8317", "", false},   // 局域网共享无令牌：拒绝
		{"192.168.1.5:8317", "", false},
		{"0.0.0.0:8317", "secret", true}, // 带令牌放行
	}
	for _, c := range cases {
		err := ValidateSecurity(c.listen, c.token)
		if c.ok && err != nil {
			t.Errorf("ValidateSecurity(%q,%q) 应通过: %v", c.listen, c.token, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidateSecurity(%q,%q) 应拒绝", c.listen, c.token)
		}
	}
}

// 还原旧代码（无鉴权中间件）即失败：token 态下无令牌请求将得 200。
func TestAuthMiddleware(t *testing.T) {
	appDir := t.TempDir()
	s := &Server{
		Planner: NewPlanner(nil, nil), Version: "test",
		AppDir: appDir, ProvidersPath: filepath.Join(appDir, "providers.json"),
		Token: "secret-1234",
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// 免鉴权：健康探针与静态壳
	for _, p := range []string{"/healthz", "/", "/web/app.js"} {
		rsp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		rsp.Body.Close()
		if rsp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s 无令牌应放行（静态壳/探针），得到 %d", p, rsp.StatusCode)
		}
	}
	// 受保护端点：无令牌 401
	rsp, _ := http.Get(srv.URL + "/api/state")
	b, _ := io.ReadAll(rsp.Body)
	rsp.Body.Close()
	if rsp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌 /api/state 应 401，得到 %d: %s", rsp.StatusCode, b)
	}
	// 三种携带方式均放行（与 callerKeyOf 同源）
	for name, hdr := range map[string][]string{
		"X-Gw-Token":     {"secret-1234"},
		"Authorization":  {"Bearer secret-1234"},
		"X-Api-Key":      {"secret-1234"},
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
		req.Header.Set(name, hdr[0])
		rsp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rsp.Body.Close()
		if rsp.StatusCode != http.StatusOK {
			t.Fatalf("%s 携带正确令牌应放行，得到 %d", name, rsp.StatusCode)
		}
	}
	// 错误令牌 401
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.Header.Set("X-Gw-Token", "wrong")
	rsp, _ = http.DefaultClient.Do(req)
	rsp.Body.Close()
	if rsp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误令牌应 401，得到 %d", rsp.StatusCode)
	}
	// 推断端点同样受保护
	rsp2, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	rsp2.Body.Close()
	if rsp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌 /v1 应 401，得到 %d", rsp2.StatusCode)
	}
}

// 还原旧代码（token 态）即失败：空 token 服务器全放行。
func TestAuthMiddlewareEmptyTokenOpen(t *testing.T) {
	_, srv := newConfigServer(t, nil)
	rsp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		t.Fatalf("空 token（本机信任态）应放行，得到 %d", rsp.StatusCode)
	}
}

// 还原旧代码（settings PUT 不校验安全基线/令牌脱敏往返破坏令牌）即失败。
func TestSettingsPutTokenSemantics(t *testing.T) {
	s, srv := newConfigServer(t, nil)

	// 非回环 + 无令牌：400 且不落盘
	st, body := doPut(t, srv.URL, "/api/config/settings",
		`{"listen":"0.0.0.0:8317"}`)
	if st != http.StatusBadRequest {
		t.Fatalf("非回环无令牌应 400，得到 %d: %s", st, body)
	}
	if _, err := store.LoadSettings(store.Paths{AppDir: s.AppDir}); err == nil {
		t.Fatalf("被拒请求不得落盘（settings.json 不应存在）")
	}

	// 非回环 + 令牌：放行
	st, body = doPut(t, srv.URL, "/api/config/settings",
		`{"listen":"0.0.0.0:8317","token":"my-secret-9999"}`)
	if st != http.StatusOK {
		t.Fatalf("非回环带令牌应 200，得到 %d: %s", st, body)
	}
	// GET 脱敏
	_, gb := doGet(t, srv.URL+"/api/config/settings")
	if !strings.Contains(string(gb), "***9999") || strings.Contains(string(gb), "my-secret-9999") {
		t.Fatalf("settings GET 应脱敏令牌: %s", gb)
	}
	// 脱敏形回传（带同形 token + 空显示）→ 文件令牌保留
	st, _ = doPut(t, srv.URL, "/api/config/settings",
		`{"global_proxy":"","l1_classifier":"","l1_timeout_ms":0,"listen":"0.0.0.0:8317","token":"***9999"}`)
	if st != http.StatusOK {
		t.Fatalf("脱敏往返应 200，得到 %d", st)
	}
	cur, _ := store.LoadSettings(store.Paths{AppDir: s.AppDir})
	if cur.Token != "my-secret-9999" {
		t.Fatalf("脱敏往返后令牌应保留原值，得到 %q", cur.Token)
	}
	if cur.Listen != "0.0.0.0:8317" {
		t.Fatalf("listen 应落盘: %+v", cur)
	}
}
