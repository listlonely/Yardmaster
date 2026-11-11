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
	"strings"

	"yardmaster/internal/dispatch"
	"yardmaster/internal/netproxy"
)

// agentManifest 是 Agent 包内 manifest.json 的契约。
type agentManifest struct {
	ID   string             `json:"id"`
	Spec dispatch.AgentSpec `json:"spec"`
}

// dispatchAgentsInstallDir 返回 Agent 包安装目录：<AppDir>/dispatch-agents/<id>。
func (s *Server) dispatchAgentsInstallDir(id string) string {
	return filepath.Join(s.AppDir, "dispatch-agents", id)
}

// catalogView 是目录条目的展示视图（内嵌条目 + 运行态标注）。
type catalogView struct {
	dispatch.CatalogEntry
	Installed bool   `json:"installed"` // id 已在 dispatch.json
	Found     bool   `json:"found"`     // 前置 CLI LookPath 命中
	Path      string `json:"path,omitempty"`
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
	views := make([]catalogView, 0, len(agentCatalog))
	for _, e := range dispatch.Catalog() {
		v := catalogView{CatalogEntry: e, Installed: installed[e.Spec.ID]}
		if p, err := exec.LookPath(e.Probe); err == nil {
			v.Found = true
			v.Path = p
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"ok": true, "catalog": views})
}

// handleDispatchCatalogInstall POST /api/dispatch/catalog/{id}/install：
// 下载 Agent 包 → 解压校验 → 注册 dispatch.json（备份审计）→ 热重载。
func (s *Server) handleDispatchCatalogInstall(w http.ResponseWriter, id string) {
	if s.Dispatch == nil {
		dispatchDisabled(w)
		return
	}
	var entry *dispatch.CatalogEntry
	for i := range agentCatalog {
		if agentCatalog[i].Spec.ID == id {
			entry = &agentCatalog[i]
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
	for _, a := range f.Agents {
		if a.ID == id {
			writeAPIErr(w, &apiError{status: http.StatusConflict,
				Msg: "Agent " + id + " 已安装（重复安装请先在列表删除）"})
			return
		}
	}
	if entry.DownloadURL == "" {
		writeAPIErr(w, &apiError{status: http.StatusServiceUnavailable,
			Msg: "Agent " + id + " 的安装包暂未发布——可先手动接入 dispatch.json"})
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

	next := append(append([]dispatch.AgentSpec(nil), f.Agents...), spec)
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
	s.Dispatch.Reload(f) // 热生效（运行中会话不受影响）
	slog.Info("Agent 已从目录安装", "id", id, "dir", dir)
	s.writeResultExtra(w, map[string]any{
		"installed": id,
		"note":      "已写入 dispatch.json（写前备份留底）并热生效；" +
			"请确认本机已安装并登录前置 CLI（" + entry.Probe + "），CLI 本体网关不代装不代登录",
	})
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
