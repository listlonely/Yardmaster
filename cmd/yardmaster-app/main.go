// Command yardmaster-app 是桌面应用入口（Windows GUI subsystem，M8
// 收官后用户反馈双击黑窗闪现）：与 cmd/yardmaster 共享全部逻辑
//（internal/app），仅链接期 subsystem 不同——双击/开始菜单零终端黑窗、
// 零闪现；命令行场景（serve/CLI）仍用 yardmaster.exe（console
// subsystem）。构建由 release.yml 以 -ldflags "-H=windowsgui" 产出，
// 图标/版本/DPI 资源经 cmd/yardmaster/rsrc_windows_amd64.syso 同目录
// 自动链接。
package main

import (
	"fmt"
	"os"

	"yardmaster/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "yardmaster:", err)
		os.Exit(1)
	}
}
