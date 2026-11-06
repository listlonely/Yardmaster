// 插件商店（2026-11 批3）：内置社区插件索引 + 一键安装。
//
//	GET  /api/plugin-store              索引视图（含安装状态由前端 join /api/plugins）
//	POST /api/plugin-store/{id}/install 下载 zip → 校验 → 落盘 → 注册热生效
//
// 安全边界（插件铁律的落地）：主程序只提供「下载、校验、落盘、注册、
// 可观测」的安装框架，不代登录订阅；zip 强制三重校验——SHA256（索引
// 提供时）、zip slip 路径防护（目标必须落在插件目录内）、manifest 契约
// （id 与索引一致 + command 非空 + plugin.Validate 全文档）。安装写路
// 径走 writeConfig（备份栈+审计），成功后 Registry.Reload 热生效。
// 相对 command 在注册时物化为插件目录内的绝对路径（子进程 exec 按网
// 关进程 cwd 解析，相对路径不可靠）。
package gateway

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yardmaster/internal/netproxy"
	"yardmaster/internal/plugin"
)

// storeEntry 是商店索引的一个条目。
type storeEntry struct {
	ID          string             `json:"id"`
	Ver         string             `json:"ver"`
	Author      string             `json:"author"`
	Cap         string             `json:"cap"` // 主能力（分类徽章）
	Desc        string             `json:"desc"`
	DownloadURL string             `json:"download_url"`        // zip 包地址；空=社区包未发布
	SHA256      string             `json:"sha256,omitempty"`    // 包哈希（提供则强校验）
	Intents     []plugin.IntentRef `json:"intents,omitempty"`   // 安装后声明意图（classify 插件）
}

// PluginStoreIndex 是内置社区插件目录（单一事实源：前端商店页直接拉
// GET /api/plugin-store，不再内嵌副本）。community 包未发布时
// download_url 留空，安装端点如实报错。
var PluginStoreIndex = []storeEntry{
	{ID: "claude-auth", Ver: "0.5.0", Author: "@ym-community", Cap: plugin.CapOAuth,
		Desc: "Claude 订阅登录、续期与多账号适配（社区维护，主程序无厂商代码）"},
	{ID: "codex-auth", Ver: "0.4.1", Author: "@ym-community", Cap: plugin.CapOAuth,
		Desc: "Codex 订阅适配（社区维护）"},
	{ID: "gemini-auth", Ver: "0.4.0", Author: "@ym-community", Cap: plugin.CapOAuth,
		Desc: "Gemini 订阅适配（社区维护）"},
	{ID: "incident-intents", Ver: "0.3.0", Author: "@ym-community", Cap: plugin.CapClassify,
		Desc: "SRE 事故分类词表：oncall / rollback / postmortem",
		Intents: []plugin.IntentRef{
			{ID: "sre/oncall", Label: "值班响应", Desc: "值班期间告警与快速处置"},
			{ID: "sre/postmortem", Label: "复盘分析", Desc: "事故复盘与改进项跟踪"},
		}},
	{ID: "cost-alert", Ver: "0.2.1", Author: "@ym-community", Cap: plugin.CapEvents,
		Desc: "按账号/意图设成本阈值，超额触发系统通知"},
	{ID: "otel-exporter", Ver: "0.1.9", Author: "@ym-community", Cap: plugin.CapEvents,
		Desc: "决策 trace 转发 OTLP 后端（Jaeger / Tempo / Langfuse）"},
	{ID: "qwen-vendor", Ver: "0.4.2", Author: "@ym-community", Cap: plugin.CapVendor,
		Desc: "Qwen 200 伪装拒绝长尾特判词表"},
}

// 安装包硬上限（zip 原始大小 / 累计解压大小）。
const (
	storeMaxArchive   = 32 << 20 // 32 MiB
	storeMaxExtracted = 64 << 20 // 64 MiB
	storeDlTimeout    = 60 * time.Second
)

// handleStoreView GET /api/plugin-store：内置索引（只读）。
func (s *Server) handleStoreView(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "store": PluginStoreIndex})
}

// storeManifest 是插件安装包内的 manifest.json 契约。
type storeManifest struct {
	ID           string   `json:"id"`
	Command      []string `json:"command"`
	Capabilities []string `json:"capabilities"`
	Desc         string   `json:"desc,omitempty"`
	Intents      []plugin.IntentRef `json:"intents,omitempty"`
}

// pluginsInstallDir 返回插件安装目录：<AppDir>/plugins/<id>。
func (s *Server) pluginsInstallDir(id string) string {
	return filepath.Join(s.AppDir, "plugins", id)
}

