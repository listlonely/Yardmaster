// M7-W1 models.dev 单价同步——解析容错（string/number 成本、无成本、
// 坏 provider）、合并纪律（人工价保护/-force/冲突字典序取胜）、
// 管理 API 写路径（fail-closed 读取+备份审计）。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/store"
)

// m7w1Catalog 覆盖目录解析全部分支：number/string 成本、无 cost 对象、
// 负成本、缺 output、空 provider、双 provider 同名模型（冲突）。
const m7w1Catalog = `{
  "anthropic": {"id":"anthropic","models":{
    "claude-test":{"id":"claude-test","name":"Claude Test","cost":{"input":3,"output":15,"cache_read":0.3}},
    "dup-model":{"id":"dup-model","cost":{"input":3,"output":15}},
    "claude-nocost":{"id":"claude-nocost","name":"NoCost"}
  }},
  "openai": {"id":"openai","models":{
    "gpt-test":{"id":"gpt-test","name":"GPT Test","cost":{"input":2.5,"output":"10","cache_read":"1.25"}},
    "gpt-neg":{"id":"gpt-neg","cost":{"input":-1,"output":2}},
    "dup-model":{"id":"dup-model","cost":{"input":9,"output":9}}
  }},
  "empty-provider": {"id":"empty-provider"}
}`

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseModelsDevShapes(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "catalog.json")
	mustWriteFile(t, fixture, m7w1Catalog)
	cat, err := LoadModelsDev(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Providers) != 2 {
		t.Fatalf("应保留 2 个有模型成本的 provider: %d", len(cat.Providers))
	}
	if cat.NoPrice != 2 { // gpt-neg（负成本）/claude-nocost（无 cost 对象）
		t.Fatalf("无成本计数: %d", cat.NoPrice)
	}
	// string 形态成本兼容；cache_read 缺省 0。
	gpt := cat.Providers["openai"]["gpt-test"]
	if gpt.Input != 2.5 || gpt.Output != 10 || gpt.CacheRead != 1.25 {
		t.Fatalf("string 成本解析: %+v", gpt)
	}
	if len(cat.Notes) == 0 || !strings.Contains(cat.Notes[0], "empty-provider") {
		t.Fatalf("坏 provider 留痕: %v", cat.Notes)
	}
}

