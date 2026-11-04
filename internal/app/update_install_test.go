// M11-W3 安装引擎测试：换侧矩阵（双 exe/单 exe/失败还原）、锁单飞、
// 暂存定位、进程等待原语。spawn/接力/回滚全链路属进程级行为，由 W4
// 真机演练覆盖（app.md §十一），单测覆盖其可参数化的纯文件层。
package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedSwapDir 造换侧现场：exeDir 放旧双 exe（可选 app），stageDir 放
// 新版暂存（VERSION + 双 exe，可选省略 app）。
func seedSwapDir(t *testing.T, withOldApp, withStagedApp bool) (exeDir, stageDir string) {
	t.Helper()
	base := t.TempDir()
	exeDir, stageDir = filepath.Join(base, "bin"), filepath.Join(base, "update")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exeDir, gatewayExeName), []byte("OLD-GW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if withOldApp {
		if err := os.WriteFile(filepath.Join(exeDir, appExeName), []byte("OLD-APP"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stageDir, gatewayExeName), []byte("NEW-GW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "VERSION"), []byte("v9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withStagedApp {
		if err := os.WriteFile(filepath.Join(stageDir, appExeName), []byte("NEW-APP"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return exeDir, stageDir
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 双 exe 换侧：原位=新字节、.old=旧字节；暂存被搬空。
func TestSwapAsideBothExes(t *testing.T) {
	exeDir, stageDir := seedSwapDir(t, true, true)
	appSwapped, err := swapAside(
		filepath.Join(stageDir, gatewayExeName), filepath.Join(stageDir, appExeName),
		filepath.Join(exeDir, gatewayExeName), filepath.Join(exeDir, appExeName))
	if err != nil || !appSwapped {
		t.Fatalf("swapAside: swapped=%v err=%v", appSwapped, err)
	}
	if got := readAll(t, filepath.Join(exeDir, gatewayExeName)); got != "NEW-GW" {
		t.Fatalf("原位应为新网关，得到 %q", got)
	}
	if got := readAll(t, filepath.Join(exeDir, gatewayExeName+oldSuffix)); got != "OLD-GW" {
		t.Fatalf(".old 应为旧网关，得到 %q", got)
	}
	if got := readAll(t, filepath.Join(exeDir, appExeName)); got != "NEW-APP" {
		t.Fatalf("原位应为新桌面入口，得到 %q", got)
	}
	if got := readAll(t, filepath.Join(exeDir, appExeName+oldSuffix)); got != "OLD-APP" {
		t.Fatalf(".old 应为旧桌面入口，得到 %q", got)
	}
	if _, serr := os.Stat(filepath.Join(stageDir, gatewayExeName)); !os.IsNotExist(serr) {
		t.Fatal("暂存网关应已搬空")
	}
}

// 暂存无 app exe：只换网关，原位 app 不动。
func TestSwapAsideGatewayOnly(t *testing.T) {
	exeDir, stageDir := seedSwapDir(t, true, false)
	appSwapped, err := swapAside(
		filepath.Join(stageDir, gatewayExeName), "",
		filepath.Join(exeDir, gatewayExeName), filepath.Join(exeDir, appExeName))
	if err != nil || appSwapped {
		t.Fatalf("swapAside: swapped=%v err=%v", appSwapped, err)
	}
	if got := readAll(t, filepath.Join(exeDir, appExeName)); got != "OLD-APP" {
		t.Fatalf("原桌面入口不应被动，得到 %q", got)
	}
	if _, serr := os.Stat(filepath.Join(exeDir, appExeName+oldSuffix)); !os.IsNotExist(serr) {
		t.Fatal("app exe 未换侧不应产生 .old")
	}
}

// 暂存网关缺失：moveIn 失败必须整体还原（原位恒有可执行文件）。
func TestSwapAsideFailureRestores(t *testing.T) {
	exeDir, stageDir := seedSwapDir(t, true, true)
	_, err := swapAside(
		filepath.Join(stageDir, "不存在的网关"), filepath.Join(stageDir, appExeName),
		filepath.Join(exeDir, gatewayExeName), filepath.Join(exeDir, appExeName))
	if err == nil || !strings.Contains(err.Error(), "就位失败") {
		t.Fatalf("应报就位失败，得到 %v", err)
	}
	if got := readAll(t, filepath.Join(exeDir, gatewayExeName)); got != "OLD-GW" {
		t.Fatalf("失败后原位应还原旧网关，得到 %q", got)
	}
	if got := readAll(t, filepath.Join(exeDir, appExeName)); got != "OLD-APP" {
		t.Fatalf("失败后原位应还原旧桌面入口，得到 %q", got)
	}
	for _, name := range []string{gatewayExeName, appExeName} {
		if _, serr := os.Stat(filepath.Join(exeDir, name+oldSuffix)); !os.IsNotExist(serr) {
			t.Fatalf("%s 还原后不应残留 .old", name)
		}
	}
}

// 上一轮残留 .old 不阻塞新一轮换侧（aside 先清残留）。
func TestSwapAsideClearsStaleOld(t *testing.T) {
	exeDir, stageDir := seedSwapDir(t, false, false)
	stale := filepath.Join(exeDir, gatewayExeName+oldSuffix)
	if err := os.WriteFile(stale, []byte("STALE"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := swapAside(
		filepath.Join(stageDir, gatewayExeName), "",
		filepath.Join(exeDir, gatewayExeName), filepath.Join(exeDir, appExeName)); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, stale); got != "OLD-GW" {
		t.Fatalf("残留 .old 应被本轮旧字节覆盖，得到 %q", got)
	}
}

// stagePaths 定位：完整/缺 VERSION/缺网关/app 可缺。
func TestStagePathsMatrix(t *testing.T) {
	_, stageDir := seedSwapDir(t, false, false)
	gw, ap, ver, err := stagePaths(stageDir)
	if err != nil || ver != "v9.9.9" || !strings.HasSuffix(gw, gatewayExeName) || ap != "" {
		t.Fatalf("stagePaths: gw=%q app=%q ver=%q err=%v", gw, ap, ver, err)
	}
	if _, _, _, err = stagePaths(t.TempDir()); err == nil || !strings.Contains(err.Error(), "VERSION") {
		t.Fatalf("缺 VERSION 应报错，得到 %v", err)
	}
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "VERSION"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = stagePaths(empty); err == nil || !strings.Contains(err.Error(), gatewayExeName) {
		t.Fatalf("缺网关 exe 应报错，得到 %v", err)
	}
}

// 锁单飞：正常获取释放；死 pid 残留锁自动清除；存活 pid 拒绝。
func TestInstallLockLifecycle(t *testing.T) {
	stageDir := t.TempDir()
	release, err := installLock(stageDir)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, serr := os.Stat(filepath.Join(stageDir, ".install.lock")); !os.IsNotExist(serr) {
		t.Fatal("释放后锁文件应删除")
	}
	// 残留锁（死 pid）：清除重建。
	lockPath := filepath.Join(stageDir, ".install.lock")
	if err := os.WriteFile(lockPath, []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err = installLock(stageDir)
	if err != nil {
		t.Fatalf("死 pid 残留锁应自动清除: %v", err)
	}
	release()
	// 存活 pid：拒绝（父进程 go test 运行器在测试期间存活）。
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getppid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = installLock(stageDir); err == nil || !strings.Contains(err.Error(), "进行中") {
		t.Fatalf("存活 pid 锁应拒绝，得到 %v", err)
	}
	_ = os.Remove(lockPath)
}

// waitForPID：不存在 pid 立即返回（视作已退出）。
func TestWaitForPIDDeadImmediately(t *testing.T) {
	start := time.Now()
	if err := waitForPID(999999999, 5*time.Second); err != nil {
		t.Fatalf("不存在的 pid 应视作已退出: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("已退出 pid 不应等待")
	}
}

// pidAlive：本进程存活。
func TestPidAliveSelf(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("本进程应存活")
	}
}

// relayInit：非法标记按普通启动。
func TestRelayInitGarbageEnv(t *testing.T) {
	t.Setenv(relayEnv, "not-a-pid")
	if st := relayInit(); st.oldPid != 0 {
		t.Fatalf("非法标记应返回零值，得到 %+v", st)
	}
	if _, ok := os.LookupEnv(relayEnv); ok {
		t.Fatal("处理后应清除接力环境变量")
	}
}
