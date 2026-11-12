// plugin CLI 测试（M7-W5）：list/enable/disable 直改 plugins.json
//（备份同规、损坏 fail-closed 拒改、未知 id 指引）。HOME 经 testenv
// 沙箱隔离。
package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yardmaster/internal/plugin"
	"yardmaster/internal/testenv"
)

func writePluginFile(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".yardmaster")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugins.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestPluginCLIFlow(t *testing.T) {
	home := testenv.New(t)
	writePluginFile(t, home, `{"plugins":[
		{"id":"logger","enabled":true,"command":["node","examples/trace-logger.mjs"],"capabilities":["events.trace"]},
		{"id":"parser","enabled":false,"command":["node","x.mjs"],"capabilities":["classify.intent"]},
		{"id":"helper","enabled":true,"command":["x"],"capabilities":["classify.intent"]}
	]}`)

	var out, errOut strings.Builder
	if err := Run([]string{"plugin", "list"}, &out, &errOut); err != nil {
		t.Fatalf("list: %v\nstderr=%s", err, errOut.String())
	}
	for _, want := range []string{"logger", "parser", "helper", "events.trace", "classify.intent"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list 缺 %q: %s", want, out.String())
		}
	}

	// disable → 落盘翻转 + 备份。
	out.Reset()
	if err := Run([]string{"plugin", "disable", "logger"}, &out, &errOut); err != nil {
		t.Fatalf("disable: %v\nstderr=%s", err, errOut.String())
	}
	f2, err := plugin.Load(filepath.Join(home, ".yardmaster", "plugins.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f2.Plugins {
		if p.ID == "logger" && p.Enabled {
			t.Fatal("disable 后应 enabled=false")
		}
	}
	if entries, err := os.ReadDir(filepath.Join(home, ".yardmaster", "backup")); err != nil || len(entries) == 0 {
		t.Fatalf("写前备份缺失: %v %d", err, len(entries))
	}

	// enable 恢复。
	if err := Run([]string{"plugin", "enable", "logger"}, &out, &errOut); err != nil {
		t.Fatalf("enable: %v", err)
	}
	f3, err := plugin.Load(filepath.Join(home, ".yardmaster", "plugins.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f3.Plugins {
		if p.ID == "logger" && !p.Enabled {
			t.Fatal("enable 后应 enabled=true")
		}
	}

	// 未知 id → ErrUsage 指引。
	if err := Run([]string{"plugin", "enable", "nope"}, &out, &errOut); !errors.Is(err, ErrUsage) {
		t.Fatalf("未知 id 应 ErrUsage: %v", err)
	}
	if !strings.Contains(errOut.String(), "不在清单") {
		t.Fatalf("指引: %s", errOut.String())
	}
}

func TestPluginCLIFailClosed(t *testing.T) {
	home := testenv.New(t)
	path := writePluginFile(t, home, "{broken")

	var out, errOut strings.Builder
	if err := Run([]string{"plugin", "list"}, &out, &errOut); err == nil {
		t.Fatal("损坏文件 list 应报错")
	} else if !strings.Contains(err.Error(), "读不懂=未知") {
		t.Fatalf("fail-closed 指引: %v", err)
	}
	errOut.Reset()
	if err := Run([]string{"plugin", "enable", "x"}, &out, &errOut); err == nil {
		t.Fatal("损坏文件 enable 应报错")
	}
	// 原文件未被改写。
	b, _ := os.ReadFile(path)
	if string(b) != "{broken" {
		t.Fatalf("损坏文件不得被改写: %q", string(b))
	}
	// 不存在 → 指引示例（删掉损坏文件走 ErrNotExist 路径）。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run([]string{"plugin", "list"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "未配置插件") {
		t.Fatalf("无文件指引: %s", out.String())
	}
}
