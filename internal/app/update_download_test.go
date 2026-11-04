// M11-W2 管线测试：httptest 假 release 服务覆盖 happy 路径与三降级
// （校验失败 / 断流 / 缺资产）+ SHA256SUMS 解析变体 + settings update
// 三态 CLI 接线。zip 布局对齐 package.sh 实证（目录前缀 + 双 exe +
// README 干扰项）。
package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"yardmaster/internal/store"
	"yardmaster/internal/testenv"
)

// fakeUpdateOpts 注入故障场景。
type fakeUpdateOpts struct {
	sumOverride string // 非空=SHA256SUMS 给错误哈希（校验失败）
	truncateZip bool   // zip 下发一半断流（Content-Length 全量）
	dropZip     bool   // 不含平台 zip 资产（缺资产）
}

// buildUpdateTestZip 构造假平台 zip（package.sh 布局：目录前缀+双 exe+
// README 干扰项）。每次调用独立构建（zip 头含时间戳，字节不重复）。
func buildUpdateTestZip(t *testing.T, gatewayContent string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range [][2]string{
		{"yardmaster-windows-amd64/yardmaster.exe", gatewayContent},
		{"yardmaster-windows-amd64/yardmaster-app.exe", "FAKE-APP-EXE"},
		{"yardmaster-windows-amd64/README.md", "readme"},
	} {
		w, err := zw.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newUpdateTestServer 起假 release 服务（/latest 元数据 + /dl/* 资产），
// 返回指向该服务的 releaseInfo（测试直接喂 stageUpdate，不依赖外网）。
func newUpdateTestServer(t *testing.T, opts fakeUpdateOpts) (*httptest.Server, *releaseInfo, int64) {
	t.Helper()
	gateway := fmt.Sprintf("FAKE-GATEWAY-%s-%s", runtime.GOOS, runtime.GOARCH)
	zipBytes := buildUpdateTestZip(t, gateway)
	sum := sha256.Sum256(zipBytes)
	sumHex := hex.EncodeToString(sum[:])
	if opts.sumOverride != "" {
		sumHex = opts.sumOverride
	}
	var sumsBuf bytes.Buffer
	fmt.Fprintf(&sumsBuf, "%s  %s\n", sumHex, platformAssetName())

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &releaseInfo{Tag: "v9.9.9", URL: srv.URL}
	if !opts.dropZip {
		rel.Assets = append(rel.Assets, releaseAsset{
			Name: platformAssetName(), BrowserDownloadURL: srv.URL + "/dl/" + platformAssetName(), Size: int64(len(zipBytes)),
		})
	}
	rel.Assets = append(rel.Assets, releaseAsset{
		Name: "SHA256SUMS", BrowserDownloadURL: srv.URL + "/dl/SHA256SUMS", Size: int64(sumsBuf.Len()),
	})

	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(rel); err != nil {
			t.Errorf("编码假 release: %v", err)
		}
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(sumsBuf.Bytes())
	})
	mux.HandleFunc("/dl/"+platformAssetName(), func(w http.ResponseWriter, _ *http.Request) {
		if opts.truncateZip {
			w.Header().Set("Content-Length", strconv.FormatInt(int64(len(zipBytes)), 10))
			_, _ = w.Write(zipBytes[:len(zipBytes)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush() // 头+半包先落网，Do() 成功后在 body 阶段断
			}
			panic(http.ErrAbortHandler) // 未写满即断流
		}
		_, _ = w.Write(zipBytes)
	})
	return srv, rel, int64(len(zipBytes))
}

// happy 路径：fetchRelease 元数据 → stageUpdate 校验解包 → 双 exe +
// VERSION 落暂存、zip.part 清除、README 不入、进度回调收尾对齐。
func TestStageUpdateHappyPath(t *testing.T) {
	srv, _, zipSize := newUpdateTestServer(t, fakeUpdateOpts{})
	got, err := fetchRelease(context.Background(), srv.Client(), srv.URL+"/latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v9.9.9" || len(got.Assets) != 2 {
		t.Fatalf("fetchRelease: %+v", got)
	}
	stageDir := filepath.Join(t.TempDir(), "update")
	var lastDone, lastTotal int64
	progress := func(done, total int64) { lastDone, lastTotal = done, total }
	st, err := stageUpdate(context.Background(), srv.Client(), got, stageDir, progress)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tag != "v9.9.9" || st.Dir != stageDir || st.Size != zipSize {
		t.Fatalf("staged: %+v（zip %d）", st, zipSize)
	}
	wantGateway := fmt.Sprintf("FAKE-GATEWAY-%s-%s", runtime.GOOS, runtime.GOARCH)
	for exe, want := range map[string]string{st.GatewayExe: wantGateway, st.AppExe: "FAKE-APP-EXE"} {
		b, rerr := os.ReadFile(exe)
		if rerr != nil || string(b) != want {
			t.Fatalf("解包 %s: err=%v content=%q", exe, rerr, b)
		}
	}
	ver, _ := os.ReadFile(filepath.Join(stageDir, "VERSION"))
	if string(ver) != "v9.9.9\n" {
		t.Fatalf("VERSION 标记: %q", ver)
	}
	entries, _ := os.ReadDir(stageDir)
	if len(entries) != 3 { // 双 exe + VERSION（.part 已删、README 未入）
		t.Fatalf("暂存应恰含 3 个文件，实际 %d", len(entries))
	}
	if lastDone == 0 || lastDone != lastTotal || lastTotal != zipSize {
		t.Fatalf("进度回调收尾: done=%d total=%d zip=%d", lastDone, lastTotal, zipSize)
	}
}

// 降级 1：SHA256 不匹配 → 报错含 SHA256 且暂存目录整体清理。
func TestStageUpdateChecksumMismatch(t *testing.T) {
	_, rel, _ := newUpdateTestServer(t, fakeUpdateOpts{sumOverride: strings.Repeat("1", 64)})
	stageDir := filepath.Join(t.TempDir(), "update")
	_, err := stageUpdate(context.Background(), http.DefaultClient, rel, stageDir, nil)
	if err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("应报 SHA256 校验失败，得到 %v", err)
	}
	if _, serr := os.Stat(stageDir); !os.IsNotExist(serr) {
		t.Fatalf("失败后暂存目录应整体清理，stat err=%v", serr)
	}
}

// 降级 2：传输中断（Content-Length 全量、实发一半）→ 报错且清理。
func TestStageUpdateTruncatedStream(t *testing.T) {
	_, rel, _ := newUpdateTestServer(t, fakeUpdateOpts{truncateZip: true})
	stageDir := filepath.Join(t.TempDir(), "update")
	_, err := stageUpdate(context.Background(), http.DefaultClient, rel, stageDir, nil)
	if err == nil || !strings.Contains(err.Error(), "中断") {
		t.Fatalf("断流应报下载中断，得到 %v", err)
	}
	if _, serr := os.Stat(stageDir); !os.IsNotExist(serr) {
		t.Fatalf("断流后暂存应清理，stat err=%v", serr)
	}
}

// 降级 3：release 缺本平台 zip 资产 → 报错（非 zip 平台自然降级路径）。
func TestStageUpdateMissingPlatformAsset(t *testing.T) {
	_, rel, _ := newUpdateTestServer(t, fakeUpdateOpts{dropZip: true})
	_, err := stageUpdate(context.Background(), http.DefaultClient, rel, filepath.Join(t.TempDir(), "update"), nil)
	if err == nil || !strings.Contains(err.Error(), "缺少本平台资产") {
		t.Fatalf("缺资产应报错，得到 %v", err)
	}
}

// SHA256SUMS 解析变体：两空格、* 二进制标记、CRLF、杂行跳过。
func TestParseSHA256SUMSVariants(t *testing.T) {
	in := strings.Repeat("a", 64) + "  yardmaster-linux-amd64.tar.gz\r\n" +
		strings.Repeat("b", 64) + " *yardmaster-windows-amd64.zip\r\n" +
		"junk line\n" +
		strings.Repeat("c", 64) + "  yardmaster-linux-arm64.tar.gz\n"
	m := parseSHA256SUMS([]byte(in))
	if len(m) != 3 {
		t.Fatalf("应解析 3 条，得到 %d: %v", len(m), m)
	}
	if m["yardmaster-windows-amd64.zip"] != strings.Repeat("b", 64) {
		t.Fatalf("* 二进制标记未剥除: %v", m)
	}
}

// settings update 三态 CLI 接线：合法值落盘、非法值拒绝且不改现值。
func TestSettingsSetUpdateThreeState(t *testing.T) {
	testenv.New(t)
	var out, errOut bytes.Buffer
	if err := runSettings([]string{"set", "update", "auto"}, &out, &errOut); err != nil {
		t.Fatalf("set update auto: %v（%s）", err, errOut.String())
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.LoadSettings(paths)
	if err != nil || s.Update != store.UpdateAuto {
		t.Fatalf("落盘: %+v err=%v", s, err)
	}
	if err := runSettings([]string{"set", "update", "daily"}, &out, &errOut); err == nil {
		t.Fatal("非法取值应报错")
	}
	s, _ = store.LoadSettings(paths)
	if s.Update != store.UpdateAuto {
		t.Fatalf("被拒请求不得改值: %+v", s)
	}
}

// 真机守卫测试：YM_REAL_RELEASE=vX.Y.Z 时对 GitHub 真资产跑完整
// stageUpdate 管线（走环境代理；资产 URL 按发布命名规则构造，不经
// API——匿名限额与网络环境解耦）。CI 不设变量恒 Skip。
func TestRealReleaseStageUpdate(t *testing.T) {
	tag := os.Getenv("YM_REAL_RELEASE")
	if tag == "" {
		t.Skip("未设置 YM_REAL_RELEASE（真机核销时提供，如 v0.10.0）")
	}
	base := "https://github.com/listlonely/Yardmaster/releases/download/" + tag
	rel := &releaseInfo{Tag: tag, Assets: []releaseAsset{
		{Name: platformAssetName(), BrowserDownloadURL: base + "/" + platformAssetName()}, // Size 0=跳过字节数比对
		{Name: "SHA256SUMS", BrowserDownloadURL: base + "/SHA256SUMS"},
	}}
	stageDir := filepath.Join(t.TempDir(), "update")
	st, err := stageUpdate(context.Background(), &http.Client{}, rel, stageDir, func(done, total int64) {
		fmt.Printf("\r  下载 %d / %d 字节", done, total)
	})
	fmt.Println()
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"网关": st.GatewayExe, "桌面": st.AppExe} {
		fi, serr := os.Stat(p)
		if serr != nil || fi.Size() < 10<<20 {
			t.Fatalf("%s exe 异常 %s: %v", name, p, serr)
		}
	}
	ver, _ := os.ReadFile(filepath.Join(stageDir, "VERSION"))
	if string(ver) != tag+"\n" {
		t.Fatalf("VERSION: %q", ver)
	}
	t.Logf("真机核销通过：%s（%.1f MB，SHA256 %s…）", st.Tag, float64(st.Size)/(1<<20), st.SHA256[:12])
}
