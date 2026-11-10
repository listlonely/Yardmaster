// dispatch 子命令（M5-W5）：任务→Agent 派发的命令行入口。
//
//	yardmaster dispatch agents            列出 dispatch.json 的 agent
//	yardmaster dispatch init [-write]     打印三家建议配置（形状未实测，
//	                                      须自行确认；-write 校验后落盘）
//	yardmaster dispatch run [-agent id] [-model m] [-events] 任务文本...
//	                                      本地引擎派发（stdout 流式透传；
//	                                      -events 实时事件流，M6-W3）
//	yardmaster dispatch run -t <模板> key=val...
//	                                      按任务模板派发（M6-W4：占位
//	                                      实例化 + 模板默认 agent/超时）
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/store"
)

func runDispatch(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: yardmaster dispatch <agents|init|run> [参数]")
		return ErrUsage
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "agents":
		return dispatchAgents(paths, stdout)
	case "init":
		return dispatchInit(paths, args[1:], stdout)
	case "run":
		return dispatchRun(paths, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q；可用：agents、init、run\n", args[0])
		return ErrUsage
	}
}

// loadEngineOrErr 读 dispatch.json：损坏 → fail-closed 报错（不静默空跑）。
// 会话终态落盘（M6-W2）：CLI 派发与网关派发同留档（恢复仅在 serve
// 装配做——CLI 一次性进程无历史可见需求）。
func loadEngineOrErr(paths store.Paths) (*dispatch.Engine, error) {
	e, err := dispatch.LoadEngine(paths.DispatchPath())
	if err != nil {
		return nil, err
	}
	e.AttachStore(dispatch.NewSessionStore(paths.DispatchSessionsPath()))
	return e, nil
}

func dispatchAgents(paths store.Paths, stdout io.Writer) error {
	e, err := loadEngineOrErr(paths)
	if err != nil {
		return err
	}
	agents := e.Agents()
	if len(agents) == 0 {
		fmt.Fprintln(stdout, "（dispatch.json 无 agent 条目；yardmaster dispatch init 生成建议配置）")
		return nil
	}
	for _, a := range agents {
		match := "仅显式指定"
		if a.Match != nil {
			var parts []string
			if a.Match.Intent != "" {
				parts = append(parts, "intent="+a.Match.Intent)
			}
			if a.Match.ModelRegex != "" {
				parts = append(parts, "model~"+a.Match.ModelRegex)
			}
			if a.Match.PromptRegex != "" {
				parts = append(parts, "prompt~"+a.Match.PromptRegex)
			}
			match = strings.Join(parts, " ")
		}
		if a.Default {
			match += " · default 兜底"
		}
		fmt.Fprintf(stdout, "%s\t%s\t输出=%s\t%s\n", a.ID, strings.Join(a.Command, " "),
			orDefaultStr(a.Output, "text"), match)
	}
	return nil
}

