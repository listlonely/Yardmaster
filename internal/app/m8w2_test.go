// M8-W2 依赖边界断言（app.md §三双保险之单测侧）：网关核心包的依赖
// 链不得出现 wails——webview 依赖仅允许在 internal/app 壳层
// （window_wails.go，windows build tag）。CI 另有 grep 步骤兜底。
package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorePackagesNoWailsDependency(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go 不在 PATH（跳过依赖边界断言；CI 与本机全量跑覆盖）")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("定位模块根: %v", err)
	}
	for _, pkg := range []string{
		"./internal/gateway", "./internal/store",
		"./internal/plugin", "./internal/otlp", "./internal/credential",
		"./internal/netproxy",
	} {
		cmd := exec.Command(goBin, "list", "-deps", pkg)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
		}
		if strings.Contains(string(out), "wailsapp") {
			t.Errorf("%s 依赖链出现 wailsapp（铁律：webview 不进网关核心，app.md §三）", pkg)
		}
	}
}
