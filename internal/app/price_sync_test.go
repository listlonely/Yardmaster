// price sync CLI 测试（M7-W1）：-from 本地目录文件全链路（报告/落盘/
// 来源列/备份留底/-force 覆盖人工价）。HOME 经 testenv 沙箱隔离。
package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestPriceSyncCLIFlow(t *testing.T) {
	home := testenv.New(t)
	fixture := filepath.Join(home, "catalog.json")
	if err := os.WriteFile(fixture, []byte(m7w1Catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	// 人工价先行（将被保护），再同步。
	var out, errOut strings.Builder
	if err := Run([]string{"price", "set", "gpt-test", "9", "9"}, &out, &errOut); err != nil {
		t.Fatalf("price set: %v\nstderr=%s", err, errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if err := Run([]string{"price", "sync", "-from", fixture}, &out, &errOut); err != nil {
		t.Fatalf("price sync: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "新增 1") || !strings.Contains(out.String(), "保护跳过 1") {
		t.Fatalf("同步报告: %s", out.String())
	}
	if !strings.Contains(out.String(), "gpt-test（人工价保护，-force 覆盖）") {
		t.Fatalf("保护明细: %s", out.String())
	}
	// 备份留底（price set 先写了一版，sync 前再备份一版）。
	backupDir := filepath.Join(home, ".yardmaster", "backup")
	ents, err := os.ReadDir(backupDir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("backup 目录缺失或为空: %v", err)
	}
	// list 展示来源列。
	out.Reset()
	if err := Run([]string{"price", "list"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "claude-test") || !strings.Contains(out.String(), "models.dev") {
		t.Fatalf("list 来源列: %s", out.String())
	}

	// -force 覆盖人工价。
	out.Reset()
	if err := Run([]string{"price", "sync", "-from", fixture, "-force"}, &out, &errOut); err != nil {
		t.Fatalf("price sync -force: %v\nstdout=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "覆盖人工价 1") {
		t.Fatalf("force 报告: %s", out.String())
	}
}
