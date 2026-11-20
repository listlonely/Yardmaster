//go:build windows && amd64

// WebView2 渲染面同步保障（2026-11-19/20 窄条根治终版：像素探针伺服）。
//
// 现象：窗口创建/最大化后，页面以非确定概率只渲染在左侧窄条（实测
// ≈470/958 不等，窗口本体铺满，右侧为背景色）；手动拖拽窗口边缘必恢复。
//
// 根因（三层叠加，v3.1.0 实读源码确认）：
//   1. WebView2 光栅化面/视口卡在 controller 附件早期尺寸——上游已知
//      缺陷（WebView2Feedback #5544/#5605，"until a manual resize"），
//      离散的程序化 resize（单次 SetWindowPos / SW_MAXIMIZE 一步到位）
//      可能被吞，交互式拖拽的每帧连续 WM_SIZE 流才必然重算；
//   2. wails 首次 NavigationCompleted 做 Hide()+Show()（上游自述
//      "Hack to make it visible"，#1077；v3.1.0 仍在）——若导航完成
//      晚于任何一次性修复，Show() 按内部过期 client rect 重附渲染树，
//      把修复成果打回窄条（17:10 复现实证）；
//   3. 本壳「恢复窗口状态 + 延迟 Maximise + 页面异步加载」三异步事件
//      时序互斥——任何固定时间表的修复都在跟竞态赛跑。
//
// 检测（v2，17:36 误报教训）：窄条病发生在 Chrome_WidgetWin_* 内部
// 渲染层，外壳几何永远正确跟随 resize——几何比对必然误报「收敛」
//（17:36:30 伺服收敛 + PrintWindow 实拍仍窄条实锤）。检测必须下沉到
// 渲染像素：GetDC(Chrome 子窗口)+GetPixel 采样右侧 header 带（x 60%
// ~95%、y 6%~14% 共 24 点），全部≈右上角背景基准色即判窄条（正常
// 布局该带必有卡片/文字像素混入）；判读失败的像素（CLR_INVALID）按
// 非背景计，避免瞎修。几何比对保留为兜底（外层也卡的场景）。
//
// 修复（v4，真实鼠标拖拽）：PostMessage 伪造 ENTER/EXIT 消息对跨进程
// 被忽略（v3 测试实锤：伺服循环在跑、修复后复验仍窄条）。手动拖拽
// 100% 有效源自真实鼠标输入流触发的系统 modal resize 循环——
// webviewDragResize 用 SendInput 按下 + SetCursorPos 分步横移 + 抬起
// 复刻同一输入流（非客户区点击=激活+直接进入尺寸循环，后台窗口同样
// 生效）。zoomed 态修复走 UnMaximise→(拖拽)→Maximise（对 zoomed 窗口
// SetWindowPos 传尺寸=还原，禁止直传）。
//
// 伺服主循环：每 500ms 跑「几何比对 ∨ header 探针」，连续 2 轮异常
// 才动手（扛拖拽过渡态/探针毛刺），修复后复验，连续 3 轮正常收敛；
// 总预算 25s 覆盖最慢冷启动导航。误判代价=一次无害闪动，可接受。
package app

