// Command genwinres 生成 Windows 资源与图标产物（M8-W5 建，M11 后期
// 换装 jimeng 图标）：单一事实源 appicon.png（jimeng 生成图裁剪——第一
// 排第三：黑底白色发光 S 形岔路箭头，用户 2026-11-04 定选）→
//
//  1. exe 图标资源（256/64/48/32/16 五尺寸，圆角透明）→ 两份 syso
//  2. 托盘多尺寸 ICO（internal/app/tray_icon.ico，白色圆角外框版——
//     黑底图标在深色任务栏隐形、浅色任务栏黑图可见；白框反之，双保险）
//  3. 通知图标 PNG（internal/app/notify_icon.png，beeep 要文件路径）
//
// 纯 Go（tc-hib/winres + x/image 缩放，wails 同源依赖），预生成后入库
// ——go build 按文件名后缀自动链接对应架构的 syso（amd64/386 各一份，
// CI/release 零改动）；GOOS=linux 构建按文件名忽略。重新生成：
// go generate ./cmd/yardmaster。
//
// 历史教训（本工具改造的动因）：托盘图标此前用代码生成 PNG 字节直接
// 喂 fyne.io/systray——其 Windows 端 SetIcon 经 LoadImage(LR_LOADFROMFILE)
// 只认 .ico 文件，PNG 解析失败且 GetLastError=0（M8 记录的「unable to
// set icon: The operation completed successfully」假阴性）——图标从未
// 显示过（2026-11-04 用户截图实证：tooltip 在、图标无）。托盘改喂 ICO。
// 注意：资源版本（appVersion）随发版更新；运行时版本仍由 release 流程
// -ldflags 注入 internal/app.Version，两者互不依赖。
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

// appVersion 嵌入 exe 属性的资源版本（三段，第四位补 0）。
const appVersion = "0.1.4"

// iconSrcCandidates 是图标单一事实源的候选路径：go:generate 的工作
// 目录是 cmd/yardmaster（指令所在包目录），手动跑本工具时是本目录——
// 依次尝试。内容=jimeng 生成图第一排第三的裁剪结果（2048px 源图裁
// [1380,290,530,530]——黑底白 S 岔路箭头）。
var iconSrcCandidates = []string{
	"../../tools/genwinres/appicon.png",
	"appicon.png",
}

// outTargets 输出对象文件（相对 go generate 的包目录 cmd/yardmaster）：
// 文件名带 GOARCH 后缀（winres 约定 "rsrc_windows_"+arch+".syso"），go
// build 按目标架构自动链接——amd64/386 各一份（COFF machine 字段随
// WriteObject 匹配；[2026-11-09-arch386] 起 386 为支持目标，缺失会导致
// 386 产物无图标/版本属性/DPI manifest）。console 与 GUI 两入口各两份。
var outTargets = []struct {
	rel  string
	arch winres.Arch
}{
	{"rsrc_windows_amd64.syso", winres.ArchAMD64},
	{"rsrc_windows_386.syso", winres.ArchI386},
	{"../yardmaster-app/rsrc_windows_amd64.syso", winres.ArchAMD64},
	{"../yardmaster-app/rsrc_windows_386.syso", winres.ArchI386},
}

// trayIcoOut / notifyPngOut 是托盘与通知产物（相对本包目录）。
var (
	trayIcoOut   = filepath.Join("..", "..", "internal", "app", "tray_icon.ico")
	notifyPngOut = filepath.Join("..", "..", "internal", "app", "notify_icon.png")
)

// traySizes 是托盘 ICO 帧尺寸（16/20/24/32/48：LoadImage 按
// SM_CXICON/SM_CXSMICON 取帧，DPI 缩放中间档一并覆盖）。
var traySizes = []int{16, 20, 24, 32, 48}

// cornerRadius 是源图标圆角占比（jimeng 图标为 iOS 风格圆角方块，
// 半径约边长 11.5%）。
const cornerRadius = 0.115

