// Command yardmaster 是唯一入口：只做装配，业务逻辑全部在 internal 包
// （开发文档 §3 仓库结构）。启动：yardmaster serve；查询版本：yardmaster version。
//
// Windows 资源（图标/版本信息/DPI manifest）经 tools/genwinres 预生成
// 为 rsrc_windows_amd64.syso 随构建自动链接；发版后重新生成：
//
//go:generate go run ../../tools/genwinres
package main

import (
	"errors"
	"fmt"
	"os"

	"yardmaster/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var ec *app.ExitCodeError // run 子命令：透传 headless 会话约定退出码
		if errors.As(err, &ec) {
			os.Exit(ec.Code)
		}
		fmt.Fprintln(os.Stderr, "yardmaster:", err)
		os.Exit(1)
	}
}