import (
	"log/slog"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32Sync           = syscall.NewLazyDLL("user32.dll")
	procGetWindowRectSyn = user32Sync.NewProc("GetWindowRect")
	procSetWindowPosSync = user32Sync.NewProc("SetWindowPos")
	procEnumChildSync    = user32Sync.NewProc("EnumChildWindows")
	procGetClassNameSync = user32Sync.NewProc("GetClassNameW")
	procIsZoomedSync     = user32Sync.NewProc("IsZoomed")
	procGetClientRectSyn = user32Sync.NewProc("GetClientRect")
	procGetDCSyn         = user32Sync.NewProc("GetDC")
	procReleaseDCSyn     = user32Sync.NewProc("ReleaseDC")
	procSetCursorPosSync = user32Sync.NewProc("SetCursorPos")
	procSetFGSync        = user32Sync.NewProc("SetForegroundWindow")
	procSendInputSync    = user32Sync.NewProc("SendInput")
	procGetSysMetSync    = user32Sync.NewProc("GetSystemMetrics")
	procShowWindowSync   = user32Sync.NewProc("ShowWindow")
	procPostMessageSyn   = user32Sync.NewProc("PostMessageW")
	procWinFromPtSync    = user32Sync.NewProc("WindowFromPoint")
	procIsChildSync      = user32Sync.NewProc("IsChild")
	procGetFGSync        = user32Sync.NewProc("GetForegroundWindow")
	procSendMessageSync  = user32Sync.NewProc("SendMessageW")
	procGetTIDSync       = user32Sync.NewProc("GetWindowThreadProcessId")
	procAttachTISync     = user32Sync.NewProc("AttachThreadInput")
	procGetDPISync       = user32Sync.NewProc("GetDpiForSystem")
	// GetCurrentThreadId 属 kernel32 而非 user32。
	kernel32Sync    = syscall.NewLazyDLL("kernel32.dll")
	procCurrTIDSyn  = kernel32Sync.NewProc("GetCurrentThreadId")
	// GetPixel 属 GDI 而非 user32（08:35 panic 实锤：放错 DLL 直接
	// LazyProc.mustFind 崩进程）。
	gdi32Sync       = syscall.NewLazyDLL("gdi32.dll")
	procGetPixelSyn = gdi32Sync.NewProc("GetPixel")
)

// SW_RESTORE（ShowWindow 还原参数；v6 实测 wails UnMaximise 包装层
// InvokeSync 返回后窗口仍 WS_MAXIMIZE，绕过包装层直调才落地）。
const (
	swRestoreSync    = 9
	wmSysCommandSync = 0x0112
	scRestoreSync    = 0xF120 // SC_RESTORE：系统菜单「还原」命令
	scSizeRightSync  = 0xF002 // SC_SIZE|2：系统菜单「大小」调右边缘
	wmNCHitTestSync  = 0x0084 // WM_NCHITTEST：点位区域查询
	htRightSync      = 11     // HTRIGHT：右边缘可拖热区
	smCXScreenSync   = 0      // SM_CXSCREEN：主屏宽
)

// SetWindowPos 标志：不移动位置、不改 Z 序、不抢焦点。
const (
	swpNoMoveSync     = 0x0002
	swpNoZOrderSync   = 0x0004
	swpNoActivateSync = 0x0010
	swpNoSizeSync     = 0x0001
	swpShowSync       = 0x0040
)

// HWND_TOPMOST/HWND_NOTMOST（x64 负句柄）；Z 序提升不需要前台权限，
// 是后台伺服窗口摆脱遮挡的正路。
const (
	hwndTopmostSync = ^uintptr(0) // -1
	hwndNotmostSync = ^uintptr(1) // -2
)

// 真实鼠标输入流（SendInput）常量与 win32 INPUT/MOUSEINPUT 布局镜像
//（amd64：Type 后 4 字节对齐填充，INPUT 总长 40 字节）。
const (
	inputMouse    = 0
	mefLeftDown   = 0x0002
	mefLeftUp     = 0x0004
	smCXFrameSync = 32 // SM_CXFRAME：可调边框宽
	smCXPadBrd    = 92 // SM_CXPADDEDBORDER：DPI 加宽部分
	smCYFullScr   = 17 // SM_CYFULLSCREEN：屏高-任务栏（工作区高）
)

type mouseInputSync struct {
	DX, DY    int32
	MouseData uint32
	DwFlags   uint32
	Time      uint32
	ExtraInfo uintptr
}

type inputSync struct {
	Type uint32
	_    [4]byte
	Mi   mouseInputSync
}

// webviewChildWMax 是 EnumChildWindows 回调的宽度收集槽（屏坐标 px，
// 与顶层 GetWindowRect 同基准可比）：回调与 EnumChildWindows.Call 同
// 线程同步执行，且本壳单实例单窗口（SingleInstance 锁），包级原子槽
// 即并发安全——规避 lparam 指针往返的 vet unsafe.Pointer 误用告警。
var webviewChildWMax atomic.Int32

