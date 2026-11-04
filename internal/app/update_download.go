// M11-W2 下载与校验管线（app.md §十一）：release 资产枚举 → 平台
// zip 匹配 → SHA256SUMS 解析 → 流式下载边下边校验 → 解包暂存 update/
// 子目录（双 exe + VERSION 标记）。自替换与接力重启（W3 --update-relay）
// 在本管线产物之上工作。
//
// 安全语义：SHA256 不匹配即弃（删文件报错，绝不留半成品）；任何一步
// 失败整体清理暂存目录；磁盘空间预检（zip×3：本体+解包+替换余量）；
// zip 按文件名白名单提取（yardmaster.exe / yardmaster-app.exe，输出
// 名固定），无 zip-slip 面。
package app

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// releaseAsset 是 GitHub release 资产条目。
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// releaseInfo 是 release 查询结果（CLI check 与下载管线共用）。
type releaseInfo struct {
	Tag    string         `json:"tag_name"`
	URL    string         `json:"html_url"`
	Assets []releaseAsset `json:"assets"`
}

// stagedUpdate 是已校验解包的暂存产物（W3 自替换引擎的输入）。
type stagedUpdate struct {
	Dir        string // 暂存目录（双 exe + VERSION）
	GatewayExe string // update/yardmaster.exe（必有）
	AppExe     string // update/yardmaster-app.exe（桌面入口，可缺）
	Tag        string // 如 v0.11.0（与 VERSION 标记一致）
	SHA256     string // zip 资产十六进制校验和
	Size       int64  // zip 字节数
}

// fetchRelease 查询 release 元数据（含资产清单）。
func fetchRelease(ctx context.Context, client *http.Client, apiURL string) (*releaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询最新版本失败（网络不可达？）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GitHub API 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	return &out, nil
}

// platformAssetName 返回当前平台的 release 资产名（package.sh 实证：
// 仅 Windows 打 zip；其他平台真 release 中查无此资产而自然报错，不
// 做硬编码平台拦截）。
func platformAssetName() string {
	return fmt.Sprintf("yardmaster-%s-%s.zip", runtime.GOOS, runtime.GOARCH)
}

// defaultStageDir 返回默认暂存目录（exe 同级 update/）。
func defaultStageDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "update"), nil
}

// parseSHA256SUMS 解析 sha256sum 输出（`<hash>  <name>` 两空格或 `*`
// 二进制标记；容忍 CRLF 与杂行——字段数不足或哈希非 64 位十六进制的
// 行跳过）。重复名以先出现为准。
func parseSHA256SUMS(data []byte) map[string]string {
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || len(fields[0]) != 64 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if _, dup := sums[name]; !dup {
			sums[name] = strings.ToLower(fields[0])
		}
	}
	return sums
}

// stageUpdate 执行完整管线：平台 zip 匹配 → SHA256SUMS 解析 → 磁盘
// 预检 → 流式下载边下边校验 → 解包暂存。任何一步失败即整体清理暂存
// 目录并返回错误。progress 为下载进度回调（已收/总字节；nil=无回调）
// ——W3 托盘气泡经此节流推送。
func stageUpdate(ctx context.Context, client *http.Client, rel *releaseInfo, stageDir string, progress func(done, total int64)) (*stagedUpdate, error) {
	zipName := platformAssetName()
	var asset, sumsAsset *releaseAsset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case zipName:
			asset = &rel.Assets[i]
		case "SHA256SUMS":
			sumsAsset = &rel.Assets[i]
		}
	}
	if asset == nil {
		return nil, fmt.Errorf("release %s 缺少本平台资产 %s（非 zip 平台请手动更新）", rel.Tag, zipName)
	}
	if sumsAsset == nil {
		return nil, fmt.Errorf("release %s 缺少 SHA256SUMS 资产，拒绝无校验下载", rel.Tag)
	}

	// 暂存目录：全新开始（残留旧暂存一并清除）。
	if err := os.RemoveAll(stageDir); err != nil {
		return nil, fmt.Errorf("清理旧暂存失败: %w", err)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建暂存目录失败: %w", err)
	}
	fail := func(err error) (*stagedUpdate, error) {
		os.RemoveAll(stageDir) // 绝不留半成品
		return nil, err
	}

	// 磁盘空间预检：zip×3（本体+解包产物+替换余量）；探测失败不阻断
	//（ exotic 文件系统交给后续真实写错误兜底）。
	if free, derr := diskFree(stageDir); derr == nil && free < uint64(asset.Size)*3 {
		return fail(fmt.Errorf("磁盘空间不足：可用 %d MB < 需约 %d MB", free>>20, uint64(asset.Size)*3>>20))
	}

	sums, err := fetchAssetBytes(ctx, client, sumsAsset, 64<<10)
	if err != nil {
		return nil, err // 尚未落任何文件，无需清理
	}
	want, ok := parseSHA256SUMS(sums)[zipName]
	if !ok {
		return fail(fmt.Errorf("SHA256SUMS 未收录 %s 的校验和", zipName))
	}

	zipPath := filepath.Join(stageDir, zipName+".part")
	size, err := downloadVerified(ctx, client, asset, want, zipPath, progress)
	if err != nil {
		return fail(err)
	}
	st := &stagedUpdate{Dir: stageDir, Tag: strings.TrimSpace(rel.Tag), SHA256: want, Size: size}
	if st.GatewayExe, st.AppExe, err = unzipExes(zipPath, stageDir); err != nil {
		return fail(err)
	}
	os.Remove(zipPath) // zip 本体解包后即弃（暂存只留可执行产物+标记）
	if err := os.WriteFile(filepath.Join(stageDir, "VERSION"), []byte(st.Tag+"\n"), 0o644); err != nil {
		return fail(err)
	}
	return st, nil
}

