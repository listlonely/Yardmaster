// price sync CLI 测试（M7-W1 增量）：按用量同步（trace 种子→精确键）、
// 人工价保护/-force、单价列与双币显示。HOME 经 testenv 沙箱隔离。
package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yardmaster/internal/testenv"
)

const m7w1Catalog = `{
  "anthropic": {"id":"anthropic","models":{
    "claude-test":{"id":"claude-test","cost":{"input":3,"output":15,"cache_read":0.3}}
  }},
  "openai": {"id":"openai","models":{
    "gpt-test":{"id":"gpt-test","cost":{"input":"2.5","output":"10"}}
  }}
}`

// seedTrace 在沙箱写一条已计费 trace（用量驱动同步与成本报告的数据源）。
func seedTrace(t *testing.T, home, served, model string) {
	t.Helper()
	dir := filepath.Join(home, ".yardmaster", "trace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("20060102")
	line := `{"id":"t1","time":"` + time.Now().UTC().Format(time.RFC3339) + `","served":"` + served + `","served_model":"` + model + `","outcome":"ok","usage":{"prompt":100,"completion":10}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "trace-"+today+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPriceSyncCLIFlow(t *testing.T) {
	home := testenv.New(t)
	fixture := filepath.Join(home, "catalog.json")
	if err := os.WriteFile(fixture, []byte(m7w1Catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	// 人工裸键先行（用量模式不触碰），再种 trace。
	var out, errOut strings.Builder
	if err := Run([]string{"price", "set", "gpt-test", "9", "9"}, &out, &errOut); err != nil {
		t.Fatalf("price set: %v\nstderr=%s", err, errOut.String())
	}
	seedTrace(t, home, "anthropic/main", "claude-test")

	// 缺省按用量同步：anthropic 命中 → 精确键 anthropic/claude-test。
	out.Reset()
	errOut.Reset()
	if err := Run([]string{"price", "sync", "-from", fixture}, &out, &errOut); err != nil {
		t.Fatalf("price sync: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "按用量（1 个模型对）") || !strings.Contains(out.String(), "新增 1") {
		t.Fatalf("同步报告: %s", out.String())
	}
	// list：来源列（精确键 + 人工）。
	out.Reset()
	if err := Run([]string{"price", "list"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "anthropic/claude-test") || !strings.Contains(out.String(), "models.dev") {
		t.Fatalf("list 来源列: %s", out.String())
	}
	if !strings.Contains(out.String(), "gpt-test") || !strings.Contains(out.String(), "人工") {
		t.Fatalf("人工键展示: %s", out.String())
	}

	// 显式 -models：裸键兜底命中人工键 → 保护；-force 覆盖。
	out.Reset()
	if err := Run([]string{"price", "sync", "-from", fixture, "-models", "gpt-test"}, &out, &errOut); err != nil {
		t.Fatalf("price sync -models: %v\nstdout=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "gpt-test（人工价保护，-force 覆盖）") {
		t.Fatalf("保护明细: %s", out.String())
	}
	out.Reset()
	if err := Run([]string{"price", "sync", "-from", fixture, "-models", "gpt-test", "-force"}, &out, &errOut); err != nil {
		t.Fatalf("price sync -force: %v\nstdout=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "覆盖人工价 1") {
		t.Fatalf("force 报告: %s", out.String())
	}

	// 双币：配汇率后成本报告显示 ≈¥ 与单价列。
	out.Reset()
	if err := Run([]string{"settings", "set", "fx-usd-cny", "7.2"}, &out, &errOut); err != nil {
		t.Fatalf("settings set: %v\nstderr=%s", err, errOut.String())
	}
	out.Reset()
	if err := Run([]string{"cost", "-days", "1"}, &out, &errOut); err != nil {
		t.Fatalf("cost: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "≈¥") || !strings.Contains(out.String(), "3/15") {
		t.Fatalf("成本报告双币/单价列: %s", out.String())
	}
}
