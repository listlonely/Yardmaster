// M2-W1 管理 API 测试：经 HTTP 全链路验证配置读写、脱敏往返、
// 备份留底、fail-closed 与热重载。文件全部落在 t.TempDir（不碰真机）。
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
	"time"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// newConfigServer 装配带数据目录的测试网关：AppDir/ProvidersPath 指向
// 临时目录，Provider 指向真实占位（不发请求）。返回 server 与 httptest。
func newConfigServer(t *testing.T, seed *store.ProvidersFile) (*Server, *httptest.Server) {
	t.Helper()
	appDir := t.TempDir()
	s := &Server{
		Planner:       NewPlanner(nil, nil),
		Version:       "test",
		AppDir:        appDir,
		ProvidersPath: filepath.Join(appDir, "providers.json"),
	}
	if seed != nil {
		if err := store.SaveProvidersFile(s.ProvidersPath, *seed); err != nil {
			t.Fatal(err)
		}
		s.Planner.Reload(seed.Providers, seed.Groups)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func doGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func doPut(t *testing.T, url, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func seedProvidersFile() store.ProvidersFile {
	return store.ProvidersFile{
		Providers: []provider.Provider{{
			ID: "prov", Name: "Test", Protocol: provider.ProtocolChat,
			BaseURL: "https://upstream.test",
			Accounts: []provider.Account{
				{Name: "one", APIKey: "sk-test-1234567890abcd", Status: provider.AccountAvailable},
			},
		}},
	}
}

func TestConfigGetProvidersMasksKeys(t *testing.T) {
	_, srv := newConfigServer(t, &store.ProvidersFile{Providers: []provider.Provider{{
		ID: "prov", Name: "Test", Protocol: provider.ProtocolChat, BaseURL: "https://upstream.test",
		Accounts: []provider.Account{{Name: "one", APIKey: "sk-test-1234567890abcd"}},
	}}})
	st, body := doGet(t, srv.URL+"/api/config/providers")
	if st != http.StatusOK {
		t.Fatalf("GET providers: HTTP %d: %s", st, body)
	}
	if !strings.Contains(string(body), "***abcd") {
		t.Fatalf("GET 输出应包含脱敏密钥 ***abcd: %s", body)
	}
	if strings.Contains(string(body), "sk-test-1234567890abcd") {
		t.Fatalf("GET 输出泄漏了原始密钥: %s", body)
	}
}

// 还原旧代码（keepProviderKeys 不保留脱敏回传）即失败：往返后密钥会
// 被写成 "***abcd" 而非原值。
func TestConfigPutProvidersRoundTripKeepsKey(t *testing.T) {
	seed := seedProvidersFile()
	s, srv := newConfigServer(t, &seed)
	_, body := doGet(t, srv.URL+"/api/config/providers")
	st, _ := doPut(t, srv.URL, "/api/config/providers", string(body))
	if st != http.StatusOK {
		t.Fatalf("PUT 回传: HTTP %d", st)
	}
	var onDisk store.ProvidersFile
	if err := store.LoadJSON(s.ProvidersPath, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := onDisk.Providers[0].Accounts[0].APIKey; got != "sk-test-1234567890abcd" {
		t.Fatalf("脱敏往返后密钥应保留原值，得到 %q", got)
	}
}

func TestConfigPutProvidersAdoptsNewKey(t *testing.T) {
	seed := seedProvidersFile()
	s, srv := newConfigServer(t, &seed)
	doc := seed
	doc.Providers[0].Accounts[0].APIKey = "sk-new-real-key-9999"
	b, _ := json.Marshal(doc)
	st, respBody := doPut(t, srv.URL, "/api/config/providers", string(b))
	if st != http.StatusOK {
		t.Fatalf("PUT 新密钥: HTTP %d: %s", st, respBody)
	}
	var onDisk store.ProvidersFile
	if err := store.LoadJSON(s.ProvidersPath, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := onDisk.Providers[0].Accounts[0].APIKey; got != "sk-new-real-key-9999" {
		t.Fatalf("新密钥应被采纳，得到 %q", got)
	}
}

func TestConfigPutRejectsUnknownFieldAndLeavesFileUntouched(t *testing.T) {
	seed := seedProvidersFile()
	s, srv := newConfigServer(t, &seed)
	st, body := doPut(t, srv.URL, "/api/config/settings", `{"bogus_field":1}`)
	if st != http.StatusBadRequest {
		t.Fatalf("未知字段应 400，得到 HTTP %d: %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(s.AppDir, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("被拒请求不得产生配置文件: %v", err)
	}
}

func TestConfigPutSettingsWritesBackupAndFile(t *testing.T) {
	s, srv := newConfigServer(t, nil)
	settingsPath := filepath.Join(s.AppDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"global_proxy":"http://old:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, body := doPut(t, srv.URL, "/api/config/settings",
		`{"global_proxy":"http://new:2","l1_classifier":"prov/m","l1_timeout_ms":300}`)
	if st != http.StatusOK {
		t.Fatalf("PUT settings: HTTP %d: %s", st, body)
	}
	var cur store.Settings
	if err := store.LoadJSON(settingsPath, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.GlobalProxy != "http://new:2" || cur.L1Classifier != "prov/m" {
		t.Fatalf("设置未落盘: %+v", cur)
	}
	matches, _ := filepath.Glob(filepath.Join(s.AppDir, "backup", "settings-*.json"))
	if len(matches) != 1 {
		t.Fatalf("写前应留 1 份备份，实际 %d", len(matches))
	}
	snap, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(snap), "http://old:1") {
		t.Fatalf("备份应是旧值快照: %s", snap)
	}
}

func TestConfigPutRulesInvalidRegexRejected(t *testing.T) {
	s, srv := newConfigServer(t, nil)
	st, body := doPut(t, srv.URL, "/api/config/rules",
		`{"rules":[{"id":"r1","target":"prov/m","regex":"([bad"}]}`)
	if st != http.StatusBadRequest {
		t.Fatalf("非法正则应 400，得到 HTTP %d: %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(s.AppDir, "rules.json")); !os.IsNotExist(err) {
		t.Fatalf("校验失败不得落盘: %v", err)
	}
}

func TestConfigGetPutUnknownName404(t *testing.T) {
	_, srv := newConfigServer(t, nil)
	if st, _ := doGet(t, srv.URL+"/api/config/bogus"); st != http.StatusNotFound {
		t.Fatalf("GET 未知配置名应 404，得到 %d", st)
	}
	if st, _ := doPut(t, srv.URL, "/api/config/bogus", `{}`); st != http.StatusNotFound {
		t.Fatalf("PUT 未知配置名应 404，得到 %d", st)
	}
}

// 还原旧代码（Reload 不剪枝已删除账号的休眠键）即失败。
func TestPlannerReloadPrunesStateOfRemovedAccounts(t *testing.T) {
	prov := chatProvider("https://upstream.test")
	p := NewPlanner([]provider.Provider{prov}, nil)
	p.Rest(Candidate{Provider: &prov, Account: prov.Accounts[0]}, 5*time.Minute) // one
	p.Rest(Candidate{Provider: &prov, Account: prov.Accounts[1]}, 5*time.Minute) // two
	if _, ok := p.Resting()["prov/one"]; !ok {
		t.Fatal("前置：休眠键应存在")
	}
	// 新配置移除了账号 one（只留 two）：one 的休眠键应被剪枝，two 保留
	newProv := chatProvider("https://upstream.test")
	newProv.Accounts = newProv.Accounts[1:]
	p.Reload([]provider.Provider{newProv}, nil)
	if _, ok := p.Resting()["prov/one"]; ok {
		t.Fatal("Reload 后已删除账号的休眠键应被剪枝")
	}
	if _, ok := p.Resting()["prov/two"]; !ok {
		t.Fatal("未删除账号的休眠键应保留")
	}
}

func TestReloadAppliesFileChangesAndFailsClosedOnCorrupt(t *testing.T) {
	seed := seedProvidersFile()
	s, srv := newConfigServer(t, &seed)
	// 改文件：加一个 provider，重载后 /api/state 应可见
	seed.Providers = append(seed.Providers, provider.Provider{
		ID: "b", Name: "B", Protocol: provider.ProtocolChat, BaseURL: "https://b.test",
		Accounts: []provider.Account{{Name: "a1", APIKey: "k"}},
	})
	if err := store.SaveProvidersFile(s.ProvidersPath, seed); err != nil {
		t.Fatal(err)
	}
	st, body := doGet(t, srv.URL+"/api/state")
	if st != http.StatusOK {
		t.Fatal(string(body))
	}
	if strings.Contains(string(body), `"id":"b"`) {
		t.Fatal("重载前新 provider 不应可见")
	}
	resp, err := http.Post(srv.URL+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/reload: HTTP %d: %s", resp.StatusCode, b)
	}
	_, body = doGet(t, srv.URL+"/api/state")
	if !strings.Contains(string(body), `"id":"b"`) {
		t.Fatalf("重载后新 provider 应可见: %s", body)
	}
	// 损坏文件：fail-closed，409 且保留旧快照
	if err := os.WriteFile(s.ProvidersPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Post(srv.URL+"/api/reload", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("损坏文件重载应 409，得到 HTTP %d: %s", resp.StatusCode, b)
	}
	_, body = doGet(t, srv.URL+"/api/state")
	if !strings.Contains(string(body), `"id":"b"`) {
		t.Fatalf("重载失败后应保留旧快照: %s", body)
	}
}