// webviewChromeHWNDs 收集全部 Chrome_WidgetWin_* 后代窗口句柄（0=宿
// 主层级根，1/2=内部渲染层），探针逐个 GetDC 采样取并集视图。
var webviewChromeHWNDs atomic.Value // store []uintptr

// webviewChildEnumCB 是 EnumChildWindows 回调：命中 Chrome_WidgetWin_*
// 时记录宽度最大值与句柄清单。包级注册一次（NewCallback 限额）。
var webviewChildEnumCB = syscall.NewCallback(func(hwnd, lparam uintptr) uintptr {
	var buf [64]uint16
	if n, _, _ := procGetClassNameSync.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 {
		if cls := syscall.UTF16ToString(buf[:n]); len(cls) >= 17 && cls[:17] == "Chrome_WidgetWin_" {
			var r struct{ L, T, R, B int32 }
			if rp, _, _ := procGetWindowRectSyn.Call(hwnd, uintptr(unsafe.Pointer(&r))); rp != 0 && r.R > r.L {
				if w := int32(r.R - r.L); w > webviewChildWMax.Load() {
					webviewChildWMax.Store(w)
				}
			}
			if hs, ok := webviewChromeHWNDs.Load().([]uintptr); ok {
				webviewChromeHWNDs.Store(append(hs, hwnd))
			} else {
				webviewChromeHWNDs.Store([]uintptr{hwnd})
			}
		}
	}
	return 1 // 继续枚举
})

// windowRect 读顶层窗口矩形宽高（失败/异常值返回 false）。最小化
//（160×28）等异常几何不参与判定，避免伺服对最小化态误修。
func windowRect(topHwnd uintptr) (w, h int, ok bool) {
	if topHwnd == 0 {
		return 0, 0, false
	}
	var r struct{ L, T, R, B int32 }
	if rp, _, _ := procGetWindowRectSyn.Call(topHwnd, uintptr(unsafe.Pointer(&r))); rp == 0 || r.R <= r.L || r.B <= r.T {
		return 0, 0, false
	}
	w, h = int(r.R-r.L), int(r.B-r.T)
	if w < 200 || h < 200 || w > 20000 || h > 20000 {
		return 0, 0, false
	}
	return w, h, true
}

// webviewChromeSnapshot 枚举顶层窗口后代，返回 Chrome_WidgetWin_*
// 句柄清单与最大窗口宽（找不到=false：WebView2 未创建/已销毁）。
func webviewChromeSnapshot(topHwnd uintptr) (hwnds []uintptr, maxW int, ok bool) {
	webviewChildWMax.Store(0)
	webviewChromeHWNDs.Store([]uintptr(nil))
	rp, _, _ := procEnumChildSync.Call(topHwnd, webviewChildEnumCB, 0)
	hwnds, _ = webviewChromeHWNDs.Load().([]uintptr)
	maxW = int(webviewChildWMax.Load())
	if maxW <= 0 || len(hwnds) == 0 || rp == 0 {
		return nil, 0, false
	}
	return hwnds, maxW, true
}

// isZoomedWindow 查窗口最大化态（IsZoomed；最小化时反映其还原目标态）。
func isZoomedWindow(topHwnd uintptr) bool {
	r, _, _ := procIsZoomedSync.Call(topHwnd)
	return r != 0
}

