// Command agenthub 是唯一入口：只做装配，业务逻辑全部在 internal 包
// （开发文档 §3 仓库结构）。启动：agenthub serve；查询版本：agenthub version。
package main

import (
	"fmt"
	"os"

	"agenthub/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agenthub:", err)
		os.Exit(1)
	}
}
