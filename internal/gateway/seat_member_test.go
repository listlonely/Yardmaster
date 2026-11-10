// 批1 落地测试：成员账号席位（provider/model@account）与插件声明意图。
package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yardmaster/internal/classify"
	"yardmaster/internal/plugin"
	"yardmaster/internal/provider"
)

func classifyBuiltin() []string { return classify.BuiltinIntents }

// ---- 成员账号席位：校验（写路径） ----

func TestValidateMemberSeat(t *testing.T) {
	provs := twoProviders()
	groups := map[string]bool{"g": true}
	cases := []struct {
		m     string
		valid bool
		sub   string // 非法时错误信息须含的片段
	}{
		{"prov/up-m", true, ""},
		{"prov", true, ""},
		{"prov/up-m@a2", true, ""},
		{"group/g", true, ""},
		{"prov/up-m@ghost", false, "账号不存在"},
		{"prov/up-m@", false, "席位名为空"},
		{"ghost/up-m@a1", false, "provider 不存在"},
		{"prov/up-m2@a1", true, ""}, // 模型名不做清单校验（与 provider/model 同口径）
	}
	for _, c := range cases {
		err := validateMember(c.m, provs, groups)
		if c.valid != (err == nil) {
			t.Fatalf("validateMember(%q) = %v, 期望合法=%v", c.m, err, c.valid)
		}
		if !c.valid && !strings.Contains(err.Error(), c.sub) {
			t.Fatalf("validateMember(%q) 错误 %q 应含 %q", c.m, err, c.sub)
		}
	}
}

// ---- 成员账号席位：计划展开（席位过滤） ----

func TestPlanSeatMemberFiltersAccount(t *testing.T) {
	provs := twoProviders()
	g := provider.Group{ID: "g", Members: []string{"prov/up-m@a2", "prov-b/up-m"}}
	p := NewPlanner(provs, []provider.Group{g})
	cands, err := p.Plan("group/g", "")
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	if len(got) != 3 {
		t.Fatalf("席位成员应只展开 prov/a2 + prov-b 全部账号，got %v", got)
	}
	if got[0] != "prov/a2|up-m" {
		t.Fatalf("首位应是席位候选 prov/a2，got %v", got)
	}
}

func TestPlanSeatMemberMissingAccountYieldsEmpty(t *testing.T) {
	provs := twoProviders()
	g := provider.Group{ID: "g", Members: []string{"prov/up-m@ghost"}}
	p := NewPlanner(provs, []provider.Group{g})
	cands, err := p.Plan("group/g", "")
	if err != nil {
		t.Fatalf("账号席位悬空是动态状态，不应报错: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("席位账号不存在应返回空候选，got %v", candKeys(cands))
	}
}

func TestPlanDirectTargetSeat(t *testing.T) {
	provs := twoProviders()
	p := NewPlanner(provs, nil)
	cands, err := p.Plan("prov/up-m@a2", "") // 规则直连目标同支持席位
	if err != nil {
		t.Fatal(err)
	}
	got := candKeys(cands)
	if len(got) != 1 || got[0] != "prov/a2|up-m" {
		t.Fatalf("直连席位应仅 prov/a2，got %v", got)
	}
}

// ---- 插件声明意图：校验 + 词表并入 ----

func TestPluginValidateIntentsRequireCapability(t *testing.T) {
	base := plugin.Spec{ID: "x", Command: []string{"x"}, Capabilities: []string{plugin.CapClassify},
		Intents: []plugin.IntentRef{{ID: "sre/oncall", Label: "值班响应"}}}
	if err := plugin.Validate(plugin.File{Plugins: []plugin.Spec{base}}); err != nil {
		t.Fatalf("classify 插件声明意图应合法: %v", err)
	}
	bad := base
	bad.Capabilities = []string{plugin.CapEvents}
	err := plugin.Validate(plugin.File{Plugins: []plugin.Spec{bad}})
	if err == nil || !strings.Contains(err.Error(), "classify.intent") {
		t.Fatalf("无 classify 能力声明意图应拒收: %v", err)
	}
	noID := base
	noID.Intents = []plugin.IntentRef{{Label: "无 id"}}
	if err := plugin.Validate(plugin.File{Plugins: []plugin.Spec{noID}}); err == nil {
		t.Fatal("意图 id 为空应拒收")
	}
	oauthCap := plugin.Spec{ID: "y", Command: []string{"y"}, Capabilities: []string{plugin.CapOAuth}}
	if err := plugin.Validate(plugin.File{Plugins: []plugin.Spec{oauthCap}}); err != nil {
		t.Fatalf("provider.oauth 能力应合法: %v", err)
	}
}

func TestVocabularyIncludesPluginIntents(t *testing.T) {
	c := NewIntentChain(t.TempDir()+"/intents.json", "", 0, nil)
	if got := c.Vocabulary(); len(got) != len(classifyBuiltin()) {
		t.Fatalf("初始词表应仅内置: %v", got)
	}
	c.WithPluginIntents(func() []plugin.IntentRef {
		return []plugin.IntentRef{{ID: "sre/oncall"}, {ID: "team/incident"}}
	})
	got := c.Vocabulary()
	has := map[string]bool{}
	for _, v := range got {
		has[v] = true
	}
	if !has["sre/oncall"] || !has["team/incident"] || !has["coding"] {
		t.Fatalf("词表应并入插件声明意图: %v", got)
	}
}

// ---- POST /api/provider/probe：假上游 /models 全链路 ----

func TestProviderProbeEndpoint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m-a"},{"id":"m-b"},{"id":""}]}`))
	}))
	t.Cleanup(up.Close)

	s := &Server{Planner: NewPlanner(nil, nil), Version: "test"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	body := `{"protocol":"openai-chat","base_url":"` + up.URL + `","api_key":"sk-probe"}`
	resp, err := http.Post(srv.URL+"/api/provider/probe", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	var out struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(out.Models) != 2 || out.Models[0] != "m-a" || out.Models[1] != "m-b" {
		t.Fatalf("探测结果异常: %+v", out)
	}

	// 非法协议 → 400
	bad := `{"protocol":"gemini","base_url":"https://x","api_key":"k"}`
	resp2, err := http.Post(srv.URL+"/api/provider/probe", "application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("gemini 协议应 400，got %d", resp2.StatusCode)
	}
}
