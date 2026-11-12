// Agent 目录化安装（2026-11-11）：与插件商店同构的真下载安装——目录
// 条目的 Agent 包（zip：manifest.json + 包装脚本，托管仓库
// examples/agent-packs/ 随代码版本化）下载解压到
// <AppDir>/dispatch-agents/<id>/，manifest 的 spec 经 `./` 前缀物化为
// 包目录绝对路径后注册 dispatch.json（校验+备份+审计+热重载）。
//
// ToS 纪律（派发页同款）：包装脚本调用的是用户自装并登录的本机
// CLI——网关真下载的是包装产物，不代装不代登录 CLI 本体；前置 CLI 以
// LookPath 探测展示（found=false 不拦截安装——可先装预设后补 CLI）。
//
// 包内 manifest 契约：{"id":"<id>","spec":{AgentSpec，command[0] 以
// "./" 前缀指包内相对路径}}。动态校验替代静态 sha256（HTTPS + zip
// 结构 + manifest 一致性白名单；包随代码提交无发布死锁）。
package gateway

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/netproxy"
)

// agentManifest 是 Agent 包内 manifest.json 的契约（Version 与目录条目
// 双写——单一事实源在包内；开发端改包必 bump，驱动用户端更新标记与
// 版本闸，2026-11-12 插件化发布管线）。
type agentManifest struct {
	ID      string             `json:"id"`
	Version string             `json:"version,omitempty"`
	Spec    dispatch.AgentSpec `json:"spec"`
}

// dispatchAgentsInstallDir 返回 Agent 包安装目录：<AppDir>/dispatch-agents/<id>。
func (s *Server) dispatchAgentsInstallDir(id string) string {
	return filepath.Join(s.AppDir, "dispatch-agents", id)
}

// catalogView 是目录条目的展示视图（内嵌条目 + 运行态标注）。
type catalogView struct {
	dispatch.CatalogEntry
	Installed    bool   `json:"installed"`           // id 已在 dispatch.json
	Found        bool   `json:"found"`               // 前置 CLI LookPath 命中
	Path         string `json:"path,omitempty"`
	LocalVersion string `json:"local_version,omitempty"` // 已装包落盘 manifest 的版本
	Updated      bool   `json:"updated"`                 // 包版本高于已装（可更新）
}

// handleDispatchCatalog GET /api/dispatch/catalog：内置目录 + installed/found 标注。
func (s *Server) handleDispatchCatalog(w http.ResponseWriter, _ *http.Request) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	var f dispatch.DispatchFile
	if err := s.loadCurrentForWrite(s.dispatchPath(), &f); err != nil {
		writeAPIErr(w, err)
		return
	}
	installed := map[string]bool{}
	for _, a := range f.Agents {
		installed[a.ID] = true
	}
	cat := dispatch.Catalog()
	views := make([]catalogView, 0, len(cat))
	for _, e := range cat {
		v := catalogView{CatalogEntry: e, Installed: installed[e.Spec.ID]}
		if v.Installed {
			v.LocalVersion = s.installedAgentVersion(e.Spec.ID)
			v.Updated = semverCompare(e.Version, v.LocalVersion) > 0
		}
		if p, err := exec.LookPath(e.Probe); err == nil {
			v.Found = true
			v.Path = p
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"ok": true, "catalog": views})
}

// installedAgentVersion 读已装包落盘 manifest 的版本（缺文件/损坏=空串，
// 视为 0.0.0 参与 semverCompare——旧版安装兼容）。
func (s *Server) installedAgentVersion(id string) string {
	b, err := os.ReadFile(filepath.Join(s.dispatchAgentsInstallDir(id), "manifest.json"))
	if err != nil {
		return ""
	}
	var m agentManifest
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.Version
}

// semverCompare 比较三段数字版本号（缺省段=0；非数字段按 0；前缀 v 容
// 忍；空串=0.0.0）。返回 -1/0/1。
func semverCompare(a, b string) int {
	pa, pb := semverParse(a), semverParse(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func semverParse(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3) {
		if i >= 3 {
			break
		}
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			n = 0
		}
		out[i] = n
	}
	return out
}

// orVersion 空版本显示为 0.0.0（旧版安装兼容的提示形态）。
func orVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "0.0.0"
	}
	return v
}

