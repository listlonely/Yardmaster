// M11-W3 自替换安装引擎（app.md §十一）：aside 换侧 + 接力进程 +
// healthz 探活 + 失败回滚。工作在 W2 下载管线产物（update/ 暂存双 exe
// + VERSION）之上。
//
// 流程（W1 探针五机制实证）：
//
//	旧进程 installStaged：锁单飞 → 暂存校验 → 版本闸 → 旧 exe rename
//	.old → 暂存 exe moveIn 原位 → spawn 新 exe（环境变量 relayEnv=旧
//	pid，argv 原样透传）→ 旧进程收尾退出。
//	新进程 relay：Run 入口检 relayEnv → 等旧 pid 退出（端口让位）→
//	正常启动 serve/app → healthz 自探 → 成功：删 .old、清暂存；失败
//	（serve 报错/探活超时）：exe→.new-failed、.old→原位、spawn 旧版
//	回滚，新版进程退出。
//
// 设计取舍（对照 Magpie updater，app.md §十一定稿）：探活用网关自带
// healthz（优于进程存活判断）；回滚 spawn 的旧版不带 relayEnv——天然
// 无循环；标记防循环由版本闸（不降级不重装）承担；空闲保护（in-flight
// 请求等待）砍项留档（本地网关重启<2s，见开发文档 §9.13）。
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// relayEnv 是旧进程传给接力进程的环境变量（值为旧进程 pid）。选环境
// 变量而非 argv 标志：不侵入子命令 flag 解析、serve/app 两入口统一、
// 对用户可见的命令行零污染（W1 方案写 --update-relay，落地等价改 env，
// app.md 留痕）。
const relayEnv = "YM_UPDATE_RELAY_PID"

// exe 基名与 aside 后缀（.old=换侧备份、.new-failed=回滚废件）。
const (
	gatewayExeName = "yardmaster.exe"
	appExeName     = "yardmaster-app.exe"
	oldSuffix      = ".old"
	failedSuffix   = ".new-failed"
)

// relayState 是接力进程的启动上下文（oldPid=0 表示普通启动）。
type relayState struct {
	oldPid int
}

// stagePaths 定位暂存产物；缺失/不完整即拒装（读不懂=未知）。
func stagePaths(stageDir string) (gateway, appExe, version string, err error) {
	vb, verr := os.ReadFile(filepath.Join(stageDir, "VERSION"))
	if verr != nil {
		return "", "", "", fmt.Errorf("暂存目录 %s 无 VERSION 标记（先 yardmaster update download）: %w", stageDir, verr)
	}
	version = strings.TrimSpace(string(vb))
	gateway = filepath.Join(stageDir, gatewayExeName)
	if _, serr := os.Stat(gateway); serr != nil {
		return "", "", "", fmt.Errorf("暂存缺 %s: %w", gatewayExeName, serr)
	}
	appExe = filepath.Join(stageDir, appExeName)
	if _, serr := os.Stat(appExe); serr != nil {
		appExe = "" // 桌面入口可缺（暂存只有网关 exe）
	}
	return gateway, appExe, version, nil
}

// moveFile 同卷 rename、跨卷回退 copy（暂存 -dir 可能不在 exe 卷）。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		_ = in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = in.Close()
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// installLock 单飞锁（update/.install.lock，内容=持有 pid；进程已死视为
// 残留自动清除）。跨进程互斥靠 O_EXCL + pid 存活复核。
func installLock(stageDir string) (release func(), err error) {
	lock := filepath.Join(stageDir, ".install.lock")
	pid := strconv.Itoa(os.Getpid())
	f, oerr := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if oerr != nil {
		if b, rerr := os.ReadFile(lock); rerr == nil {
			if old, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && old != os.Getpid() && pidAlive(old) {
				return nil, fmt.Errorf("已有更新安装进行中（pid %d）", old)
			}
		}
		// 残留锁（进程已死/本进程重复触发）——清除重建
		_ = os.Remove(lock)
		f, oerr = os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if oerr != nil {
			return nil, fmt.Errorf("获取更新锁失败: %w", oerr)
		}
	}
	if _, err := f.WriteString(pid); err != nil {
		_ = f.Close()
		_ = os.Remove(lock)
		return nil, err
	}
	if err := f.Close(); err != nil { // 锁内容未完整落盘=锁不可信
		_ = os.Remove(lock)
		return nil, err
	}
	return func() { _ = os.Remove(lock) }, nil
}

