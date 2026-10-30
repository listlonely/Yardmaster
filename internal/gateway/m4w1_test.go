// M4-W1 测试：/v1/responses 路由骨架（透传 + fail-closed 边界）、
// protocol 字段新枚举校验、写响应即状态（api.md 契约）。
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// ---------- /v1/responses 骨架 ----------

func responsesProvider(baseURL string) provider.Provider {
	p := chatProvider(baseURL)
	p.Protocol = provider.ProtocolResponses
	return p
}

func TestResponsesPassthroughNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`
	})
	gw := newGateway(t, responsesProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/responses",
		`{"model":"prov/up-m","input":"hello","stream":false}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	calls := up.calls()
	if len(calls) != 1 {
		t.Fatalf("上游应收到 1 次请求， got %d", len(calls))
	}
	if calls[0].path != "/responses" {
		t.Fatalf("上游路径 = %q, want /responses", calls[0].path)
	}
	// 透传路径模型名改写（W3 冒烟教训）：上游收到的必须是真实模型名。
	if !strings.Contains(calls[0].body, `"model":"up-m"`) {
		t.Fatalf("上游请求未改写模型名: %s", calls[0].body)
	}
	// Bearer 认证（Responses 走 OpenAI 形态）。
	if calls[0].auth != "Bearer sk-one" {
		t.Fatalf("Authorization = %q", calls[0].auth)
	}
	var rr ResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if rr.ID != "resp_1" || rr.Usage == nil || rr.Usage.OutputTokens != 2 {
		t.Fatalf("透传响应被篡改: %+v", rr)
	}
}

func TestResponsesBadJSONIs400(t *testing.T) {
	gw := newGateway(t, responsesProvider("http://unused.invalid"))
	resp := post(t, gw.URL, "/v1/responses", `{nope`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// W2 起：Responses 客户端 × Chat 上游 → 请求翻译为 chat 形态，响应
// 翻译回 Responses 形态（M4-W2 交付，原 W1 fail-closed 断言退役）。
func TestResponsesToChatUpstreamTranslates(t *testing.T) {
	up := newFakeUpstream(t, func(rec recordedReq, _ int) (int, string, string) {
		if rec.path != "/chat/completions" {
			t.Errorf("上游路径 = %q, want /chat/completions", rec.path)
		}
		// 请求应为 chat 形态：instructions→system、input→messages、模型名已改写
		if !strings.Contains(rec.body, `"role":"system"`) || !strings.Contains(rec.body, `"model":"up-m"`) {
			t.Errorf("chat 请求翻译异常: %s", rec.body)
		}
		return http.StatusOK, "application/json",
			`{"id":"c1","object":"chat.completion","model":"up-m","choices":[{"index":0,"message":{"role":"assistant","content":"yo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	})
	gw := newGateway(t, chatProvider(up.srv.URL))

	resp := post(t, gw.URL, "/v1/responses", `{"model":"prov/up-m","instructions":"be brief","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var rr ResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("解析 responses 响应: %v", err)
	}
	if rr.Object != "response" || rr.Status != "completed" || rr.Usage == nil || rr.Usage.InputTokens != 3 {
		t.Fatalf("responses 响应翻译异常: %+v", rr)
	}
	if got := responsesOutputText(&rr); got != "yo" {
		t.Fatalf("output 文本 = %q", got)
	}
}

func responsesOutputText(rr *ResponsesResponse) string {
	for _, item := range rr.Output {
		if item.Type == "message" {
			return respContentText(item.Content)
		}
	}
	return ""
}

// （W1 的 gemini fail-closed 测试随 W3 上游链路交付退役，
// 由 m4w3_test.go 的 TestChatClientToGeminiUpstream 等正向用例覆盖。）

// ---------- input 统计（规则引擎数据源） ----------

func TestResponsesInputStats(t *testing.T) {
	rr := &ResponsesRequest{
		Instructions: "be brief",
		Input: []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "第一句"},
			}},
			map[string]any{"role": "assistant", "content": "回答"},
			map[string]any{"type": "function_call", "name": "ls", "arguments": `{"path":"."}`},
			map[string]any{"type": "function_call_output", "output": "file list"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "最后一句"},
			}},
		},
	}
	lastUser, est := inputStats(rr)
	if lastUser != "最后一句" {
		t.Fatalf("lastUser = %q", lastUser)
	}
	if est == 0 {
		t.Fatal("长度估计不应为 0")
	}
	// 字符串 input 形态：整段即最后用户消息。
	rs := &ResponsesRequest{Input: "plain string"}
	if lu, _ := inputStats(rs); lu != "plain string" {
		t.Fatalf("string input lastUser = %q", lu)
	}
}

func TestResponsesInputTextJoinsToolPayloads(t *testing.T) {
	in := []any{
		map[string]any{"type": "function_call", "arguments": `{"a":1}`},
		map[string]any{"type": "function_call_output", "output": "ok"},
	}
	if s := responsesInputText(in); !strings.Contains(s, `{"a":1}`) || !strings.Contains(s, "ok") {
		t.Fatalf("工具负载应计入长度估计: %q", s)
	}
}