// webviewPixelProbe 对单个 Chrome 子窗口客户区做 header 带采样：返回
// （背景基准色、右侧带 24 点色值列表、客户区宽高）。读不到 DC 或客户
// 区异常返回 false。坐标系=GetDC 客户区。
func webviewPixelProbe(hwnd uintptr) (bg uint32, pts []uint32, cw, ch int, ok bool) {
	var cr struct{ L, T, R, B int32 }
	if rp, _, _ := procGetClientRectSyn.Call(hwnd, uintptr(unsafe.Pointer(&cr))); rp == 0 || cr.R-cr.L < 200 || cr.B-cr.T < 120 {
		return 0, nil, 0, 0, false
	}
	cw, ch = int(cr.R-cr.L), int(cr.B-cr.T)
	hdc, _, _ := procGetDCSyn.Call(hwnd)
	if hdc == 0 {
		return 0, nil, 0, 0, false
	}
	defer procReleaseDCSyn.Call(hwnd, hdc)
	read := func(x, y int) (uint32, bool) {
		v, _, _ := procGetPixelSyn.Call(hdc, uintptr(x), uintptr(y))
		return uint32(v), v != 0xFFFFFFFF // CLR_INVALID=不可读
	}
	// 背景基准：右上角边缘（header 带上方 padding，窄条/正常两态都是
	// 窗口背景色）。读不到=探针放弃（不判窄）。
	bg, okBg := read(cw-3, 3)
	if !okBg {
		return 0, nil, 0, 0, false
	}
	// 右侧 header 带 4×6 网格采样。
	for iy := 0; iy < 6; iy++ {
		y := ch * (6 + iy*2) / 100 // y ∈ 6%~20%
		for ix := 0; ix < 4; ix++ {
			x := cw * (60 + ix*12) / 100 // x ∈ 60%~96%
			if v, rOK := read(x, y); rOK {
				pts = append(pts, v)
			}
		}
	}
	if len(pts) == 0 {
		return 0, nil, 0, 0, false
	}
	return bg, pts, cw, ch, true
}