// loadAppIcon 读单一事实源并做圆角透明化（裁剪图四角残留原图白底，
// 圆角外区域置透明——exe 图标与托盘帧共用）。
func loadAppIcon() (*image.NRGBA, error) {
	var f *os.File
	var lastErr error
	for _, p := range iconSrcCandidates {
		var oerr error
		if f, oerr = os.Open(p); oerr == nil {
			break
		}
		lastErr = oerr
	}
	if f == nil {
		return nil, fmt.Errorf("图标源未找到（候选 %v）: %w", iconSrcCandidates, lastErr)
	}
	defer func() { _ = f.Close() }()
	src, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			// 圆角外（到最近两邻边圆心的距离 > r）置透明；边缘 1px
			// 线性过渡抗锯齿。
			r := cornerRadius * float64(w)
			cx, cy := clampf(float64(x), r, float64(w-1)-r), clampf(float64(y), r, float64(h-1)-r)
			if d := math.Hypot(float64(x)-cx, float64(y)-cy); d > r+1 {
				c.A = 0
			} else if d > r-1 {
				c.A = uint8(float64(c.A) * (r + 1 - d) / 2)
			}
			out.SetNRGBA(x, y, c)
		}
	}
	return out, nil
}

func clampf(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// trayFrame 画托盘帧：白色圆角底 + 图标缩放居中（四周留白 size*8%）。
// 白框保证深色任务栏可见；黑底图标本体保证浅色任务栏可见。
func trayFrame(src image.Image, size int) *image.NRGBA {
	canvas := image.NewNRGBA(image.Rect(0, 0, size, size))
	r := float64(size) * 0.24
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			cx, cy := clampf(float64(x), r, float64(size-1)-r), clampf(float64(y), r, float64(size-1)-r)
			if d := math.Hypot(float64(x)-cx, float64(y)-cy); d <= r {
				canvas.SetNRGBA(x, y, color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
			}
		}
	}
	inner := int(float64(size) * 0.84)
	scaled := image.NewNRGBA(image.Rect(0, 0, inner, inner))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), src, src.Bounds(), draw.Over, nil)
	offset := (size - inner) / 2
	draw.Draw(canvas, image.Rect(offset, offset, offset+inner, offset+inner), scaled, image.Point{}, draw.Over)
	return canvas
}

func main() {
	if err := run(); err != nil {
		log.Fatalln(err)
	}
}

func run() error {
	var v [3]uint16
	if _, err := fmt.Sscanf(appVersion, "%d.%d.%d", &v[0], &v[1], &v[2]); err != nil {
		return fmt.Errorf("解析 appVersion %q: %w", appVersion, err)
	}

	appIcon, err := loadAppIcon()
	if err != nil {
		return fmt.Errorf("读取图标源: %w", err)
	}

	rs := winres.ResourceSet{}
	ico, err := winres.NewIconFromResizedImage(appIcon, nil)
	if err != nil {
		return fmt.Errorf("生成 exe 图标: %w", err)
	}
	// 图标组必须嵌为数字 ID 3（wails v3 约定）：窗口创建时经
	// LoadIconWithResourceID(instance, 3) 取图标发 WM_SETICON
	// （application/webview_window_windows.go "App icon ID is 3"，
	// 对话框同源）；此前嵌为名字 "APP" 导致加载不到，标题栏只能
	// 显示窗口类默认的 IDI_APPLICATION 图标（2026-11-09 用户截图实证）。
	if err := rs.SetIcon(winres.ID(3), ico); err != nil {
		return fmt.Errorf("设置图标资源: %w", err)
	}

	// 托盘多尺寸 ICO（BMP/DIB 帧——**不能走 winres.SaveICO**：其帧为
	// PNG 编码，资源管理器认、但托盘的 LoadImage(LR_LOADFROMFILE) 按
	// DIB 误读=花屏（2026-11-04 预览实证），必须手写标准 DIB 容器）。
	traySrc := trayFrame(appIcon, 512)
	if err := writeICO(trayIcoOut, imagesOf(traySrc, traySizes)); err != nil {
		return fmt.Errorf("写托盘 ICO: %w", err)
	}

	// 通知图标 PNG（64px 圆角透明版；beeep 需要文件路径，运行时落盘）。
	notify := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	draw.CatmullRom.Scale(notify, notify.Bounds(), appIcon, appIcon.Bounds(), draw.Over, nil)
	if err := writePNG(notifyPngOut, notify); err != nil {
		return fmt.Errorf("写通知图标: %w", err)
	}

	vi := version.Info{FileVersion: [4]uint16{v[0], v[1], v[2], 0}, ProductVersion: [4]uint16{v[0], v[1], v[2], 0}}
	for _, kv := range [][2]string{
		{version.ProductName, "Yardmaster"},
		{version.FileVersion, fmt.Sprintf("%d.%d.%d.0", v[0], v[1], v[2])},
		{version.ProductVersion, fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2])},
		{version.FileDescription, "Yardmaster——本地 LLM 网关与控制台"},
		{version.LegalCopyright, "MIT License"},
		{version.OriginalFilename, "yardmaster.exe"},
		{version.InternalName, "yardmaster"},
	} {
		if err := vi.Set(0, kv[0], kv[1]); err != nil {
			return fmt.Errorf("设置版本信息 %s: %w", kv[0], err)
		}
	}
	rs.SetVersionInfo(vi)

	// per-monitor-v2 DPI awareness：进程启动即生效（wails 运行时另有
	// 兜底检测，见 application_windows.go——manifest 声明避免先 unaware
	// 后切换造成的缩放跳变）。
	rs.SetManifest(winres.AppManifest{
		ExecutionLevel:      winres.AsInvoker,
		DPIAwareness:        winres.DPIPerMonitorV2,
		UseCommonControlsV6: true,
	})

	for _, t := range outTargets {
		out, err := os.Create(t.rel)
		if err != nil {
			return err
		}
		werr := rs.WriteObject(out, t.arch)
		_ = out.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}

