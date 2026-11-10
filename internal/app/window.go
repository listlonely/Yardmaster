// 应用窗口（M5.5 桌面壳，零 cgo 方案）：以浏览器 --app 模式打开独立
// 窗口（无地址栏/标签页，FLClash 式观感），exec 纯 Go、不动交叉编译
// 铁律。Windows 优先 Edge（WebView2 运行时随系统，覆盖率高），其次
// Chrome；Linux 走 chromium 系。均不可得时回退系统默认浏览器（有地址
// 栏的普通标签页）。真原生内嵌窗口（Wails v3）仍为后续独立构建变体。
package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// appWindowBin 返回可用的 app 模式浏览器可执行文件（按优先级探测），
// 未找到返回错误。
func appWindowBin() (string, error) {
	var candidates []string
	if runtime.GOOS == "windows" {
		for _, root := range []string{
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("ProgramFiles"),
			os.Getenv("LocalAppData"),
		} {
			if root == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
			)
		}
	} else {
		candidates = []string{
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
			"/usr/bin/chromium", "/usr/bin/chromium-browser",
			"/snap/bin/chromium",
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("未找到 Edge/Chromium 系浏览器")
}

// openAppWindow 打开应用窗口（--app 模式）。浏览器不可得时回退系统
// 默认浏览器打开（保留返回错误语义给调用方提示）。
func openAppWindow(url string) error {
	bin, err := appWindowBin()
	if err != nil {
		return openBrowser(url) // 回退：普通浏览器标签页（窗口感降级但不阻塞）
	}
	args := []string{
		"--app=" + url,
		"--window-size=1200,820",
	}
	if runtime.GOOS != "windows" {
		args = append(args, "--class=Yardmaster") // X11/Wayland 窗口与任务栏标识
	}
	return exec.Command(bin, args...).Start()
}