// swapAside 换侧核心（exeDir 参数化由调用方解析，测试可注入假目录）：
// aside（原位→.old，旧 .old 先清）→ moveIn（暂存→原位）。原位缺失的
// 目标直接落位（app-only 安装补齐网关入口的场景，不产 .old）。任何
// 失败在返回前逆序还原（原位恒有可执行文件——绝无空窗）。返回
// appSwapped=桌面入口是否一并换侧。
func swapAside(stagedGateway, stagedApp, gatewayPath, appPath string) (appSwapped bool, err error) {
	moved := []string{} // 已 aside 的原位文件（还原用，逆序回滚）
	restore := func() {
		for i := len(moved) - 1; i >= 0; i-- {
			_ = os.Rename(moved[i]+oldSuffix, moved[i])
		}
	}
	aside := func(path string) error {
		_ = os.Remove(path + oldSuffix) // 上一轮残留
		if aerr := os.Rename(path, path+oldSuffix); aerr != nil {
			return fmt.Errorf("换侧失败（%s → %s）: %w", filepath.Base(path), oldSuffix, aerr)
		}
		moved = append(moved, path)
		return nil
	}
	if _, serr := os.Stat(gatewayPath); serr == nil {
		if err := aside(gatewayPath); err != nil {
			return false, err
		}
	} // 原位无 console exe（app-only 安装）：直接落位，不产 .old
	if stagedApp != "" {
		if _, serr := os.Stat(appPath); serr == nil {
			if err := aside(appPath); err != nil {
				restore()
				return false, err
			}
			appSwapped = true
		}
	}
	if err := moveFile(stagedGateway, gatewayPath); err != nil {
		restore()
		return false, fmt.Errorf("新版网关就位失败: %w", err)
	}
	if appSwapped {
		if err := moveFile(stagedApp, appPath); err != nil {
			// app exe 换新失败则整体还原（半新半旧不可接受）。
			_ = os.Remove(gatewayPath)
			restore()
			return false, fmt.Errorf("新版桌面入口就位失败: %w", err)
		}
	}
	return appSwapped, nil
}

// installStaged 执行换侧安装：锁单飞 → 暂存/版本闸校验 → swapAside →
// spawn 接力进程。restart 非空时在 spawn 成功后调用（当前进程收尾退
// 出；响应送回余量由调用方控制）。allowSame 跳过版本闸（CLI -force
// 演练路径；生产更新恒 false——防降级/重装循环）。任何失败在返回前
// 尽力还原原位。
func installStaged(stageDir string, restart func(), allowSame bool) error {
	unlock, err := installLock(stageDir)
	if err != nil {
		return err
	}
	defer unlock()

	stagedGateway, stagedApp, version, err := stagePaths(stageDir)
	if err != nil {
		return err
	}
	if !allowSame && Version != "dev" {
		if v := strings.TrimPrefix(version, "v"); compareVersions(Version, v) >= 0 {
			return fmt.Errorf("暂存版本 %s 不高于当前 %s（拒绝平级/降级安装；-force 可跳过闸）", version, Version)
		}
	}
	curExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	exeDir := filepath.Dir(curExe)
	gatewayPath := filepath.Join(exeDir, gatewayExeName)
	appPath := filepath.Join(exeDir, appExeName)

	appSwapped, err := swapAside(stagedGateway, stagedApp, gatewayPath, appPath)
	if err != nil {
		return err
	}

	// spawn 接力进程：与当前进程同入口形态、argv 原样透传、relayEnv=旧 pid。
	spawnPath := gatewayPath
	if strings.HasSuffix(strings.ToLower(curExe), appExeName) {
		spawnPath = appPath
	}
	cmd := exec.Command(spawnPath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), relayEnv+"="+strconv.Itoa(os.Getpid()))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // 接力 banner 落同一终端
	if serr := cmd.Start(); serr != nil {
		// spawn 失败还原（swapAside 已成功，手动逆操作；.old 存在才
		// 还原——app-only 补装路径无 .old，直接删除落位的新文件即可）。
		_ = os.Remove(gatewayPath)
		if appSwapped {
			_ = os.Remove(appPath)
		}
		for _, p := range []string{gatewayPath, appPath} {
			if _, e := os.Stat(p + oldSuffix); e == nil {
				_ = os.Rename(p+oldSuffix, p)
			}
		}
		return fmt.Errorf("启动接力进程失败: %w", serr)
	}
	slog.Info("自动更新已就位", "version", version, "relay_pid", cmd.Process.Pid,
		"old_pid", os.Getpid(), "staged_dir", stageDir)
	if restart != nil {
		restart()
	}
	return nil
}

// relayInit 检测接力环境（relayEnv）：等待旧进程退出（端口让位）后返回
// 接力上下文；普通启动返回零值（oldPid=0）。等待超时不阻断——残余占用
// 会让 serve bind 失败，自然进入回滚。
func relayInit() relayState {
	v := os.Getenv(relayEnv)
	if v == "" {
		return relayState{}
	}
	_ = os.Unsetenv(relayEnv) // 本进程再 spawn 的子进程不带接力标记
	pid, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || pid <= 0 {
		slog.Error("接力标记非法，按普通启动处理", relayEnv, v)
		return relayState{}
	}
	st := relayState{oldPid: pid}
	slog.Info("更新接力启动：等待旧进程退出", "old_pid", pid)
	if werr := waitForPID(pid, 20*time.Second); werr != nil {
		slog.Warn("等待旧进程退出超时（继续启动；端口冲突将走回滚）", "old_pid", pid, "err", werr)
	} else {
		time.Sleep(500 * time.Millisecond) // 退出余量（listener 释放）
	}
	return st
}