// handleDispatchCatalogInstall POST /api/dispatch/catalog/{id}/install：
// 下载 Agent 包 → 解压校验 → 注册 dispatch.json（备份审计）→ 热重载。
func (s *Server) handleDispatchCatalogInstall(w http.ResponseWriter, id string) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	cat := dispatch.Catalog()
	var entry *dispatch.CatalogEntry
	for i := range cat {
		if cat[i].Spec.ID == id {
			entry = &cat[i]
			break
		}
	}
	if entry == nil {
		writeAPIErr(w, errNotFound("目录 Agent "+id))
		return
	}
	var f dispatch.DispatchFile
	if err := s.loadCurrentForWrite(s.dispatchPath(), &f); err != nil {
		writeAPIErr(w, err)
		return
	}
	var old *dispatch.AgentSpec
	for i := range f.Agents {
		if f.Agents[i].ID == id {
			old = &f.Agents[i]
			break
		}
	}
	upgrading := old != nil
	if upgrading {
		// 版本闸（2026-11-12 插件化发布管线）：同版=重复安装；旧包=拒绝
		//（防 CDN 缓存回退降级）；新版=覆盖升级（用户基础设置保留）。
		switch c := semverCompare(entry.Version, s.installedAgentVersion(id)); c {
		case 0:
			writeAPIErr(w, &apiError{status: http.StatusConflict,
				Msg: "Agent " + id + " 已安装（重复安装请先在列表删除）"})
			return
		case -1:
			writeAPIErr(w, badRequest(fmt.Errorf(
				"安装包版本 %s 不高于已装 %s——已拒绝（防止 CDN 旧包回退降级；如需重装请先在列表删除）",
				orVersion(entry.Version), orVersion(s.installedAgentVersion(id)))))
			return
		}
	}
	if entry.DownloadURL == "" {
		writeAPIErr(w, &apiError{status: http.StatusServiceUnavailable,
			Msg: "Agent " + id + " 的安装包暂未发布（后续版本提供）"})
		return
	}

	zipBytes, err := s.downloadAgentPack(entry)
	if err != nil {
		slog.Warn("Agent 安装包下载失败", "id", id, "err", err)
		writeAPIErr(w, badRequest(fmt.Errorf("下载安装包失败: %w", err)))
		return
	}

	dir := s.dispatchAgentsInstallDir(id)
	manifest, spec, err := extractAgentArchive(zipBytes, dir)
	if err != nil {
		_ = os.RemoveAll(dir) // 解压失败不留半成品
		writeAPIErr(w, badRequest(fmt.Errorf("安装包校验失败: %w", err)))
		return
	}
	if manifest.ID != id {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(fmt.Errorf("manifest.id=%q 与目录条目 %q 不一致", manifest.ID, id)))
		return
	}
	// 预设侧重继承（2026-11-12）：包装包 manifest 只承载启动定义——
	// 安装后默认侧重以目录条目为单一事实源（manifest 未带 match 时
	// 继承；带 match 的包原样优先）。
	if spec.Match == nil && entry.Spec.Match != nil {
		m := *entry.Spec.Match
		spec.Match = &m
	}
	if upgrading && old != nil {
		// 升级只覆盖开发端拥有的部分（启动定义/预置侧重/包物化文件）；
		// 用户基础设置（超时/并发/env/意图侧重调整）原样保留——边界铁律
		// 「升级不覆盖用户改动」（Agent插件化发布与本地配置边界设计 §3.1）。
		// 意图侧重保留集 = 旧条目的 intents 数组 + 单值 intent（CLI 直配
		// 形态；保留时统一并入 intents，单值字段由 UI 保存语义负责迁空）。
		spec.Timeout, spec.MaxConcurrent, spec.Env = old.Timeout, old.MaxConcurrent, old.Env
		if old.Match != nil {
			userIntents := append([]string(nil), old.Match.Intents...)
			if old.Match.Intent != "" {
				userIntents = append(userIntents, old.Match.Intent)
			}
			if len(userIntents) > 0 {
				if spec.Match == nil {
					m := *old.Match // 旧 match 整体继承（含 CLI 正则契约）
					m.Intent = ""
					m.Intents = userIntents
					spec.Match = &m
				} else {
					spec.Match.Intents = userIntents
				}
			}
		}
	}
	if err := dispatch.ValidateDispatch(dispatch.DispatchFile{
		Agents:    append([]dispatch.AgentSpec(nil), f.Agents...),
		Templates: f.Templates,
	}); err != nil {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(err))
		return
	}
	// 前置 CLI 探测仅目录 GET 展示用（found 标注）；安装不拦截——可先
	// 装预设后补 CLI。

	next := append([]dispatch.AgentSpec(nil), f.Agents...)
	if idx := idIndexOf(next, id); idx >= 0 {
		next[idx] = spec // 升级：替换原条目
	} else {
		next = append(next, spec)
	}
	if err := dispatch.ValidateDispatch(dispatch.DispatchFile{Agents: next, Templates: f.Templates}); err != nil {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, badRequest(err))
		return
	}
	f.Agents = next
	if err := s.writeConfig(s.dispatchPath(), cfgDispatch, f); err != nil {
		_ = os.RemoveAll(dir)
		writeAPIErr(w, err)
		return
	}
	if rerr := s.Dispatch.Reload(f); rerr != nil {
		// 落盘已成功——重载失败仅运行态滞后（重启后生效），降级留痕
		// 不回滚（与 api.go cfgDispatch PUT 的 Reload 语义对齐）。
		slog.Warn("Agent 安装后热重载失败（重启后生效）", "id", id, "err", rerr)
	}
	slog.Info("Agent 已从目录安装", "id", id, "dir", dir, "version", orVersion(manifest.Version), "upgrade", upgrading)
	note := "已写入 dispatch.json（写前备份留底）并热生效；" +
		"请确认本机已安装并登录前置 CLI（" + entry.Probe + "），CLI 本体网关不代装不代登录"
	if upgrading {
		note = "已升级到 " + orVersion(manifest.Version) +
			"（开发端预置部分已覆盖；你的超时/并发/环境变量/意图侧重原样保留）。" + note
	}
	s.writeResultExtra(w, map[string]any{
		"installed": id,
		"note":      note,
	})
}

