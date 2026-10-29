// settings 子命令：全局代理等用户级设置（F3 继承链末端）。
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"agenthub/internal/netproxy"
	"agenthub/internal/store"
)

func runSettings(args []string, stdout, stderr io.Writer) error {
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: agenthub settings <get|set> [键 值]；当前支持键：global_proxy")
		return ErrUsage
	}
	switch args[0] {
	case "get":
		s, err := store.LoadSettings(paths)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if s.GlobalProxy == "" {
			fmt.Fprintln(stdout, "global_proxy = （空，跟随系统代理）")
		} else {
			fmt.Fprintf(stdout, "global_proxy = %s\n", netproxy.Redact(s.GlobalProxy))
		}
		return nil
	case "set":
		fs := flag.NewFlagSet("settings set", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return ErrUsage
		}
		if fs.NArg() != 2 {
			fmt.Fprintln(stderr, "用法: agenthub settings set global_proxy <url|direct|空>")
			return ErrUsage
		}
		key, val := fs.Arg(0), fs.Arg(1)
		if key != "global_proxy" {
			fmt.Fprintf(stderr, "不支持的设置键 %q；当前支持：global_proxy\n", key)
			return ErrUsage
		}
		if val != "" {
			if _, err := netproxy.Parse(netproxy.Ref(val)); err != nil {
				fmt.Fprintln(stderr, err)
				return ErrUsage
			}
		}
		s, err := store.LoadSettings(paths)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.GlobalProxy = val
		if err := store.SaveSettings(paths, s); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "已保存 global_proxy（重启 serve 生效）\n")
		return nil
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：get、set\n", args[0])
		return ErrUsage
	}
}