// relayFinalize 接力收尾（serve 已起）：healthz 自探确认新版健康后清理
// .old 与暂存；探活超时=新版不健康 → 回滚并退出。阻塞语义由调用方以
// goroutine 包装。
func relayFinalize(st *serveSetup, relay relayState) {
	url := "http://" + st.listen + "/healthz"
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				exe, _ := os.Executable()
				exeDir := filepath.Dir(exe)
				for _, name := range []string{gatewayExeName, appExeName} {
					p := filepath.Join(exeDir, name)
					if _, serr := os.Stat(p + oldSuffix); serr == nil {
						if rerr := os.Remove(p + oldSuffix); rerr != nil {
							slog.Warn("清理旧版备份失败（下次更新会先清除）", "file", p+oldSuffix, "err", rerr)
						}
					}
				}
				if d, derr := defaultStageDir(); derr == nil {
					if rerr := os.RemoveAll(d); rerr != nil {
						slog.Warn("清理更新暂存目录失败", "dir", d, "err", rerr)
					}
				}
				slog.Info("自动更新完成", "version", Version, "old_pid", relay.oldPid)
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	slog.Error("新版健康探活超时，回滚到旧版", "url", url)
	rollbackRelay()
	os.Exit(1)
}

// relayServeFailed serve 启动失败路径的回滚入口（relayFinalize 超时同
// 语义）。回滚后调用方进程应以非零码退出。
func relayServeFailed(serveErr error) {
	slog.Error("新版启动失败，回滚到旧版", "err", serveErr)
	rollbackRelay()
}

// rollbackRelay 回滚：新 exe→.new-failed、.old→原位、spawn 旧版（同
// argv，不带接力标记——天然无循环）。运行中 exe 可 rename（W1 探针
// 实证），故回滚不依赖先退出。
func rollbackRelay() {
	cur, err := os.Executable()
	if err != nil {
		slog.Error("回滚失败：定位当前可执行文件失败", "err", err)
		return
	}
	exeDir := filepath.Dir(cur)
	spawn := cur
	for _, name := range []string{gatewayExeName, appExeName} {
		p := filepath.Join(exeDir, name)
		if _, serr := os.Stat(p + oldSuffix); serr != nil {
			continue // 无备份（本轮未换侧）——跳过
		}
		if strings.EqualFold(p, cur) {
			if rerr := os.Rename(p, p+failedSuffix); rerr != nil {
				slog.Error("回滚失败：坏包改名失败（旧版备份 .old 仍在，可手动还原）", "file", p, "err", rerr)
				return
			}
			spawn = p
		}
		if rerr := os.Rename(p+oldSuffix, p); rerr != nil {
			slog.Error("回滚失败：旧版还原失败", "file", p+oldSuffix, "err", rerr)
			return
		}
	}
	cmd := exec.Command(spawn, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if serr := cmd.Start(); serr != nil {
		slog.Error("回滚失败：旧版重启失败（.old 已还原原位，可手动启动）", "err", serr)
		return
	}
	slog.Info("已回滚到旧版并重启", "spawn_pid", cmd.Process.Pid, "failed_exe_suffix", failedSuffix)
}

// updateInstallCmd 实现 `yardmaster update install`：暂存缺失时先走
// 完整下载管线，随后换侧+spawn 接力。CLI 进程装完即退（接力进程等
// CLI pid 退出后自行起 serve——本命令不持有网关）。
func updateInstallCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("update install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "暂存目录（缺省 exe 同级 update/）")
	force := fs.Bool("force", false, "跳过版本闸（平级/降级也装，演练用）")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	stageDir := *dir
	if stageDir == "" {
		d, err := defaultStageDir()
		if err != nil {
			return err
		}
		stageDir = d
	}
	if _, _, _, serr := stagePaths(stageDir); serr != nil {
		fmt.Fprintln(stdout, "暂存缺失，先下载最新 release…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		client := &http.Client{}
		rel, ferr := fetchRelease(ctx, client, repoAPI)
		if ferr != nil {
			return ferr
		}
		fmt.Fprintf(stdout, "最新版本 %s\n", strings.TrimPrefix(rel.Tag, "v"))
		if _, ferr = stageUpdate(ctx, client, rel, stageDir, nil); ferr != nil {
			return ferr
		}
	}
	if err := installStaged(stageDir, nil, *force); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "已安装并启动接力进程（旧网关退出后新版自动接管）")
	return nil
}