// fetchAssetBytes 拉取小体量资产（SHA256SUMS）到内存。
func fetchAssetBytes(ctx context.Context, client *http.Client, a *releaseAsset, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BrowserDownloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 失败: HTTP %d", a.Name, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// downloadVerified 流式下载资产到 dst：边下边算 SHA256（读一次磁盘都
// 不多，Tee 语义），完成后与 want 比对——不匹配或字节数与清单不符即
// 删除文件报错。返回实际字节数。
func downloadVerified(ctx context.Context, client *http.Client, a *releaseAsset, want, dst string, progress func(done, total int64)) (int64, error) {
	abort := func(err error) error {
		os.Remove(dst)
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BrowserDownloadURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("下载 %s 失败: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("下载 %s 失败: HTTP %d", a.Name, resp.StatusCode)
	}
	total := a.Size
	if resp.ContentLength > total {
		total = resp.ContentLength
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("创建暂存文件失败: %w", err)
	}
	h := sha256.New()
	var n int64
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return 0, abort(fmt.Errorf("下载 %s 取消: %w", a.Name, err))
		}
		nr, rerr := resp.Body.Read(buf)
		if nr > 0 {
			if _, werr := f.Write(buf[:nr]); werr != nil {
				f.Close()
				return 0, abort(fmt.Errorf("写暂存文件失败: %w", werr))
			}
			h.Write(buf[:nr])
			n += int64(nr)
			if progress != nil {
				progress(n, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return 0, abort(fmt.Errorf("下载 %s 中断（已收 %d/%d 字节）: %w", a.Name, n, total, rerr))
		}
	}
	if err := f.Close(); err != nil {
		return 0, abort(fmt.Errorf("写暂存文件失败: %w", err))
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return 0, abort(fmt.Errorf("SHA256 校验失败（%s）：期望 %s，实得 %s——已弃置", a.Name, want, got))
	}
	if a.Size > 0 && n != a.Size {
		return 0, abort(fmt.Errorf("%s 字节数与清单不符（清单 %d，实收 %d）", a.Name, a.Size, n))
	}
	return n, nil
}

// unzipExes 从 zip 中按文件名白名单提取双 exe（package.sh 实证布局：
// yardmaster-windows-amd64/{yardmaster.exe, yardmaster-app.exe, README.md}；
// 目录前缀任意，按 basename 匹配以兼容打包调整，README 等其余条目
// 不进暂存）。输出文件名取自固定白名单而非条目路径——zip-slip 无面。
func unzipExes(zipPath, stageDir string) (gatewayExe, appExe string, err error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", "", fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		var dst string
		switch filepath.Base(f.Name) {
		case "yardmaster.exe":
			dst = filepath.Join(stageDir, "yardmaster.exe")
			gatewayExe = dst
		case "yardmaster-app.exe":
			dst = filepath.Join(stageDir, "yardmaster-app.exe")
			appExe = dst
		default:
			continue
		}
		rc, oerr := f.Open()
		if oerr != nil {
			return "", "", fmt.Errorf("读取 zip 条目 %s 失败: %w", f.Name, oerr)
		}
		oerr = writeExeFile(rc, dst)
		rc.Close()
		if oerr != nil {
			return "", "", oerr
		}
	}
	if gatewayExe == "" {
		return "", "", fmt.Errorf("zip 内未找到 yardmaster.exe")
	}
	return gatewayExe, appExe, nil
}

// writeExeFile 把 r 完整写入 dst（可执行权限位）。
func writeExeFile(r io.Reader, dst string) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("写 %s 失败: %w", dst, err)
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("解包 %s 失败: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("写 %s 失败: %w", dst, err)
	}
	return nil
}

// updateDownloadCmd 实现 `yardmaster update download`：拉取最新
// release，校验并解包到暂存目录（缺省 exe 同级 update/；-dir 覆盖）。
// 只交付已校验的暂存产物——安装与接力重启是 W3（update install）。
func updateDownloadCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("update download", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "暂存目录（缺省 exe 同级 update/）")
	force := fs.Bool("force", false, "已是最新版仍重新下载")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := &http.Client{} // 无总超时（大文件慢链路），ctx 管总时限
	rel, err := fetchRelease(ctx, client, repoAPI)
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if Version == "dev" {
		fmt.Fprintf(stdout, "最新版本 %s（当前为开发态，仍继续下载）\n", latest)
	} else {
		fmt.Fprintf(stdout, "最新版本 %s · 当前版本 %s\n", latest, Version)
		if compareVersions(Version, latest) >= 0 && !*force {
			fmt.Fprintln(stdout, "已是最新版本（-force 可重新下载）")
			return nil
		}
	}
	staged, err := stageUpdate(ctx, client, rel, stageDir, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已下载并校验通过：%s（%.1f MB，SHA256 %s…）\n  暂存 %s\n  网关 %s\n",
		staged.Tag, float64(staged.Size)/(1<<20), staged.SHA256[:12], staged.Dir, staged.GatewayExe)
	if staged.AppExe != "" {
		fmt.Fprintf(stdout, "  桌面 %s\n", staged.AppExe)
	}
	fmt.Fprintln(stdout, "安装与接力重启：yardmaster update install（M11-W3）")
	return nil
}
