// Package credential 管理 OAuth 订阅凭证的生命周期：存储、读取与导入
// 解析（M3-W1，排期见开发文档 §9.4）。
//
// 安全基线（§4.4/F10）：
//   - access/refresh token 永不进入 providers.json 及其 backup/ 快照，
//     只存本文件包管理的独立凭证库；
//   - Windows 下整库经 DPAPI（当前用户上下文）加密为 credentials.bin；
//     其余平台为 0600 明文 JSON credentials.json；
//   - 「读不懂 = 未知」（§4.2）：凭证库损坏/解不开一律返回 ErrUnreadable，
//     调用方必须 fail-closed，绝不允许当作空凭证继续；
//   - 覆写前留底（铁律）：每次 Save 前把当前库文件快照到 backupDir。
//
// 测试不碰真机：Store 路径显式注入（testenv 沙箱），本包永不读取
// 真实 ~/.claude、~/.codex、~/.gemini 等配置。
package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"yardmaster/internal/edit"
	"yardmaster/internal/store"
)

// ErrUnreadable 表示凭证库存在但无法读取/解密（读不懂=未知）。
// 调用方必须 fail-closed；自动刷新（W3）遇到它必须中止而非覆写。
var ErrUnreadable = errors.New("credential store unreadable")

// OAuth 是一个上游订阅账号的 OAuth 凭证本体。字段名以各家 Agent 的
// 真实凭证数据为准在解析器内映射（§4.2 不得凭字段名猜测），此处只存
// 归一化结果；时间统一 Unix 秒。
type OAuth struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	ExpiresAt    int64             `json:"expires_at,omitempty"` // Unix 秒；0=上游未提供
	Scopes       []string          `json:"scopes,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"` // 厂商特有字段（如 account_id）
}

// Validate 拒绝无 access_token 的凭证（空值不可落库）。
func (o OAuth) Validate() error {
	if strings.TrimSpace(o.AccessToken) == "" {
		return fmt.Errorf("oauth 凭证缺少 access_token")
	}
	return nil
}

// Ref 是凭证在库中的键："<provider-id>/<account>"。两段都不允许含
// 分隔符，防止键注入。
type Ref string

// NewRef 构造并校验凭证键。
func NewRef(providerID, account string) (Ref, error) {
	if strings.TrimSpace(providerID) == "" || strings.TrimSpace(account) == "" {
		return "", fmt.Errorf("凭证键需要非空 provider 与账号")
	}
	if strings.ContainsAny(providerID, "/\\") || strings.ContainsAny(account, "/\\") {
		return "", fmt.Errorf("provider 与账号名不允许包含路径分隔符: %q/%q", providerID, account)
	}
	return Ref(providerID + "/" + account), nil
}

// splitRef 校验并拆开已有键（合法键必经 NewRef 产生，此处只防御
// 手工编辑或外部写入的畸形键）。
func splitRef(ref Ref) (providerID, account string, err error) {
	p, a, ok := strings.Cut(string(ref), "/")
	if !ok || p == "" || a == "" ||
		strings.ContainsAny(p, "/\\") || strings.ContainsAny(a, "/\\") {
		return "", "", fmt.Errorf("凭证键须为 <provider>/<account>: %q", ref)
	}
	return p, a, nil
}

// fileV1 是凭证库的文件形态。Windows 下整份 JSON 序列化后再经 DPAPI
// 加密落盘；其余平台直接以 JSON 落盘。
type fileV1 struct {
	Version     int              `json:"version"`
	Credentials map[string]OAuth `json:"credentials"`
}

const fileVersion = 1

// Store 是一个路径绑定的凭证库。
type Store struct {
	Path      string // credentials.bin / credentials.json（由 store.Paths.CredentialsPath 决定）
	BackupDir string // 覆写前留底目录（store.Paths.BackupDir()）
}

// Load 读取 ref 的凭证。文件或键不存在返回 os.ErrNotExist；
// 库损坏/解不开返回 ErrUnreadable（绝不返回部分可用的空凭证）。
func (s Store) Load(ref Ref) (OAuth, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return OAuth{}, err // 含 os.ErrNotExist，由调用方区分
	}
	f, err := decode(raw)
	if err != nil {
		return OAuth{}, fmt.Errorf("%w: %s: %v", ErrUnreadable, filepath.Base(s.Path), err)
	}
	o, ok := f.Credentials[string(ref)]
	if !ok {
		return OAuth{}, fmt.Errorf("%w: 无键 %q", os.ErrNotExist, ref)
	}
	return o, nil
}

// Save 原子写入 ref 的凭证：先对当前库文件留底（铁律「覆写前留底」，
// 首次创建无旧值不算错误），再经平台 seal 后原子落盘。
func (s Store) Save(ref Ref, o OAuth) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if _, _, err := splitRef(ref); err != nil {
		return err
	}
	var f fileV1
	if raw, err := os.ReadFile(s.Path); err == nil {
		cur, derr := decode(raw)
		if derr != nil {
			// 读不懂 = 未知：已有库解不开时拒绝覆写（调用方可显式删除重建），
			// 避免把可能仍可用的凭证库静默替换成只剩一个新键的库。
			return fmt.Errorf("%w: 拒绝覆写: %s: %v", ErrUnreadable, filepath.Base(s.Path), derr)
		}
		f = cur
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.Path, err)
	}
	if err := store.BackupConfig(s.Path, s.BackupDir, store.BaseName(s.Path), 10); err != nil {
		return fmt.Errorf("写前留底失败，已中止: %w", err)
	}
	if f.Credentials == nil {
		f.Credentials = map[string]OAuth{}
	}
	f.Version = fileVersion
	f.Credentials[string(ref)] = o
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	sealed, err := seal(data)
	if err != nil {
		return err
	}
	return edit.WriteAtomic(s.Path, sealed)
}

// Delete 移除 ref。库或键不存在返回 os.ErrNotExist。
func (s Store) Delete(ref Ref) error {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	f, err := decode(raw)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnreadable, filepath.Base(s.Path), err)
	}
	if _, ok := f.Credentials[string(ref)]; !ok {
		return fmt.Errorf("%w: 无键 %q", os.ErrNotExist, ref)
	}
	delete(f.Credentials, string(ref))
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := store.BackupConfig(s.Path, s.BackupDir, store.BaseName(s.Path), 10); err != nil {
		return fmt.Errorf("写前留底失败，已中止: %w", err)
	}
	sealed, err := seal(data)
	if err != nil {
		return err
	}
	return edit.WriteAtomic(s.Path, sealed)
}

// decode 还原库文件：seal 的逆操作 + JSON 解析 + 版本校验。
func decode(raw []byte) (fileV1, error) {
	plain, err := unseal(raw)
	if err != nil {
		return fileV1{}, err
	}
	var f fileV1
	if err := json.Unmarshal(plain, &f); err != nil {
		return fileV1{}, err
	}
	if f.Version != fileVersion {
		return fileV1{}, fmt.Errorf("未知版本 %d（期望 %d）", f.Version, fileVersion)
	}
	return f, nil
}