func TestSyncPricingMergeRules(t *testing.T) {
	cat, err := LoadModelsDev(mustFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cur := PricingFile{Prices: []PriceEntry{
		{Model: "claude-test", InputPerM: 99, OutputPerM: 99},                 // 人工价：保护
		{Model: "gpt-old", InputPerM: 1, OutputPerM: 2, Source: "models.dev"}, // 旧同步条目：刷新
		{Model: "prov-x/claude-test", InputPerM: 5, OutputPerM: 5},            // 精确键：同步不触碰
	}}
	merged, res := SyncPricing(cat, cur, false, "", now)

	// 人工价保护（claude-test 目录有条目但被保护）。
	if !contains(res.Skipped, func(s string) bool { return strings.HasPrefix(s, "claude-test（人工价保护") }) {
		t.Fatalf("人工价应保护跳过: %v", res.Skipped)
	}
	if got := findEntry(merged, "claude-test"); got.InputPerM != 99 {
		t.Fatalf("人工价不得被改: %+v", got)
	}
	// 精确键条目不被同步触碰。
	if got := findEntry(merged, "prov-x/claude-test"); got.InputPerM != 5 || got.Source != "" {
		t.Fatalf("精确键应原样: %+v", got)
	}
	// 新增：来源/时刻/缓存价齐备。
	added := findEntry(merged, "gpt-test")
	if added.Source != "models.dev" || added.SyncedAt == "" || added.CachedInputPerM != 1.25 || added.InputPerM != 2.5 || added.OutputPerM != 10 {
		t.Fatalf("新增条目形态: %+v", added)
	}
	if !contains(res.Added, func(s string) bool { return s == "gpt-test" }) {
		t.Fatalf("新增清单: %v", res.Added)
	}
	// 冲突：anthropic（字典序先）胜出，openai 的留痕。
	if got := findEntry(merged, "dup-model"); got.InputPerM != 3 {
		t.Fatalf("冲突应保留字典序首个 provider: %+v", got)
	}
	if !contains(res.Skipped, func(s string) bool { return strings.Contains(s, "多 provider 冲突") }) {
		t.Fatalf("冲突应留痕: %v", res.Skipped)
	}
	if res.Models != 4 || res.Providers != 2 || res.NoPrice != 2 {
		t.Fatalf("计数: %+v", res)
	}
	// cur 不被修改。
	if findEntry(cur, "claude-test").InputPerM != 99 || len(cur.Prices) != 3 {
		t.Fatal("入参单价表不得被修改")
	}
	// onlyProvider 过滤：仅 openai → 新增含 gpt-test、不含 anthropic 系。
	merged2, res2 := SyncPricing(cat, cur, false, "openai", now)
	if !contains(res2.Added, func(s string) bool { return s == "gpt-test" }) {
		t.Fatalf("过滤后应含 gpt-test: %v", res2.Added)
	}
	if contains(res2.Added, func(s string) bool { return s == "claude-test" }) ||
		findEntry(merged2, "dup-model").InputPerM != 9 {
		t.Fatalf("过滤后不应产出 anthropic 系条目: %v / %+v", res2.Added, findEntry(merged2, "dup-model"))
	}
}

func TestSyncPricingForce(t *testing.T) {
	cat, err := LoadModelsDev(mustFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cur := PricingFile{Prices: []PriceEntry{{Model: "claude-test", InputPerM: 99, OutputPerM: 99}}}
	merged, res := SyncPricing(cat, cur, true, "", now)
	if got := findEntry(merged, "claude-test"); got.InputPerM != 3 || got.Source != "models.dev" || got.SyncedAt == "" {
		t.Fatalf("-force 应覆盖人工价: %+v", got)
	}
	if !contains(res.Overwritten, func(s string) bool { return s == "claude-test" }) {
		t.Fatalf("覆盖清单: %v", res.Overwritten)
	}
}

// API 写路径：默认保护人工价；force 覆盖；现值文件损坏 409 fail-closed。
func TestPricingSyncAPI(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "catalog.json")
	mustWriteFile(t, fixture, m7w1Catalog)
	pricingPath := filepath.Join(dir, "pricing.json")
	mustWriteFile(t, pricingPath, `{"prices":[{"model":"claude-test","input_per_m":99,"output_per_m":99}]}`)

	s := &Server{Planner: NewPlanner(nil, nil), Version: "t", AppDir: dir}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	post := func(body string) *http.Response {
		resp, err := http.Post(srv.URL+"/api/pricing/sync", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post(`{"from":"` + strings.ReplaceAll(fixture, `\`, `\\`) + `"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync status = %d", resp.StatusCode)
	}
	var r struct {
		OK     bool       `json:"ok"`
		Sync   SyncResult `json:"sync"`
		Config struct {
			Prices []PriceEntry `json:"prices"`
		} `json:"config"`
	}
	decodeInto(t, resp.Body, &r)
	resp.Body.Close()
	if !r.OK || len(r.Sync.Added) == 0 || r.Sync.NoPrice != 2 {
		t.Fatalf("sync 响应: %+v", r.Sync)
	}
	// 落盘：人工价保留 + 同步条目带来源。
	var saved PricingFile
	if err := store.LoadJSON(pricingPath, &saved); err != nil {
		t.Fatal(err)
	}
	if got := findEntry(saved, "claude-test"); got.InputPerM != 99 {
		t.Fatalf("人工价应保留: %+v", got)
	}
	if got := findEntry(saved, "gpt-test"); got.Source != "models.dev" {
		t.Fatalf("来源标记: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup")); err != nil {
		t.Fatalf("写前备份缺失: %v", err)
	}

	// force 覆盖人工价。
	resp2 := post(`{"from":"` + strings.ReplaceAll(fixture, `\`, `\\`) + `","force":true}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("force status = %d", resp2.StatusCode)
	}
	resp2.Body.Close()
	if err := store.LoadJSON(pricingPath, &saved); err != nil {
		t.Fatal(err)
	}
	if got := findEntry(saved, "claude-test"); got.InputPerM != 3 {
		t.Fatalf("force 后人工价应被覆盖: %+v", got)
	}

	// 现值损坏 → 409（fail-closed，不覆写）。
	mustWriteFile(t, pricingPath, "{broken")
	resp3 := post(`{"from":"` + strings.ReplaceAll(fixture, `\`, `\\`) + `","force":true}`)
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("损坏现值应 409: %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

func mustFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	mustWriteFile(t, path, m7w1Catalog)
	return path
}

func findEntry(f PricingFile, model string) PriceEntry {
	for _, p := range f.Prices {
		if p.Model == model {
			return p
		}
	}
	return PriceEntry{}
}

func contains(ss []string, pred func(string) bool) bool {
	for _, s := range ss {
		if pred(s) {
			return true
		}
	}
	return false
}
