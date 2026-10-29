// Command yardmaster 是唯一入口：只做装配，业务逻辑全部在 internal 包
// （开发文档 §3 仓库结构）。启动：yardmaster serve；查询版本：yardmaster version。
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