// webviewHeaderLooksBlank 渲染层窄条探针：任一 Chrome 子窗口的右侧
// header 带出现非背景像素即「有内容」（false）；全部子窗口可读且右侧
// 带 ≥90% 像素≈背景基准（每通道容差 2）才判窄条（true）。无任何可读
// 子窗口=状态未知（false，不瞎修）。
func webviewHeaderLooksBlank(topHwnd uintptr) bool {
	hwnds, _, ok := webviewChromeSnapshot(topHwnd)
	if !ok {
		return false
	}
	readable := 0
	blankVotes := 0
	for _, h := range hwnds {
		bg, pts, _, _, pOK := webviewPixelProbe(h)
		if !pOK {
			continue
		}
		readable++
		same := 0
		for _, v := range pts {
			br, bgg, bb := int(bg&0xFF), int(bg>>8&0xFF), int(bg>>16&0xFF)
			pr, pg, pb := int(v&0xFF), int(v>>8&0xFF), int(v>>16&0xFF)
			if absDiff(pr, br) <= 2 && absDiff(pg, bgg) <= 2 && absDiff(pb, bb) <= 2 {
				same++
			}
		}
		if same*10 >= len(pts)*9 { // ≥90% ≈背景
			blankVotes++
		} else {
			return false // 任一子窗口右侧有内容 = 未窄
		}
	}
	return readable > 0 && blankVotes == readable
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// fakeMaxRectLogical 计算模拟最大化的窗口几何（逻辑 DIP，与 wails
// options/Bounds 同语义）。复刻真实最大化 rect 形状（本机 100% DPI 实
// 测：x=-边框、y=0、宽=屏宽+2×边框、高=工作区高+边框——左右下三边
// 出屏悬挂，标题栏完整可见）。物理量取自 GetSystemMetrics（进程 DPI
// 感知上下文与 GetDpiForSystem 自洽：感知进程两者皆物理，非感知进程
// 两者皆已虚拟化为逻辑），逻辑=物理/DPI 缩放。
// 假设（本机成立）：任务栏贴底、主屏为唯一显示器；多显示器场景假最
// 大化固定落主屏（调用方留档此限制）。
func fakeMaxRectLogical() (x, y, w, h int) {
	cx, _, _ := procGetSysMetSync.Call(smCXScreenSync)
	cyFS, _, _ := procGetSysMetSync.Call(smCYFullScr)
	frame, _, _ := procGetSysMetSync.Call(smCXFrameSync)
	dpi, _, _ := procGetDPISync.Call()
	if dpi == 0 {
		dpi = 96
	}
	sc := float64(dpi) / 96
	lg := func(v uintptr) int { return int(float64(v)/sc + 0.5) }
	return -lg(frame), 0, lg(cx) + 2*lg(frame), lg(cyFS) + lg(frame)
}

// 拖拽节奏与幅度：每步 ~90ms（modal resize 循环逐帧跟踪光标），水平
// 摆幅 120px 与手动有效拖动同量级。
const (
	dragStepPause = 90 * time.Millisecond
	dragAmplitude = 120
)

// webviewSendMouseButton 经 SendInput 注入左键按下/抬起——与真实输入
// 流同源，DefWindowProc 据此进入 modal resize 循环（PostMessage 伪造
// 消息对跨进程被忽略的替代路径）。
func webviewSendMouseButton(down bool) {
	var in inputSync
	in.Type = inputMouse
	if down {
		in.Mi.DwFlags = mefLeftDown
	} else {
		in.Mi.DwFlags = mefLeftUp
	}
	sent, _, _ := procSendInputSync.Call(1, uintptr(unsafe.Pointer(&in)), uintptr(unsafe.Sizeof(in)))
	if sent != 1 {
		slog.Warn("SendInput 注入被拒", "down", down, "sent", sent)
	}
}

// webviewDragResize 程序化复刻「手动拖拽右边缘」：光标落到右边框中
// 点 → 左键按下 → 分步横移再回位 → 抬起。按下到抬起全程处于系统
// modal resize 循环内（真实输入流专属效果），WebView2 收到连续
// WM_SIZE 流后逐帧重光栅化。zoomed 态先直调 SW_RESTORE 还原再拖。
// 光标置位失败即放弃——宁可少修一次也不乱点。
func webviewDragResize(topHwnd uintptr) {
	// 置顶+前台：伺服后台触发时窗口常被其他窗口遮挡（CopyFromScreen
	// 实证拍到壁纸/TRAE——点击会落在覆盖窗口上，既无效又危险）。Z 序
	// 提升不需要前台权限；SetForegroundWindow 受前台锁尽力而为，点击
	// 激活补足。
	procSetWindowPosSync.Call(topHwnd, hwndTopmostSync, 0, 0, 0, 0,
		uintptr(swpNoMoveSync|swpNoSizeSync|swpShowSync))
	procSetFGSync.Call(topHwnd)
	time.Sleep(120 * time.Millisecond)
	defer procSetWindowPosSync.Call(topHwnd, hwndNotmostSync, 0, 0, 0, 0,
		uintptr(swpNoMoveSync|swpNoSizeSync))
	// 输入队列共享（v18）：v12 实锤 SendInput 返回 1、前台归属正确、
	// 落点在窗口内，按下仍进不了 modal resize 循环（零摆动）——合成
	// 输入未被目标线程当作自身输入处理。AttachThreadInput 把本线程
	// 挂进窗口线程的输入队列共享组，注入点击即按目标线程自身输入
	// 派发（真实手动点击的等价通道）。
	var pidSync uintptr
	tidTarget, _, _ := procGetTIDSync.Call(topHwnd, uintptr(unsafe.Pointer(&pidSync)))
	tidSelf, _, _ := procCurrTIDSyn.Call()
	attachedSync := false
	if tidTarget != 0 && tidTarget != tidSelf {
		r, _, _ := procAttachTISync.Call(tidSelf, tidTarget, 1)
		attachedSync = r != 0
		if !attachedSync {
			slog.Warn("AttachThreadInput 失败", "tidTarget", tidTarget)
		}
	}
	if attachedSync {
		defer procAttachTISync.Call(tidSelf, tidTarget, 0)
	}
	// zoomed 窗口无可拖边框（最大化边缘在屏幕外，hit-test 不命中——
	// v6 实测 wMin=wMax=w0=1936 零摆动=点击全空）。先还原再拖；
	// ShowWindow 跨线程按投递处理，未生效再投 SC_RESTORE（系统菜单
	// 命令，走消息队列必达）双保险，仍 zoomed 判主线程异常放弃本轮。
	zoomedIn := isZoomedWindow(topHwnd)
	if zoomedIn {
		sr, _, _ := procShowWindowSync.Call(topHwnd, swRestoreSync)
		time.Sleep(120 * time.Millisecond)
		if isZoomedWindow(topHwnd) {
			slog.Warn("SW_RESTORE 未生效，改投 SC_RESTORE", "ret", sr)
			procPostMessageSyn.Call(topHwnd, wmSysCommandSync, scRestoreSync, 0)
			time.Sleep(300 * time.Millisecond)
		}
		for i := 0; i < 20 && isZoomedWindow(topHwnd); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if isZoomedWindow(topHwnd) {
			slog.Warn("窗口拒绝还原（还原命令全部无效），放弃本轮拖拽", "zoomedIn", zoomedIn)
			return
		}
		slog.Info("还原阶段完成")
		time.Sleep(300 * time.Millisecond) // 还原动画/重排余量
	}
	var r struct{ L, T, R, B int32 }
	if rp, _, _ := procGetWindowRectSyn.Call(topHwnd, uintptr(unsafe.Pointer(&r))); rp == 0 || r.R <= r.L || r.B <= r.T {
		return
	}
	w := int(r.R - r.L)
	if w-2*dragAmplitude < 200 {
		return
	}
	// 还原态尺寸仍=最大化值（毒化 rcNormalPosition：状态文件把最大化
	// rect 存成了还原尺寸）→ 右边框出屏，拖拽必空（v8 实证：IsZoomed
	// 已 false 但 rect 1936>屏宽 1920）。先离散收缩把边框带回屏幕内
	// 再拖；外壳几何跟随离散 resize 从不失灵（窄条病只在内部渲染层），
	// 此举无害。
	smx, _, _ := procGetSysMetSync.Call(smCXScreenSync)
	if int(r.R) >= int(smx) {
		nw := w - 2*dragAmplitude
		if nw < 200 {
			return
		}
		procSetWindowPosSync.Call(topHwnd, 0, 0, 0,
			uintptr(nw), uintptr(r.B-r.T), uintptr(swpNoMoveSync|swpNoZOrderSync|swpNoActivateSync))
		time.Sleep(300 * time.Millisecond)
		if rp2, _, _ := procGetWindowRectSyn.Call(topHwnd, uintptr(unsafe.Pointer(&r))); rp2 == 0 || r.R <= r.L {
			return
		}
		w = int(r.R - r.L)
		slog.Info("出屏边框已收缩", "w", w)
	}
	// 不可见拖拽边宽 = SM_CXFRAME + SM_CXPADDEDBORDER（随 DPI 缩放）。
	fx, _, _ := procGetSysMetSync.Call(smCXFrameSync)
	pb, _, _ := procGetSysMetSync.Call(smCXPadBrd)
	border := int(fx) + int(pb)
	if border <= 0 {
		border = 8
	}
	gx := int(r.R) - border/2 // 右边框中线
	gy := (int(r.T) + int(r.B)) / 2
	procSetFGSync.Call(topHwnd)
	time.Sleep(60 * time.Millisecond)
	if sp, _, _ := procSetCursorPosSync.Call(uintptr(gx), uintptr(gy)); sp == 0 {
		slog.Warn("WebView2 拖拽修复光标置位失败（输入注入被拒？）", "gx", gx, "gy", gy)
		return
	}
	// 落点归属验证：光标点必须是本窗口或其子窗口，否则说明遮挡未解除
	// 或置位失败——宁可放弃也绝不把左键点进其他应用。
	ptPacked := uintptr(uint32(gx)) | uintptr(uint32(gy))<<32
	ptHwnd, _, _ := procWinFromPtSync.Call(ptPacked)
	if ptHwnd != topHwnd {
		if ic, _, _ := procIsChildSync.Call(topHwnd, ptHwnd); ic == 0 {
			slog.Warn("拖拽落点非本窗口（遮挡未解除？），放弃", "pt", ptHwnd)
			return
		}
	}
	time.Sleep(dragStepPause)
	// 预激活点击：后台窗口首击若被激活语义吞掉（客户区惯例），这里先
	// 消耗掉；若 NC 边框首击直接进 modal 循环，静置微动无害。
	webviewSendMouseButton(true)
	time.Sleep(dragStepPause)
	webviewSendMouseButton(false)
	time.Sleep(dragStepPause)
	procSetCursorPosSync.Call(uintptr(gx), uintptr(gy))
	time.Sleep(dragStepPause)
	if fgHwnd, _, _ := procGetFGSync.Call(); fgHwnd != topHwnd {
		slog.Warn("预激活后窗口仍未成为前台", "fg", fgHwnd)
	}
	// hit-test 探针（v12 排除法终点：SendInput 返回正常+前台正确+落点
	// 在本窗口内且按下齐全仍零摆动，最后一块未知=该点是否真为 HTRIGHT
	// 热区——DPI 虚拟化错位/Chrome 子窗口覆盖边框/自定义 hit-test 都会
	// 把点击吞成页面左击）。直接问窗口拿裁决，并据此分流修复通道。
	htTop, _, _ := procSendMessageSync.Call(topHwnd, wmNCHitTestSync, 0, ptPacked)
	slog.Info("边框 hit-test 实测", "ht", htTop, "gx", gx, "gy", gy, "R", int(r.R), "border", border)
	time.Sleep(dragStepPause)
	// 摆动追踪：modal resize 循环一旦进入，窗口宽度必然跟随光标横移
	// 变化——wMin==wMax==w0 即循环没进去（v12 补上摆动前缺失的按下
	// 仍零摆动，v13 据探针结果分流双通道）。
	swing := func() (wMin, wMax int) {
		wMin, wMax = w, w
		for _, dx := range [5]int{-dragAmplitude, -dragAmplitude / 2, 0, -dragAmplitude / 2, 0} {
			procSetCursorPosSync.Call(uintptr(gx+dx), uintptr(gy))
			time.Sleep(dragStepPause)
			var rr struct{ L, T, R, B int32 }
			if rp, _, _ := procGetWindowRectSyn.Call(topHwnd, uintptr(unsafe.Pointer(&rr))); rp != 0 && rr.R > rr.L {
				hw := int(rr.R - rr.L)
				if hw < wMin {
					wMin = hw
				}
				if hw > wMax {
					wMax = hw
				}
			}
		}
		return wMin, wMax
	}
	var wMin, wMax int
	if htTop == htRightSync {
		// 命中热区：按住→摆动→抬起，复刻真实拖边（modal resize 循环
		// 原生入口，v12 已补齐按下缺位）。
		webviewSendMouseButton(true)
		time.Sleep(dragStepPause)
		wMin, wMax = swing()
		webviewSendMouseButton(false)
	} else {
		// 未命中热区：SC_SIZE|2 系统菜单命令直接命令窗口进入 sizing
		// modal 循环（键盘 Alt+Space→大小 同源路径，不依赖边框命中；
		// 同族 SC_RESTORE 跨线程投递已被还原阶段实证可达）。循环跟踪
		// 光标横移调整右边缘，末尾点击确认退出。
		procPostMessageSyn.Call(topHwnd, wmSysCommandSync, scSizeRightSync, 0)
		time.Sleep(dragStepPause)
		wMin, wMax = swing()
		webviewSendMouseButton(true)
		time.Sleep(dragStepPause)
		webviewSendMouseButton(false)
	}
	time.Sleep(dragStepPause)
	slog.Info("WebView2 拖拽修复序列完成", "ht", htTop, "w0", w, "wMin", wMin, "wMax", wMax)
}

// 伺服参数：轮距/确认轮数/容差/总预算。容差 48px 吸收边框与 DPI 取整
//（窄条差距是数百 px 级，绝不会误判）；总预算 25s 覆盖最慢冷启动导航
//（NavigationCompleted 打回只发生一次，伺服轮次必然追上）。
const (
	servoRoundGap    = 500 * time.Millisecond
	servoConfirmTrip = 2  // 连续 N 轮异常才动手（扛交互式拖拽过渡态）
	servoConvergeOK  = 3  // 连续 N 轮正常即收敛
	servoTolerancePX = 48 // 宽度一致容差（物理 px）
	servoDeadline    = 25 * time.Second
)

// webviewDumpDiagnostics 一次性诊断埋点（v19 屏拍窄条但伺服收敛的判
// 定盲区取证）：逐个 Chrome 子窗口记录外框/客户区/背景基准/右侧带非背
// 景命中数，定位「哪个窗口卡住、探针在读什么」。
func webviewDumpDiagnostics(topHwnd uintptr) {
	slog.Info("诊断:进入")
	tw, th, okT := windowRect(topHwnd)
	var tc struct{ L, T, R, B int32 }
	procGetClientRectSyn.Call(topHwnd, uintptr(unsafe.Pointer(&tc)))
	slog.Info("诊断:顶层", "w", tw, "h", th, "client", (tc.R-tc.L), "ch", (tc.B-tc.T), "ok", okT)
	hwnds, maxW, ok := webviewChromeSnapshot(topHwnd)
	if !ok {
		slog.Info("诊断:无Chrome子窗口")
		return
	}
	slog.Info("诊断:子窗口", "n", len(hwnds), "maxW", maxW)
	for i, h := range hwnds {
		var r struct{ L, T, R, B int32 }
		procGetWindowRectSyn.Call(h, uintptr(unsafe.Pointer(&r)))
		bg, pts, cw, ch, pok := webviewPixelProbe(h)
		if !pok {
			slog.Info("诊断:子窗口", "i", i, "rectW", (r.R - r.L), "rectH", (r.B - r.T), "pok", false)
			continue
		}
		br, bgg, bb := int(bg&0xFF), int(bg>>8&0xFF), int(bg>>16&0xFF)
		non := 0
		for _, v := range pts {
			if absDiff(int(v&0xFF), br) > 2 || absDiff(int(v>>8&0xFF), bgg) > 2 || absDiff(int(v>>16&0xFF), bb) > 2 {
				non++
			}
		}
		slog.Info("诊断:子窗口", "i", i, "rectW", (r.R - r.L), "rectH", (r.B - r.T), "cw", cw, "ch", ch, "bg", bg, "bandNonBg", non, "of", len(pts))
	}
}

// webviewSurfaceServo 闭环伺服主循环：每轮「几何比对 ∨ 渲染探针」，
// 持续异常达确认轮数即调 repair（修复后自然进入复验轮），正常达收敛
// 轮数或超预算返回。返回收敛=true / 超时=false（供壳层留观测日志）。
// 整体 recover 防护：探针/修复是 DLL 边界新代码，任何 panic 只降级为
// 「无伺服」（用户手动拖拽兜底），不崩整个壳进程。
func webviewSurfaceServo(topHwnd uintptr, repair func(zoomed bool)) (converged bool) {
	defer func() {
		if r := recover(); r != nil {
			converged = false
		}
	}()
	deadline := time.Now().Add(servoDeadline)
	slog.Info("诊断:伺服启动", "hwnd", topHwnd)
	okStreak, badStreak := 0, 0
	diagDone := false // 临时诊断埋点只跑一次：子窗口就绪后的首轮（v20b
	// 实锤 round0 时子窗口尚未创建、诊断门必错过，改为首个 okT&&okC 轮次）
	for round := 0; ; round++ {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(servoRoundGap)
		tw, _, okT := windowRect(topHwnd)
		_, cw, okC := webviewChromeSnapshot(topHwnd)
		if round == 0 {
			slog.Info("诊断:round0", "okT", okT, "okC", okC, "tw", tw, "cw", cw)
		}
		if okT && okC && !diagDone {
			diagDone = true
			webviewDumpDiagnostics(topHwnd) // 临时埋点
		}
		bad := false
		switch {
		case !okT || !okC:
			// 子窗口未创建/窗口最小化等：空转等待，不累积任何 streak
		case cw < tw-servoTolerancePX, webviewHeaderLooksBlank(topHwnd):
			bad = true // 外壳几何卡住（兜底）∨ 渲染层窄条（主病）
		}
		if !bad {
			okStreak++
			badStreak = 0
			if okStreak >= servoConvergeOK {
				return true
			}
			continue
		}
		badStreak++
		okStreak = 0
		if badStreak < servoConfirmTrip {
			continue
		}
		badStreak = 0
		z := isZoomedWindow(topHwnd)
		slog.Info("WebView2 渲染面异常，触发修复", "zoomed", z)
		repair(z)
		slog.Info("WebView2 修复动作返回，进入复验")
	}
}