// imagesOf 把 src 逐尺寸缩放为 image.Image 列表（NewIconFromImages 入参）。
func imagesOf(src image.Image, sizes []int) []image.Image {
	out := make([]image.Image, 0, len(sizes))
	for _, s := range sizes {
		dst := image.NewNRGBA(image.Rect(0, 0, s, s))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
		out = append(out, dst)
	}
	return out
}

// writeICO 写标准 ICO 容器（BMP/DIB 帧，LoadImage 兼容——winres 的
// PNG 帧会被托盘 LoadImage 误读）。每帧：BITMAPINFOHEADER + BGRA 像素
// （行倒序）+ 1bpp AND 掩码（alpha 主导时全 0）。
func writeICO(path string, frames []image.Image) error {
	type frameData struct {
		w, h int
		dib  []byte
	}
	dibs := make([]frameData, 0, len(frames))
	for _, f := range frames {
		b := f.Bounds()
		w, h := b.Dx(), b.Dy()
		dib, err := dibFrame(f)
		if err != nil {
			return err
		}
		dibs = append(dibs, frameData{w: w, h: h, dib: dib})
	}

	var out []byte
	le := func(v ...uint16) {
		for _, x := range v {
			out = append(out, byte(x), byte(x>>8))
		}
	}
	le32 := func(v ...uint32) {
		for _, x := range v {
			for i := 0; i < 4; i++ {
				out = append(out, byte(x>>(8*i)))
			}
		}
	}
	// ICONDIR
	le(0, 1, uint16(len(dibs)))
	// ICONDIRENTRY × n + 数据
	offset := uint32(6 + 16*len(dibs))
	for _, d := range dibs {
		wb, hb := byte(d.w), byte(d.h)  // 256 → 0，本帧最大 48
		out = append(out, wb, hb, 0, 0) // w, h, 色数 0（真彩）, 保留
		le(1, 32)                       // planes, bitcount
		le32(uint32(len(d.dib)), offset)
		offset += uint32(len(d.dib))
	}
	for _, d := range dibs {
		out = append(out, d.dib...)
	}
	return os.WriteFile(path, out, 0o644)
}

// dibFrame 把 RGBA 帧编码为 ICO 的 DIB 数据（BITMAPINFOHEADER + XOR
// BGRA 行倒序 + AND 1bpp 掩码）。
func dibFrame(f image.Image) ([]byte, error) {
	b := f.Bounds()
	w, h := b.Dx(), b.Dy()
	andStride := ((w + 31) / 32) * 4 // 1bpp 行按 4 字节对齐
	imgSize := 40 + w*h*4 + andStride*h
	dib := make([]byte, 0, imgSize)
	// BITMAPINFOHEADER：height 双倍（XOR+AND），planes=1，32bpp，无压缩
	put32 := func(v uint32) {
		for i := 0; i < 4; i++ {
			dib = append(dib, byte(v>>(8*i)))
		}
	}
	put32(40)
	put32(uint32(w))
	put32(uint32(h * 2))
	dib = append(dib, 1, 0)
	dib = append(dib, 32, 0)
	put32(0)
	put32(uint32(w*h*4 + andStride*h))
	put32(0)
	put32(0)
	put32(0)
	put32(0)
	// XOR：BGRA，行倒序
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(f.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			dib = append(dib, c.B, c.G, c.R, c.A)
		}
	}
	// AND：1bpp 全 0（32bpp 时 alpha 通道主导透明）
	dib = append(dib, make([]byte, andStride*h)...)
	if len(dib) != imgSize {
		return nil, fmt.Errorf("DIB 尺寸异常 %d != %d", len(dib), imgSize)
	}
	return dib, nil
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	werr := png.Encode(f, img)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}
