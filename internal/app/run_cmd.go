// run 子命令（M5-W3）：headless Agent 启动原语的 CLI 直通——任务文本
// 经 stdin 传入，stdout 流式透传到终端，超时/取消杀进程，退出码按约定
// 透传（0 成功 / 124 超时 / 127 启动失败 / 130 取消 / 其余=Agent 退出码）。
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"yardmaster/internal/dispatch"
)

// ExitCodeError 携带子进程约定的退出码（main 据此设置进程退出码）。
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return fmt.Sprintf("退出码 %d", e.Code) }

// runRun 直接模式：argv 显式给出（供人工调试与 W4 引擎无关的独立使用）。
// 用法：yardmaster run [-timeout 10m] [-cwd 目录] [-model m] [-name 标签] -- <argv...>
func runRun(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	timeout := fs.Duration("timeout", dispatch.DefaultTimeout, "会话超时")
	cwd := fs.String("cwd", "", "Agent 工作目录（空=当前目录）")
	model := fs.String("model", "", "argv 中 {model} 占位的替换值")
	name := fs.String("name", "adhoc", "会话标签（摘要显示用）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "用法: yardmaster run [-timeout 10m] [-cwd 目录] [-model m] [--] <argv...>")
		fmt.Fprintln(stderr, "任务文本经 stdin 传入；stdout 流式透传；Ctrl-C 取消")
		return ErrUsage
	}
	task, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("读取任务文本: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	spec := dispatch.Spec{ID: *name, Command: argv, WorkDir: *cwd, Timeout: *timeout}
	res := dispatch.Run(ctx, spec, string(task), *model, stdout)
	fmt.Fprintf(stderr, "[run] %s · %s · %.1fs · %s\n", spec.ID, res.Outcome,
		float64(res.DurationMS)/1000, outcomeNote(res))
	if res.Outcome == dispatch.OutcomeOK {
		return nil
	}
	return &ExitCodeError{Code: res.ExitCodeOf()}
}

func outcomeNote(r dispatch.Result) string {
	switch r.Outcome {
	case dispatch.OutcomeExit:
		return fmt.Sprintf("exit %d", r.ExitCode)
	case dispatch.OutcomeLaunch, dispatch.OutcomeTimeout:
		return r.ErrText
	default:
		return ""
	}
}
