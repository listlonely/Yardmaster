// settings 子命令：全局代理等用户级设置（F3 继承链末端）。
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"yardmaster/internal/gateway"
	"yardmaster/internal/netproxy"
	"yardmaster/internal/store"
)

// maskSecret 脱敏显示令牌（***+末 4 位；不足 4 位整体 ***）。
func maskSecret(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return "***" + s[len(s)-4:]
}

func runSettings(args []string, stdout, stderr io.Writer) error {
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: yardmaster settings <get|set> [键 值]；当前支持键：global_proxy、l1_classifier、l1_timeout_ms、listen、token")
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
		if s.L1Classifier == "" {
			fmt.Fprintln(stdout, "l1_classifier = （空，L1 分类关闭，仅 L0）")
		} else {
			fmt.Fprintf(stdout, "l1_classifier = %s\n", s.L1Classifier)
		}
		fmt.Fprintf(stdout, "l1_timeout_ms = %d\n", l1TimeoutOrDefault(s).Milliseconds())
		listen := s.Listen
		if listen == "" {
			listen = "127.0.0.1:8317（默认）"
		}
		fmt.Fprintf(stdout, "listen = %s\n", listen)
		if s.Token == "" {
			fmt.Fprintln(stdout, "token = （空，仅本机使用可留空）")
		} else {
			fmt.Fprintf(stdout, "token = %s\n", maskSecret(s.Token))
		}
		return nil
	case "set":
		fs := flag.NewFlagSet("settings set", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return ErrUsage
		}
		if fs.NArg() != 2 {
			fmt.Fprintln(stderr, "用法: yardmaster settings set <键> <值>")
			return ErrUsage
		}
		key, val := fs.Arg(0), fs.Arg(1)
		s, err := store.LoadSettings(paths)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		switch key {
		case "global_proxy":
			if val != "" {
				if _, err := netproxy.Parse(netproxy.Ref(val)); err != nil {
					fmt.Fprintln(stderr, err)
					return ErrUsage
				}
			}
			s.GlobalProxy = val
		case "l1_classifier":
			if val != "" && !strings.Contains(val, "/") {
				fmt.Fprintln(stderr, "l1_classifier 须为 provider/model 或 group/<id> 形式；留空关闭 L1")
				return ErrUsage
			}
			s.L1Classifier = val
		case "l1_timeout_ms":
			n, err := strconv.Atoi(val)
			if err != nil || n <= 0 || n > 10_000 {
				fmt.Fprintln(stderr, "l1_timeout_ms 须为 1~10000 的整数毫秒")
				return ErrUsage
			}
			s.L1TimeoutMS = n
		case "listen":
			if val != "" {
				if _, _, err := net.SplitHostPort(val); err != nil {
					fmt.Fprintf(stderr, "listen 须为 host:port 形式: %v\n", err)
					return ErrUsage
				}
			}
			s.Listen = val
		case "token":
			s.Token = val
		default:
			fmt.Fprintf(stderr, "不支持的设置键 %q；当前支持：global_proxy、l1_classifier、l1_timeout_ms、listen、token\n", key)
			return ErrUsage
		}
		// 安全基线（F10）：非回环监听必须带令牌（M2-W5，serve 拒启的
		// 同一规则提前到保存口拦截）。
		listen := s.Listen
		if listen == "" {
			listen = gateway.DefaultAddr
		}
		if err := gateway.ValidateSecurity(listen, s.Token); err != nil {
			fmt.Fprintln(stderr, err)
			return ErrUsage
		}
		if err := store.SaveSettings(paths, s); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "已保存 %s（重启 serve 生效）\n", key)
		return nil
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：get、set\n", args[0])
		return ErrUsage
	}
}

// l1TimeoutOrDefault 返回 L1 决策超时（0=默认 300ms，F8 规则表）。
func l1TimeoutOrDefault(s store.Settings) time.Duration {
	if s.L1TimeoutMS <= 0 {
		return 300 * time.Millisecond
	}
	return time.Duration(s.L1TimeoutMS) * time.Millisecond
}