// handleStoreInstall POST /api/plugin-store/{id}/install。
func (s *Server) handleStoreInstall(w http.ResponseWriter, id string) {
	var entry *storeEntry
	for i := range PluginStoreIndex {
		if PluginStoreIndex[i].ID == id {
			entry = &PluginStoreIndex[i]
			break
		}
	}
	if entry == nil {
		writeAPIErr(w, errNotFound("商店插件 "+id))
		return
	}
	var f plugin.File
	if err := s.loadCurrentForWrite(s.pluginsPath(), &f); err != nil {
		writeAPIErr(w, err)
		return
	}
	for _, p := range f.Plugins {
		if p.ID == id {
			writeAPIErr(w, &apiError{status: http.StatusConflict, Msg: "插件 " + id + " 已安装（重复安装请先卸载）"})
			return
		}
	}
	if entry.DownloadURL == "" {
		writeAPIErr(w, &apiError{status: http.StatusServiceUnavailable,
			Msg: "插件 " + id + " 的安装包暂未发布（社区仓库尚未就绪）——可先手动接入 plugins.json"})
		return
	}

	zipBytes, err := s.downloadStoreArchive(entry)
	if err != nil {
		slog.Warn("插件安装包下载失败", "id", id, "err", err)
		writeAPIErr(w, badRequest(fmt.Errorf("下载安装包失败: %w", err)))
		return
	}

	dir := s.pluginsInstallDir(id)
	manifest, err := extractStoreArchive(zipBytes, dir)
	if err != nil {
		_ = os.RemoveAll(dir) // 解压失败不留半成品
		writeAPIErr(w, badRequest(fmt.Errorf("安装包校验失败: %w", err)))
		return
	}
	if manifest.ID != id {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(fmt.Errorf("manifest.id=%q 与商店条目 %q 不一致", manifest.ID, id)))
		return
	}
	if len(manifest.Command) == 0 || strings.TrimSpace(manifest.Command[0]) == "" {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(errors.New("manifest.command 不能为空")))
		return
	}
	// command[0] 相对路径物化为插件目录内绝对路径（子进程按网关 cwd 解析，
	// 相对路径不可靠）；参数不物化（插件自身语义）。
	argv0 := manifest.Command[0]
	if !filepath.IsAbs(argv0) {
		argv0 = filepath.Join(dir, filepath.FromSlash(argv0))
	}
	if _, err := os.Stat(argv0); err != nil {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(fmt.Errorf("安装包入口不存在: %s", argv0)))
		return
	}
	intents := manifest.Intents
	if intents == nil {
		intents = entry.Intents // manifest 未声明时回落索引条目
	}
	spec := plugin.Spec{
		ID:           id,
		Command:      append([]string{argv0}, manifest.Command[1:]...),
		Enabled:      true,
		Capabilities: manifest.Capabilities,
		Desc:         firstNonEmpty(manifest.Desc, entry.Desc),
		Intents:      intents,
	}
	next := append(append([]plugin.Spec(nil), f.Plugins...), spec)
	if err := plugin.Validate(plugin.File{Plugins: next}); err != nil {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(err))
		return
	}
	f.Plugins = next
	if err := s.writeConfig(s.pluginsPath(), cfgPlugins, f); err != nil {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, err)
		return
	}
	if s.Plugins != nil {
		s.Plugins.Reload(f) // 热生效（新插件 enabled=true 直接拉起）
	}
	slog.Info("插件已从商店安装", "id", id, "ver", entry.Ver, "dir", dir)
	s.writeResultExtra(w, map[string]any{
		"installed": id,
		"note":      "已安装并启用（热生效）；声明意图已同步「路由 › 意图分配」",
	})
}

// downloadStoreArchive 下载安装包（经全局代理；SHA256 提供时强校验）。
func (s *Server) downloadStoreArchive(entry *storeEntry) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeDlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	rt, err := netproxy.SharedTransport(netproxy.Ref(s.GlobalProxy))
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, netproxy.Snippet(body))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, storeMaxArchive+1))
	if err != nil {
		return nil, err
	}
	if len(b) > storeMaxArchive {
		return nil, fmt.Errorf("安装包超过 %d MiB 上限", storeMaxArchive>>20)
	}
	if entry.SHA256 != "" {
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != strings.ToLower(entry.SHA256) {
			return nil, errors.New("SHA256 校验不符（包可能被篡改或损坏）")
		}
	}
	return b, nil
}

// extractStoreArchive 解压安装包到 dir（zip slip 防护 + 累计解压上限），
// 返回包内 manifest。
func extractStoreArchive(zipBytes []byte, dir string) (*storeManifest, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("zip 解析失败: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var extracted int64
	var manifest *storeManifest
	for _, zf := range zr.File {
		name := filepath.Clean(zf.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) || strings.HasPrefix(name, `\`) {
			return nil, fmt.Errorf("安装包含非法路径 %q", zf.Name)
		}
		target := filepath.Join(absDir, filepath.FromSlash(name))
		if target != absDir && !strings.HasPrefix(target, absDir+string(filepath.Separator)) {
			return nil, fmt.Errorf("安装包路径越界 %q", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if extracted+int64(zf.UncompressedSize64) > storeMaxExtracted {
			return nil, errors.New("解压总量超过上限")
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		extracted += int64(len(data))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return nil, err
		}
		if filepath.ToSlash(name) == "manifest.json" {
			var m storeManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("manifest.json 解析失败: %w", err)
			}
			manifest = &m
		}
	}
	if manifest == nil {
		return nil, errors.New("安装包缺少 manifest.json")
	}
	return manifest, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
