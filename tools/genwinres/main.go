// Command genwinres 生成 Windows 资源对象文件（M8-W5）：exe 图标（托盘
// 图标同款矢量按 32 网格放大，256/64/48/32/16 五尺寸）、版本信息、
// per-monitor-v2 DPI manifest。纯 Go（tc-hib/winres，wails 同源依赖），
// 预生成后入库——go build 自动链接 rsrc_windows_amd64.syso，CI/release
// 零改动；GOOS=linux 构建按文件名忽略。重新生成：go generate ./cmd/yardmaster。
// 注意：资源版本（appVersion）随发版更新；运行时版本仍由 release 流程
// -ldflags 注入 internal/app.Version，两者互不依赖。
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"log"
	"os"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

// appVersion 嵌入 exe 属性的资源版本（三段，第四位补 0）。
const appVersion = "0.11.0"

// outPaths 输出对象文件（相对 go generate 的包目录 cmd/yardmaster；带
// GOOS/GOARCH 后缀，仅 windows/amd64 构建链接）。两份：console 入口与
// GUI 入口（Go 只链接 main 包目录下的 syso）。
var outPaths = []string{
	"rsrc_windows_amd64.syso",
	"../yardmaster-app/rsrc_windows_amd64.syso",
}

// drawLogo 画应用图标：调车长意象（深蓝底 + 双轨横线 + 岔轨斜线），
// 与 internal/app 的 trayIcon 同款矢量、同款 32 网格坐标，按目标尺寸
// 缩放填充。
func drawLogo(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 0x1e, G: 0x24, B: 0x30, A: 0xff}
	track := color.RGBA{R: 0xe0, G: 0xe7, B: 0xff, A: 0xff}
	accent := color.RGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	const grid = 32
	cell := func(x, y int, c color.RGBA) {
		s := float64(size) / grid
		x0, y0 := int(float64(x)*s), int(float64(y)*s)
		x1, y1 := int(float64(x+1)*s), int(float64(y+1)*s)
		for yy := y0; yy < y1 && yy < size; yy++ {
			for xx := x0; xx < x1 && xx < size; xx++ {
				img.Set(xx, yy, c)
			}
		}
	}
	for x := 4; x < 28; x++ { // 两条平行横轨（y=20 / y=24）
		cell(x, 20, track)
		cell(x, 24, track)
	}
	for i := 0; i < 12; i++ { // 岔轨斜线（右上引向 x=14 的横轨）
		cell(16+i, 19-i, accent)
		cell(17+i, 19-i, accent)
	}
	return img
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

	rs := winres.ResourceSet{}
	ico, err := winres.NewIconFromResizedImage(drawLogo(256), nil)
	if err != nil {
		return fmt.Errorf("生成图标: %w", err)
	}
	if err := rs.SetIcon(winres.Name("APP"), ico); err != nil {
		return fmt.Errorf("设置图标资源: %w", err)
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

	for _, outPath := range outPaths {
		out, err := os.Create(outPath)
		if err != nil {
			return err
		}
		werr := rs.WriteObject(out, winres.ArchAMD64)
		_ = out.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}
