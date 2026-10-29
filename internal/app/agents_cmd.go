// agents 子命令（PRD F4 的 CLI 形态）：list/preview/wire/unwind/drift。
// wire 不带 --yes 时只打印预览——预览+确认是 F4 的强制流程。
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"agenthub/internal/agent"
	"agenthub/internal/store"
)

func runAgents(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: agenthub agents <list|preview|wire|unwind|drift> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	m := &agent.Manager{Paths: paths, Addr: "http://127.0.0.1:8317"}
	switch args[0] {
	case "list":
		return agentsList(m, stdout)
	case "preview":
		return agentsPreview(m, args[1:], stdout, stderr)
	case "wire":
		return agentsWire(m, args[1:], stdout, stderr)
	case "unwind":
		return agentsUnwind(m, args[1:], stdout, stderr)
	case "drift":
		return agentsDrift(m, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：list、preview、wire、unwind、drift\n", args[0])
		return ErrUsage
	}
}

var stateLabel = map[agent.State]string{
	agent.StateMissing: "未安装",
	agent.StateUnwired: "未接入",
	agent.StateWired:   "已接入",
	agent.StateDrift:   "漂移",
	agent.StateSpecTBD: "规格待核验",
}

func agentsList(m *agent.Manager, stdout io.Writer) error {
	for _, def := range agent.Registry() {
		st, err := m.StateOf(def)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%-12s %-12s %s", def.ID, def.Name, stateLabel[st])
		if def.SpecNote != "" {
			line += "（" + def.SpecNote + "）"
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func agentsPreview(m *agent.Manager, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("agents preview", flag.ContinueOnError)
	model := fs.String("model", "", "默认模型/路由组（如 siliconflow/zai-org/GLM-5.3）")
	addr := fs.String("addr", "http://127.0.0.1:8317", "网关地址")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() != 1 || *model == "" {
		fmt.Fprintln(stderr, "用法: agenthub agents preview [-addr URL] -model <模型> <agent-id>")
		return ErrUsage
	}
	m.Addr = *addr
	def, ok := agent.ByID(fs.Arg(0))
	if !ok {
		fmt.Fprintf(stderr, "未知 agent %q\n", fs.Arg(0))
		return ErrUsage
	}
	lines, err := m.Preview(def, *model)
	if err != nil {
		if errors.Is(err, agent.ErrSpecTBD) {
			fmt.Fprintln(stdout, "✗ "+err.Error())
			return nil
		}
		return err
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	return nil
}

func agentsWire(m *agent.Manager, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("agents wire", flag.ContinueOnError)
	model := fs.String("model", "", "默认模型/路由组")
	addr := fs.String("addr", "http://127.0.0.1:8317", "网关地址")
	yes := fs.Bool("yes", false, "确认应用（缺省只打印预览）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() != 1 || *model == "" {
		fmt.Fprintln(stderr, "用法: agenthub agents wire [-addr URL] -model <模型> [-yes] <agent-id>")
		return ErrUsage
	}
	m.Addr = *addr
	def, ok := agent.ByID(fs.Arg(0))
	if !ok {
		fmt.Fprintf(stderr, "未知 agent %q\n", fs.Arg(0))
		return ErrUsage
	}
	lines, err := m.Preview(def, *model)
	if err != nil {
		if errors.Is(err, agent.ErrSpecTBD) {
			fmt.Fprintln(stdout, "✗ "+err.Error())
			return nil
		}
		return err
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	if !*yes {
		fmt.Fprintln(stdout, "\n（预览模式：加 -yes 应用以上变更）")
		return nil
	}
	rec, err := m.Wire(def, *model)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ %s 已接入（key=%s，原配置已备份，断开用 agents unwind %s）。新会话生效。\n", rec.Agent, rec.Key, rec.Agent)
	return nil
}

func agentsUnwind(m *agent.Manager, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: agenthub agents unwind <agent-id>")
		return ErrUsage
	}
	def, ok := agent.ByID(args[0])
	if !ok {
		fmt.Fprintf(stderr, "未知 agent %q\n", args[0])
		return ErrUsage
	}
	if err := m.Unwire(def); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ %s 已断开，原始配置已逐字节还原\n", def.ID)
	return nil
}

func agentsDrift(m *agent.Manager, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法: agenthub agents drift <agent-id>")
		return ErrUsage
	}
	def, ok := agent.ByID(args[0])
	if !ok {
		fmt.Fprintf(stderr, "未知 agent %q\n", args[0])
		return ErrUsage
	}
	rows, err := m.Drift(def)
	if err != nil {
		return err
	}
	drifted := 0
	for _, r := range rows {
		mark := " "
		if r.Drift {
			mark = "!"
			drifted++
		}
		fmt.Fprintf(stdout, "%s %-40s 期望=%s 当前=%s\n", mark, r.Desc, r.Expected, r.Current)
	}
	if drifted > 0 {
		fmt.Fprintf(stdout, "检测到 %d 处漂移；重新应用请先 unwind 再 wire（覆盖前有备份）\n", drifted)
	} else {
		fmt.Fprintln(stdout, "无漂移")
	}
	return nil
}
