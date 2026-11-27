// Windows 开机自启动（2026-11-11）：settings.autostart 偏好的进程外
// 落地——HKCU 注册表 Run 键（登录时拉起当前 exe；GUI 模式下即
// yardmaster-app.exe，与双击入口一致）。HKCU 无需管理员权限；键值带
// 引号防路径空格。写/删失败仅降级留痕（壳层纪律：不阻断网关启动），
// 下次设置变更或启动自愈会重试。
//
// 双触发器（2026-11-27）：Run 键之外再注册用户级登录计划任务。背景：
// 外部启动器/优化工具会清 Run 键（2026-11-25 看门狗当场捕获一次），而
// 删除若发生在程序未运行期间（退出后/关机前后），周期校验鞭长莫及——
// 键被删→登录时程序不起→自愈没机会跑，自启动就此失效（11-27 早实证：
// 登录 4.5 分钟后仍需手动拉起）。任务计划程序不在常规清理工具的清理
// 范围内，且两个通道独立生效：任务在登录时拉起程序 → 启动自愈把 Run
// 键补回来，恢复双保险。注册通道走 PowerShell Cmdlet（schtasks CLI
// 的 onlogon 触发器非提权报拒绝访问，实测 Register-ScheduledTask 无此
// 限制）。同步经合并队列保序收敛：高频连击（开→关）不会因 goroutine
// 乱序把终态留成「偏好关而任务残留」。
//
//go:build windows

package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

// autostartValueName 是 Run 键的值名（控制面板/注册表编辑器里显示的
// 条目名，与产品名一致便于用户辨认）。
const autostartValueName = "Yardmaster"

// autostartRunKeyPath 是 HKCU 下的当前用户登录项路径。
const autostartRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// scheduledTaskName 是任务计划程序库里的任务名（第二自启触发器，
// 2026-11-27；与 Run 键同指令同语义，删除任一通道由启动自愈补齐）。
const scheduledTaskName = "YardmasterAutostart"

// autostartValueExists 报告 Run 键当前是否已有 Yardmaster 条目（壳层
// 看门狗周期校验用，2026-11-25：外部启动器/优化工具会清 Run 键——当日
// 实测被删而进程日志无任何「已关闭」记录）。只读探测，失败=false（校验
// 方将按缺失补写，幂等无害）。任务通道不作周期校验：清理工具不碰任务
// 计划程序，且每次 applyAutostart(true)（启动自愈/偏好切换）都会
// -Force 重注册，路径漂移与误删均在下一次进程启动时收敛。
func autostartValueExists() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue(autostartValueName)
	return err == nil
}

// applyAutostart 按偏好写/删注册表 Run 键（幂等；enable=true 确保存在，
// false 确保不存在），并请求计划任务通道同步（双触发器，见文件头）。
func applyAutostart(enable bool) {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("开机自启动同步失败（取可执行文件路径）", "err", err)
		return
	}
	// 双通道纪律：先排队任务同步再落注册表，注册表通道任何失败都不
	// 阻断任务通道——两者独立生效，任一存活即可在下次登录拉起。
	requestScheduledTaskSync(enable, exe)
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		slog.Warn("开机自启动同步失败（打开注册表 Run 键）", "err", err)
		return
	}
	defer key.Close()
	if enable {
		// --no-window：登录自启动为**静默常驻**（托盘承载入口，不弹
		// 控制台窗口——用户主动双击 exe 或托盘菜单「打开窗口」时才见
		// 界面）。runApp 的既有 flag（app.go）。
		if err := key.SetStringValue(autostartValueName, fmt.Sprintf(`"%s" --no-window`, exe)); err != nil {
			slog.Warn("开机自启动同步失败（写入 Run 键）", "err", err)
			return
		}
		slog.Info("开机自启动已开启", "exe", exe)
		return
	}
	if err := key.DeleteValue(autostartValueName); err != nil && err != registry.ErrNotExist {
		// 键不存在=目标状态，不算失败；其余写失败留痕。
		slog.Warn("开机自启动同步失败（删除 Run 键）", "err", err)
		return
	}
	slog.Info("开机自启动已关闭")
}

