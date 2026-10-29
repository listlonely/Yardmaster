// trace 子命令（PRD F9）：决策记录的命令行展示——list 概览、show
// 展开完整候选链（失败链呈现：失败候选 ✗ 标注原因，成功 ✓）。
package app

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"agenthub/internal/gateway"
	"agenthub/internal/store"
)

func runTrace(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: agenthub trace <list|show> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return traceList(paths, args[1:], stdout, stderr)
	case "show":
		return traceShow(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：list、show\n", args[0])
		return ErrUsage
	}
}

func traceList(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("trace list", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "最多显示条数")
	agent := fs.String("agent", "", "按调用方 key 前缀过滤")
	result := fs.String("result", "", "按结果过滤：ok | fail | switch")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	switch *result {
	case "", "ok", "fail", "switch":
	default:
		fmt.Fprintln(stderr, "-result 仅支持 ok | fail | switch")
		return ErrUsage
	}
	traces, err := gateway.LoadTraceFiles(paths.TraceDir(), 0)
	if err != nil {
		return err
	}
	// 从新到旧过滤
	var shown int
	for i := len(traces) - 1; i >= 0 && shown < *limit; i-- {
		tr := traces[i]
		if *agent != "" && !strings.HasPrefix(tr.Caller, *agent) {
			continue
		}
		switch *result {
		case "ok":
			if tr.Outcome != "ok" {
				continue
			}
		case "fail":
			if tr.Outcome != "fail" {
				continue
			}
		case "switch":
			if !tr.Switched {
				continue
			}
		}
		mark := "✓"
		if tr.Outcome != "ok" {
			mark = "✗"
		}
		usage := ""
		if tr.Usage != nil {
			usage = fmt.Sprintf(" %d+%dtok", tr.Usage.Prompt, tr.Usage.Completion)
		}
		served := tr.Served
		if served == "" {
			served = fmt.Sprintf("HTTP %d", tr.HTTPStatus)
		}
		fmt.Fprintf(stdout, "%s %s  %-12s %s → %s  %dms%s  %s\n",
			mark, tr.Time, tr.Caller, tr.RequestModel, served, tr.TTFBMS, usage, tr.ID)
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(stdout, "（无匹配的决策记录）")
	}
	return nil
}

func traceShow(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: agenthub trace show <trace-id>")
		return ErrUsage
	}
	traces, err := gateway.LoadTraceFiles(paths.TraceDir(), 0)
	if err != nil {
		return err
	}
	for _, tr := range traces {
		if tr.ID != args[0] {
			continue
		}
		return renderTrace(stdout, tr)
	}
	return fmt.Errorf("trace %q 不存在", args[0])
}

func renderTrace(w io.Writer, tr gateway.Trace) error {
	outcome := "成功"
	if tr.Outcome != "ok" {
		outcome = "失败"
	}
	fmt.Fprintf(w, "决策 %s · %s · %s（%s）\n", tr.ID, tr.Time, outcome, tr.RoutePath)
	fmt.Fprintf(w, "  调用方: %s\n", orDash(tr.Caller))
	fmt.Fprintf(w, "  意图:   %s", orDash(tr.Intent))
	if tr.Rule != "" {
		fmt.Fprintf(w, "  ← 规则命中 %s", tr.Rule)
	}
	fmt.Fprintf(w, "\n  请求模型: %s → 目标: %s\n", orDash(tr.RequestModel), orDash(tr.Target))
	if len(tr.Candidates) > 0 {
		fmt.Fprintf(w, "  候选尝试链:\n")
		for _, c := range tr.Candidates {
			mark := "✓"
			if c.Result != gateway.CandOK {
				mark = "✗"
			}
			reason := c.Result
			if c.Status != 0 {
				reason = fmt.Sprintf("%s(HTTP %d)", c.Result, c.Status)
			}
			if c.Err != "" {
				reason += ": " + c.Err
			}
			proxy := ""
			if c.Proxy != "" {
				proxy = " 出口 " + c.Proxy
			}
			fmt.Fprintf(w, "    %s %s/%s [%s]%s %dms — %s\n",
				mark, c.Provider, c.Account, c.Model, proxy, c.DurMS, reason)
		}
	}
	served := tr.Served
	if served == "" {
		served = fmt.Sprintf("（无服务方，HTTP %d）", tr.HTTPStatus)
	} else {
		served += " [" + tr.ServedModel + "]"
	}
	fmt.Fprintf(w, "  实际服务方: %s\n", served)
	fmt.Fprintf(w, "  首字节 %dms · 总耗时 %dms", tr.TTFBMS, tr.TotalMS)
	if tr.Usage != nil {
		fmt.Fprintf(w, " · tokens %d+%d", tr.Usage.Prompt, tr.Usage.Completion)
	}
	if tr.Switched {
		fmt.Fprintf(w, " · 发生候选切换")
	}
	fmt.Fprintln(w)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
