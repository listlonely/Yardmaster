// 命名访问令牌管理面（2026-11 批2，多令牌 · 命名归属）：
//
//	POST   /api/tokens            生成（plain 仅本次响应返回，落盘只存 hash）
//	POST   /api/tokens/{id}/rotate 轮换（旧 plain 即刻失效，新 plain 仅本次返回）
//	DELETE /api/tokens/{id}        删除（该客户端即刻失去访问）
//
// 写路径走 writeConfig（备份栈+审计，与 /api/config/settings 同规）；
// 写成后热替换 Server 内 hash 集——令牌变更即时生效，无需重启。
// legacy 单令牌（settings.token）不变，两者叠加生效。
package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"yardmaster/internal/store"
)

// newTokenPlain 生成一枚令牌明文：ym- 前缀 + 128bit 随机 hex（与
// callerKeyOf 三头任一兼容）。
func newTokenPlain() (plain, hash, mask string, err error) {
	var b [16]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	plain = "ym-" + hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(plain))
	hash = hex.EncodeToString(sum[:])
	mask = "***" + plain[len(plain)-4:]
	return plain, hash, mask, nil
}

// newTokenID 生成令牌条目 ID（tk-<4hex>；唯一性由清单查重兜底）。
func newTokenID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return "tk-" + hex.EncodeToString(b[:])
}

// writeTokens 应用命名令牌清单：校验 → 写 settings.json（备份+审计）→
// 热替换 hash 集。返回脱敏视图（hash 剥除）。
func (s *Server) writeTokens(next []store.TokenEntry) ([]store.TokenEntry, error) {
	var cur store.Settings
	if err := s.loadCurrentForWrite(s.settingsPath(), &cur); err != nil {
		return nil, err
	}
	if err := store.ValidateTokenEntries(next); err != nil {
		return nil, badRequest(err)
	}
	cur.Tokens = next
	if err := s.writeConfig(s.settingsPath(), cfgSettings, cur); err != nil {
		return nil, err
	}
	s.SetTokenHashes(TokenHashesOf(next))
	slog.Info("命名令牌清单已更新", "count", len(next))
	return store.StripTokenHashes(next), nil
}

// handleTokenCreate POST /api/tokens：{"name": "Trae · 笔记本"}。
// 响应 extra 携带 plain（仅此一次）与 token 条目（脱敏形）。
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIErr(w, badRequest(errors.New("请求体解析失败")))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeAPIErr(w, badRequest(errors.New("归属名（发给谁）不能为空")))
		return
	}
	var cur store.Settings
	if err := s.loadCurrentForWrite(s.settingsPath(), &cur); err != nil {
		writeAPIErr(w, err)
		return
	}
	if len(cur.Tokens) >= 64 {
		writeAPIErr(w, badRequest(errors.New("命名令牌已达上限（64 枚）——先删除不再使用的条目")))
		return
	}
	plain, hash, mask, err := newTokenPlain()
	if err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	entry := store.TokenEntry{
		ID: newTokenID(), Name: name, Hash: hash, Mask: mask,
		Created: time.Now().Format("2006-01-02"),
	}
	views, err := s.writeTokens(append(append([]store.TokenEntry(nil), cur.Tokens...), entry))
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	s.writeResultExtra(w, map[string]any{
		"plain": plain, // 仅本次响应可见；关闭后只剩掩码
		"token": entryView(entry),
		"tokens": views,
		"note":  "请立即复制保存——落盘只存哈希，关闭后无法再次查看明文",
	})
}

// handleTokenRotate POST /api/tokens/{id}/rotate：旧 plain 即刻失效。
func (s *Server) handleTokenRotate(w http.ResponseWriter, id string) {
	var cur store.Settings
	if err := s.loadCurrentForWrite(s.settingsPath(), &cur); err != nil {
		writeAPIErr(w, err)
		return
	}
	idx := -1
	for i := range cur.Tokens {
		if cur.Tokens[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeAPIErr(w, errNotFound("令牌 "+id))
		return
	}
	plain, hash, mask, err := newTokenPlain()
	if err != nil {
		writeAPIErr(w, badRequest(err))
		return
	}
	entry := cur.Tokens[idx]
	entry.Hash, entry.Mask = hash, mask
	next := append(append([]store.TokenEntry(nil), cur.Tokens[:idx]...), append([]store.TokenEntry{entry}, cur.Tokens[idx+1:]...)...)
	views, err := s.writeTokens(next)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	slog.Info("访问令牌已轮换", "id", id, "name", entry.Name)
	s.writeResultExtra(w, map[string]any{
		"plain": plain, // 仅本次响应可见
		"token": entryView(entry),
		"tokens": views,
		"note":  "旧令牌即刻失效——该客户端需换成新令牌，其他令牌不受影响",
	})
}

// handleTokenDelete DELETE /api/tokens/{id}。
func (s *Server) handleTokenDelete(w http.ResponseWriter, id string) {
	var cur store.Settings
	if err := s.loadCurrentForWrite(s.settingsPath(), &cur); err != nil {
		writeAPIErr(w, err)
		return
	}
	found := false
	next := make([]store.TokenEntry, 0, len(cur.Tokens))
	for _, t := range cur.Tokens {
		if t.ID == id {
			found = true
			continue
		}
		next = append(next, t)
	}
	if !found {
		writeAPIErr(w, errNotFound("令牌 "+id))
		return
	}
	views, err := s.writeTokens(next)
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	s.writeResultExtra(w, map[string]any{"tokens": views})
}

func entryView(t store.TokenEntry) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "mask": t.Mask, "created": t.Created}
}
