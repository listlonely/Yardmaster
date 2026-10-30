// app/console 子命令与桌面窗口的单元测试（不碰网络、不真开窗口）。
package app

import (
	"os"
	"strings"
	"testing"
)

func TestConsoleTargetNormalizes(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9000": "http://127.0.0.1:9000/",
		"localhost:8317": "http://localhost:8317/",
		"http://a.b/c":   "http://a.b/c", // 已带协议：原样
		"https://x.y":    "https://x.y",
	}
	for in, want := range cases {
		if got := consoleTarget(in); got != want {
			t.Fatalf("consoleTarget(%q) = %q，want %q", in, got, want)
		}
	}
	// 空 addr：缺省回环（settings.listen 为空或无文件的机器上）
	if got := consoleTarget(""); got == "" || !strings.HasSuffix(got, "/") {
		t.Fatalf("consoleTarget(空) 应返回归一 URL: %q", got)
	}
}

// TestAppWindowBinFindsBrowser：常见 CI/桌面环境装有 Edge/Chrome；
// 未安装时跳过（openAppWindow 会回退系统浏览器，不在此测试范围）。
func TestAppWindowBinFindsBrowser(t *testing.T) {
	bin, err := appWindowBin()
	if err != nil {
		t.Skipf("本机无 Edge/Chromium 系浏览器: %v", err)
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatalf("探测到 %q 但文件不存在: %v", bin, serr)
	}
}

// TestRunAppUsageError：非法 flag 走 ErrUsage（不装配网关）。
func TestRunAppUsageError(t *testing.T) {
	var out strings.Builder
	if err := runApp([]string{"-bogus"}, &out); err != ErrUsage {
		t.Fatalf("app 非法 flag 应 ErrUsage: %v", err)
	}
	if err := runConsole([]string{"-bogus"}, &out); err != ErrUsage {
		t.Fatalf("console 非法 flag 应 ErrUsage: %v", err)
	}
}