// 计划任务同步的合并队列：applyAutostart 的调用方分布在启动自愈/托盘
// 切换/设置页落盘钩子/看门狗补写多处且可能高频连击（快速开→关），每
// 调用直接起一个 PowerShell 子进程既浪费又可能乱序收敛（注销先于注册
// 执行=偏好关而任务残留）。单 worker 消费最新期望态：连续变更合并为
// 一次执行，终态恒等于最后一次偏好。
var (
	schedMu     sync.Mutex
	schedEnable bool   // 最新期望态（合并队列的收敛目标）
	schedExe    string // 期望态对应的 exe 路径（重注册刷新路径漂移）
	schedKick   chan struct{}
	schedOnce   sync.Once
)

// requestScheduledTaskSync 排队一次任务同步（非阻塞；已有待办则只
// 更新期望态，worker 醒来读到的即最新合并值）。
func requestScheduledTaskSync(enable bool, exe string) {
	schedOnce.Do(func() {
		schedKick = make(chan struct{}, 1)
		go scheduledTaskWorker()
	})
	schedMu.Lock()
	schedEnable, schedExe = enable, exe
	schedMu.Unlock()
	select {
	case schedKick <- struct{}{}:
	default:
	}
}

// scheduledTaskWorker 常驻消费合并队列（每轮读最新期望态执行一次）。
func scheduledTaskWorker() {
	for range schedKick {
		schedMu.Lock()
		enable, exe := schedEnable, schedExe
		schedMu.Unlock()
		syncScheduledTask(enable, exe)
	}
}

// syncScheduledTask 注册/注销用户级登录计划任务（第二自启触发器）。
// powershell.exe 走 Register/Unregister-ScheduledTask（-Force 幂等重
// 注册，路径漂移随之刷新）。HideWindow 防子进程控制台在桌面闪烁；
// 15s 超时防挂死；失败仅降级留痕（Run 键通道独立存活，壳层纪律）。
func syncScheduledTask(enable bool, exe string) {
	user := os.Getenv("USERNAME")
	if enable && (user == "" || exe == "") {
		slog.Warn("开机自启计划任务跳过（缺用户名或 exe 路径）", "user", user, "exe", exe)
		return
	}
	var script string
	if enable {
		// PowerShell 单引号字符串：内嵌 ' 翻倍转义（路径含人名缩写等
		// 边缘字符时的防御，常规路径不受影响）。
		escExe := strings.ReplaceAll(exe, "'", "''")
		escUser := strings.ReplaceAll(user, "'", "''")
		script = fmt.Sprintf(
			`Register-ScheduledTask -TaskName '%s' -Action (New-ScheduledTaskAction -Execute '%s' -Argument '--no-window') `+
				`-Trigger (New-ScheduledTaskTrigger -AtLogOn -User '%s') `+
				`-Principal (New-ScheduledTaskPrincipal -UserId '%s' -LogonType Interactive -RunLevel Limited) -Force | Out-Null`,
			scheduledTaskName, escExe, escUser, escUser)
	} else {
		script = fmt.Sprintf(
			`Unregister-ScheduledTask -TaskName '%s' -Confirm:$false -ErrorAction SilentlyContinue`,
			scheduledTaskName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		slog.Warn("开机自启计划任务同步超时", "enable", enable)
		return
	}
	if err != nil {
		slog.Warn("开机自启计划任务同步失败", "enable", enable, "err", err, "out", strings.TrimSpace(string(out)))
		return
	}
	if enable {
		slog.Info("开机自启计划任务已注册", "task", scheduledTaskName, "exe", exe)
	} else {
		slog.Info("开机自启计划任务已注销")
	}
}