// ---------- 写响应即状态（api.md 契约） ----------

func newAdminGateway(t *testing.T, prov store.ProvidersFile) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "\\providers.json"
	if err := store.SaveJSON(path, prov); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Planner:      NewPlanner(prov.Providers, prov.Groups),
		Version:      "test",
		AppDir:       dir,
		ProvidersPath: path,
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, dir
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return resp.StatusCode, m
}

// PUT providers 写响应包：{ok, effect, config(脱敏), state}；落盘文件
// 保留原 key（防泄漏双断言：响应掩码 + 文件原值）。
func TestPutProvidersWriteResultContract(t *testing.T) {
	gw, dir := newAdminGateway(t, store.ProvidersFile{Providers: []provider.Provider{
		{ID: "p1", Name: "P1", Protocol: provider.ProtocolChat, BaseURL: "http://up",
			Accounts: []provider.Account{{Name: "main", APIKey: "sk-secret123", Status: provider.AccountAvailable}}},
	}})

	putBody := `{"providers":[{"id":"p1","name":"P1","protocol":"openai-chat","base_url":"http://up","accounts":[{"name":"main","api_key":"***t123"}]}]}`
	st, m := doJSON(t, "PUT", gw.URL+"/api/config/providers", putBody)
	if st != http.StatusOK {
		t.Fatalf("status = %d: %v", st, m)
	}
	if m["ok"] != true || m["effect"] != "immediate" {
		t.Fatalf("ok/effect 异常: %v", m)
	}
	cfg, _ := m["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("写响应应携带 config: %v", m)
	}
	provs, _ := cfg["providers"].([]any)
	first, _ := provs[0].(map[string]any)
	accs, _ := first["accounts"].([]any)
	acc, _ := accs[0].(map[string]any)
	if acc["api_key"] != "***t123" {
		t.Fatalf("config 视图 api_key 应为掩码形: %v", acc["api_key"])
	}
	state, _ := m["state"].(map[string]any)
	if state["version"] != "test" {
		t.Fatalf("state 应与 GET /api/state 同构: %v", state)
	}
	if _, ok := state["providers"].([]any); !ok {
		t.Fatalf("state 应含 providers 快照: %v", state)
	}
	// 落盘文件仍是原值（用户值优先 + 掩码往返保留）。
	var disk store.ProvidersFile
	if err := store.LoadJSON(dir+"\\providers.json", &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Providers[0].Accounts[0].APIKey != "sk-secret123" {
		t.Fatalf("落盘 key 被破坏: %q", disk.Providers[0].Accounts[0].APIKey)
	}
	// state 不泄漏明文 key（值拷贝脱敏，防穿透运行态）。
	if strings.Contains(fmt.Sprint(state), "sk-secret123") {
		t.Fatal("state 泄漏明文 key")
	}
}

// reload 写响应包：保留 providers 计数 legacy 字段 + 携带 state。
func TestReloadWriteResultContract(t *testing.T) {
	gw, _ := newAdminGateway(t, store.ProvidersFile{Providers: []provider.Provider{
		{ID: "p1", Name: "P1", Protocol: provider.ProtocolChat, BaseURL: "http://up",
			Accounts: []provider.Account{{Name: "main", APIKey: "sk-x", Status: provider.AccountAvailable}}},
	}})
	st, m := doJSON(t, "POST", gw.URL+"/api/reload", "")
	if st != http.StatusOK {
		t.Fatalf("status = %d", st)
	}
	if m["providers"] != float64(1) {
		t.Fatalf("providers 计数应保留: %v", m)
	}
	if _, ok := m["state"].(map[string]any); !ok {
		t.Fatalf("reload 响应应携带 state: %v", m)
	}
}

// 新协议枚举经管理 API 写入即生效（validateProviders 放行）。
func TestPutProvidersAcceptsNewProtocols(t *testing.T) {
	gw, dir := newAdminGateway(t, store.ProvidersFile{})
	body := `{"providers":[` +
		`{"id":"rx","name":"RX","protocol":"openai-responses","base_url":"http://up","accounts":[{"name":"main","api_key":"k"}]},` +
		`{"id":"gm","name":"GM","protocol":"gemini","base_url":"http://up","accounts":[{"name":"main","api_key":"k"}]}]}`
	st, m := doJSON(t, "PUT", gw.URL+"/api/config/providers", body)
	if st != http.StatusOK {
		t.Fatalf("新协议应可写入: %d %v", st, m)
	}
	var disk store.ProvidersFile
	if err := store.LoadJSON(dir+"\\providers.json", &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Providers[0].Protocol != provider.ProtocolResponses || disk.Providers[1].Protocol != provider.ProtocolGemini {
		t.Fatalf("落盘协议异常: %v / %v", disk.Providers[0].Protocol, disk.Providers[1].Protocol)
	}
}

// readBody 已由 m4w2_test.go 的 readAll 取代（Gemini fail-closed 用例退役）。