func dispatchInit(paths store.Paths, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("dispatch init", flag.ContinueOnError)
	write := fs.Bool("write", false, "校验通过后写入 dispatch.json（已存在则拒绝）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	f := dispatch.DispatchFile{Agents: dispatch.Suggestions()}
	fmt.Fprintln(stdout, "建议配置（三家 headless 形状未经真实凭证实测，请按本机安装情况修改 command）：")
	for _, a := range f.Agents {
		fmt.Fprintf(stdout, "  %s: %s\n", a.ID, strings.Join(a.Command, " "))
	}
	if !*write {
		fmt.Fprintln(stdout, "预览模式；确认后加 -write 写入 "+paths.DispatchPath())
		return nil
	}
	if _, err := os.Stat(paths.DispatchPath()); err == nil {
		return errors.New("dispatch.json 已存在（不覆盖；手工编辑或先删除）")
	}
	if err := dispatch.ValidateDispatch(f); err != nil {
		return err
	}
	if err := store.SaveJSON(paths.DispatchPath(), f); err != nil {
		return fmt.Errorf("写入 dispatch.json: %w", err)
	}
	fmt.Fprintln(stdout, "已写入 "+paths.DispatchPath())
	return nil
}

func dispatchRun(paths store.Paths, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dispatch run", flag.ContinueOnError)
	agentID := fs.String("agent", "", "显式指定 agent（空=按 match/default 选择）")
	model := fs.String("model", "", "argv {model} 占位的替换值")
	events := fs.Bool("events", false, "实时事件流渲染（jsonl agent 生效；其他输出原样透传）")
	tmplName := fs.String("t", "", "任务模板名（dispatch.json templates；剩余位置参数按 key=val 传参）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	rest := fs.Args()
	e, err := loadEngineOrErr(paths)
	if err != nil {
		return err
	}

	// 任务来源二选一：-t 模板实例化（fail-closed：缺参/多参/模板不存在
	// 均拒绝）或位置参数拼任务文本。
	var taskText string
	var tmpl dispatch.Template
	if *tmplName != "" {
		params := map[string]string{}
		for _, kv := range rest {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				fmt.Fprintf(stderr, "模板参数须为 key=val 形式: %q（-t 之后的参数都按模板传参解析；标志需写在前面，如 dispatch run -events -t 模板 k=v）\n", kv)
				return ErrUsage
			}
			params[k] = v
		}
		var err error
		tmpl, err = e.FindTemplate(*tmplName)
		if err != nil {
			return err
		}
		taskText, err = tmpl.Instantiate(params)
		if err != nil {
			return err
		}
		if *agentID == "" {
			*agentID = tmpl.Agent // 模板默认 agent（显式 -agent 仍可覆盖）
		}
		if *model == "" {
			*model = tmpl.Model
		}
	} else {
		taskText = strings.TrimSpace(strings.Join(rest, " "))
		if taskText == "" {
			fmt.Fprintln(stderr, "用法: yardmaster dispatch run [-agent id] [-model m] [-events] [-t 模板 key=val...] 任务文本")
			return ErrUsage
		}
	}
	agent, err := e.Select(*agentID, taskText, "", *model)
	if err != nil {
		return err
	}
	if tmpl.Timeout != "" { // 模板超时覆盖所选 agent 的超时
		agent.Timeout = tmpl.Timeout
	}
	fmt.Fprintf(stderr, "[dispatch] 选中 %s → %s\n", agent.ID, strings.Join(agent.Command, " "))
	var stream io.Writer
	var esw *eventStreamWriter
	if *events {
		esw = newEventStreamWriter(stdout)
		stream = esw
	} else {
		stream = stdout
	}
	sess, err := e.Submit(context.Background(), agent, taskText, *model, stream)
	if err != nil {
		return err
	}
	sess.Wait()
	if esw != nil { // 末行无换行时收口（运行 goroutine 的 Flush 只进 live 缓冲）
		for _, ev := range esw.p.Flush() {
			fmt.Fprintln(stdout, dispatch.RenderEvent(ev))
		}
	}
	if sess.Result == nil {
		return errors.New("会话无结果（异常）")
	}
	r := sess.Result
	if esw == nil { // 事件流已实时渲染终态，不再重复打印提取结果
		extracted := dispatch.ExtractResult(r.Output, agent.Output)
		if extracted != "" && agent.Output == "jsonl" && extracted != r.Output {
			fmt.Fprintln(stdout, "\n[dispatch] 结果: "+extracted)
		}
	}
	fmt.Fprintf(stderr, "[dispatch] %s · %s · %.1fs · %s\n", sess.ID, r.Outcome,
		float64(r.DurationMS)/1000, orDefaultStr(r.ErrText, ""))
	if r.Outcome != dispatch.OutcomeOK {
		return &ExitCodeError{Code: r.ExitCodeOf()}
	}
	return nil
}

// eventStreamWriter 把原始字节流喂进事件解析器并即时渲染完整事件行
// （CLI --events；raw 事件原样回显——非 jsonl 输出退化为透传）。
// 仅会话运行 goroutine 单写者访问，解析器无需加锁。
type eventStreamWriter struct {
	p   *dispatch.EventParser
	out io.Writer
}

func newEventStreamWriter(out io.Writer) *eventStreamWriter {
	return &eventStreamWriter{p: dispatch.NewEventParser(), out: out}
}

func (w *eventStreamWriter) Write(p []byte) (int, error) {
	for _, ev := range w.p.Feed(p) {
		fmt.Fprintln(w.out, dispatch.RenderEvent(ev))
	}
	return len(p), nil
}

func orDefaultStr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