// idIndexOf 返回 agents 中首个 id 匹配的下标（无则 -1）。
func idIndexOf(agents []dispatch.AgentSpec, id string) int {
	for i := range agents {
		if agents[i].ID == id {
			return i
		}
	}
	return -1
}

// downloadAgentPack 下载 Agent 包（经全局代理；20MB 上限；无静态 sha256
// ——包随代码提交，安装时以 manifest/结构动态校验替代，HTTPS 直链足够）。
func (s *Server) downloadAgentPack(entry *dispatch.CatalogEntry) ([]byte, error) {
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
	return b, nil
}

// extractAgentArchive 解压 Agent 包到 dir（zip slip 防护 + 累计解压上限
// 同插件商店），返回包内 manifest 与物化后的 spec——command[0] 的 `./`
// 前缀替换为包目录绝对路径（子进程按网关 cwd 解析，相对路径不可靠）。
func extractAgentArchive(zipBytes []byte, dir string) (*agentManifest, dispatch.AgentSpec, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, dispatch.AgentSpec{}, fmt.Errorf("zip 解析失败: %w", err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, dispatch.AgentSpec{}, err
	}
	var extracted int64
	var manifest *agentManifest
	for _, zf := range zr.File {
		name := filepath.Clean(zf.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) || strings.HasPrefix(name, `\`) {
			return nil, dispatch.AgentSpec{}, fmt.Errorf("安装包含非法路径 %q", zf.Name)
		}
		target := filepath.Join(absDir, filepath.FromSlash(name))
		if target != absDir && !strings.HasPrefix(target, absDir+string(filepath.Separator)) {
			return nil, dispatch.AgentSpec{}, fmt.Errorf("安装包路径越界 %q", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, dispatch.AgentSpec{}, err
			}
			continue
		}
		if extracted+int64(zf.UncompressedSize64) > storeMaxExtracted {
			return nil, dispatch.AgentSpec{}, errors.New("解压总量超过上限")
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, dispatch.AgentSpec{}, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, dispatch.AgentSpec{}, err
		}
		extracted += int64(len(data))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, dispatch.AgentSpec{}, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return nil, dispatch.AgentSpec{}, err
		}
		if filepath.ToSlash(name) == "manifest.json" {
			var m agentManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, dispatch.AgentSpec{}, fmt.Errorf("manifest.json 解析失败: %w", err)
			}
			manifest = &m
		}
	}
	if manifest == nil {
		return nil, dispatch.AgentSpec{}, errors.New("安装包缺少 manifest.json")
	}
	spec := manifest.Spec
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		return nil, dispatch.AgentSpec{}, errors.New("manifest.spec.command 不能为空")
	}
	argv0 := spec.Command[0]
	if strings.HasPrefix(argv0, "./") { // 包内相对路径 → 绝对路径
		argv0 = filepath.Join(absDir, filepath.FromSlash(strings.TrimPrefix(argv0, "./")))
		if _, err := os.Stat(argv0); err != nil {
			return nil, dispatch.AgentSpec{}, fmt.Errorf("安装包入口不存在: %s", argv0)
		}
		spec.Command[0] = argv0
	}
	return manifest, spec, nil
}
