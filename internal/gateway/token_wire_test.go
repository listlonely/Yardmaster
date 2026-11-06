// 批2 测试：命名令牌（多令牌 · 命名归属）生命周期与鉴权叠加。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/store"
)

// newTokenServer 装配无 legacy 令牌、带数据目录的测试网关。
func newTokenServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	appDir := t.TempDir()
	s := &Server{
		Planner:       NewPlanner(nil, nil),
		Version:       "test",
		AppDir:        appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json"),
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv, appDir
}

func postJSON(t *testing.T, url, path, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func delJSON(t *testing.T, url, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestTokenLifecycle(t *testing.T) {
	s, srv, appDir := newTokenServer(t)

	// 生成：响应含 plain（仅此一次）与脱敏条目
	code, body := postJSON(t, srv.URL, "/api/tokens", `{"name":"Trae · 笔记本"}`, "")
	if code != 200 {
		t.Fatalf("create status = %d, body = %s", code, body)
	}
	var cr struct {
		OK     bool           `json:"ok"`
		Plain  string         `json:"plain"`
		Token  map[string]any `json:"token"`
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal(body, &cr); err != nil {
		t.Fatal(err)
	}
	if !cr.OK || !strings.HasPrefix(cr.Plain, "ym-") || cr.Plain[len(cr.Plain)-4:] != strings.TrimPrefix(cr.Token["mask"].(string), "***") {
		t.Fatalf("生成响应异常: %s", body)
	}

	// 落盘只存 hash：settings.json 不含 plain，含 64 位 hash
	raw, err := os.ReadFile(filepath.Join(appDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cr.Plain) {
		t.Fatal("settings.json 不得包含令牌明文")
	}
	if !strings.Contains(string(raw), `"hash"`) {
		t.Fatal("settings.json 应含命名令牌 hash")
	}

	// plain 命中鉴权（此前 Server 无任何令牌=信任态；生成后即刻生效）
	code, _ = postJSON(t, srv.URL, "/api/quota/probe", "", cr.Plain)
	if code != 200 {
		t.Fatalf("plain 应通过鉴权，got %d", code)
	}
	code, _ = postJSON(t, srv.URL, "/api/quota/probe", "", "ym-wrong")
	if code != 401 {
		t.Fatalf("错误令牌应 401，got %d", code)
	}

	// 管理 API 输出剥除 hash（GET 需携带刚生成的 plain——生成后信任态关闭）
	{
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/config/settings", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+cr.Plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		code = resp.StatusCode
	}
	if code != 200 {
		t.Fatalf("settings GET = %d", code)
	}
	if strings.Contains(string(body), `"hash"`) {
		t.Fatal("settings 脱敏视图不得含 hash")
	}
	var sv struct {
		Tokens []store.TokenEntry `json:"tokens"`
	}
	_ = json.Unmarshal(body, &sv)
	if len(sv.Tokens) != 1 || sv.Tokens[0].Hash != "" || sv.Tokens[0].Name != "Trae · 笔记本" {
		t.Fatalf("脱敏 tokens 异常: %s", body)
	}

	// 轮换：旧 plain 失效、新 plain 生效
	code, body = postJSON(t, srv.URL, "/api/tokens/"+sv.Tokens[0].ID+"/rotate", "", cr.Plain)
	if code != 200 {
		t.Fatalf("rotate status = %d, body = %s", code, body)
	}
	var rr struct {
		Plain string `json:"plain"`
	}
	if err := json.Unmarshal(body, &rr); err != nil {
		t.Fatal(err)
	}
	code, _ = postJSON(t, srv.URL, "/api/quota/probe", "", cr.Plain)
	if code != 401 {
		t.Fatalf("旧 plain 轮换后应 401，got %d", code)
	}
	code, _ = postJSON(t, srv.URL, "/api/quota/probe", "", rr.Plain)
	if code != 200 {
		t.Fatalf("新 plain 应生效，got %d", code)
	}

	// 删除：hash 集清空 → 回到「无令牌信任态」（本机放行，语义正确）
	code, body = delJSON(t, srv.URL, "/api/tokens/"+sv.Tokens[0].ID, rr.Plain)
	if code != 200 {
		t.Fatalf("delete status = %d, body = %s", code, body)
	}
	code, _ = postJSON(t, srv.URL, "/api/quota/probe", "", "")
	if code != 200 {
		t.Fatalf("删光令牌后应回到本机信任态，got %d", code)
	}
	if s.HasTokenHashes() {
		t.Fatal("全部令牌删除后 hash 集应为空")
	}
}

func TestTokenValidationAndBaseline(t *testing.T) {
	// 归属名必填
	_, srv, _ := newTokenServer(t)
	code, body := postJSON(t, srv.URL, "/api/tokens", `{"name":"  "}`, "")
	if code != 400 {
		t.Fatalf("空归属名应 400，got %d: %s", code, body)
	}
	// 不存在的条目 404
	code, _ = postJSON(t, srv.URL, "/api/tokens/tk-xxxx/rotate", "", "")
	if code != 404 {
		t.Fatalf("轮换不存在条目应 404，got %d", code)
	}
	// 安全基线：非回环监听 + 仅命名令牌 → settings PUT 通过
	appDir := t.TempDir()
	s2 := &Server{Planner: NewPlanner(nil, nil), Version: "test", AppDir: appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json")}
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	code, body = postJSON(t, srv2.URL, "/api/tokens", `{"name":"lan"}`, "")
	if code != 200 {
		t.Fatalf("seed token status = %d: %s", code, body)
	}
	var seed struct {
		Plain string `json:"plain"`
	}
	if err := json.Unmarshal(body, &seed); err != nil {
		t.Fatal(err)
	}
	hash := s2.TokenHashList()[0]
	put := `{"listen":"0.0.0.0:8317","tokens":[{"id":"tk-1","name":"lan","hash":"` + hash + `","mask":"***abcd"}]}`
	code, body = doPut(t, srv2.URL, "/api/config/settings", put)
	_ = body
	// doPut 不带头——改用带令牌的请求重发
	if code == 401 {
		req, _ := http.NewRequest(http.MethodPut, srv2.URL+"/api/config/settings", strings.NewReader(put))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+seed.Plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		code, body = resp.StatusCode, b
	}
	if code != 200 {
		t.Fatalf("非回环 + 仅命名令牌应通过安全基线，got %d: %s", code, body)
	}
	if err := ValidateSecurityN("0.0.0.0:8317", "", 2); err != nil {
		t.Fatalf("多枚命名令牌应满足基线: %v", err)
	}
	if err := ValidateSecurityN("0.0.0.0:8317", "", 0); err == nil {
		t.Fatal("无任何令牌的非回环监听应拒绝")
	}
}
