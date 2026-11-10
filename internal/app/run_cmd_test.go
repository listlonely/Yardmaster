// run 子命令测试：以测试二进制自身扮演假 Agent（TestHelperProcess 模式，
// 不碰真机 Agent、不碰网络）。
package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// helperRunSpec 以测试二进制自身扮演假 Agent（行为经环境变量注入，
// 子进程继承父环境）。
func helperRunSpec(t *testing.T, behavior string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("YM_RUN_HELPER_ON", "1")
	t.Setenv("YM_RUN_HELPER_BEHAVIOR", behavior)
	return []string{exe, "-test.run=TestRunHelperProcess", "--"}
}

// TestRunHelperProcess 不是真正的测试：作为假 Agent 子进程运行。
func TestRunHelperProcess(t *testing.T) {
	if os.Getenv("YM_RUN_HELPER_ON") != "1" {
		return
	}
	switch os.Getenv("YM_RUN_HELPER_BEHAVIOR") {
	case "ok":
		fmt.Println("答案是 42")
	case "fail":
		fmt.Fprintln(os.Stderr, "agent 挂了")
		os.Exit(7)
	case "slow":
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func TestRunCmdStreamsAndExitsZero(t *testing.T) {
	var out, errOut strings.Builder
	argv := append([]string{"-name", "fake"}, helperRunSpec(t, "ok")...)
	err := runRun(argv, strings.NewReader("任务文本"), &out, &errOut)
	if err != nil {
		t.Fatalf("run 应成功: %v", err)
	}
	if !strings.Contains(out.String(), "答案是 42") {
		t.Fatalf("stdout 未透传: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "ok") {
		t.Fatalf("摘要行缺失: %q", errOut.String())
	}
}

func TestRunCmdPropagatesExitCode(t *testing.T) {
	var out, errOut strings.Builder
	argv := append([]string{"-timeout", "5s"}, helperRunSpec(t, "fail")...)
	err := runRun(argv, strings.NewReader("t"), &out, &errOut)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 7 {
		t.Fatalf("应透传退出码 7: %v", err)
	}
}

func TestRunCmdTimeoutUsesConvention(t *testing.T) {
	var out, errOut strings.Builder
	argv := append([]string{"-timeout", "200ms"}, helperRunSpec(t, "slow")...)
	start := time.Now()
	err := runRun(argv, strings.NewReader("t"), &out, &errOut)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 124 {
		t.Fatalf("超时应退出码 124: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("超时未及时终止")
	}
}

func TestRunCmdLaunchFailIs127(t *testing.T) {
	var out, errOut strings.Builder
	err := runRun([]string{"definitely-not-real-xyz"}, strings.NewReader("t"), &out, &errOut)
	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != 127 {
		t.Fatalf("启动失败应退出码 127: %v", err)
	}
	if !strings.Contains(errOut.String(), "已安装") && !strings.Contains(errOut.String(), "未找到可执行文件") {
		t.Fatalf("ToS 提示缺失: %q", errOut.String())
	}
}

func TestRunCmdUsageError(t *testing.T) {
	var out, errOut strings.Builder
	if err := runRun([]string{}, strings.NewReader(""), &out, &errOut); err != ErrUsage {
		t.Fatalf("空 argv 应 ErrUsage: %v", err)
	}
}
