// 网关侧 OAuth 运行时（M3-W3）：access token 缓存、上游请求注入、
// 到期前主动刷新与 401 被动刷新（single-flight），失败冷却复用候选
// 休眠机制。刷新事件全程审计且不含任何 token 值（§4.4）。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"yardmaster/internal/credential"
	"yardmaster/internal/provider"
	"yardmaster/internal/store"
)

// oauthRefreshAhead 是主动刷新提前量：access token 距到期不足此时长
// 即刷新（避免把即将过期的 token 打到上游）。
const oauthRefreshAhead = 10 * time.Minute

// oauthMaxFails 是被动刷新连续失败上限：超过后标记 bad-credential
// （需人工重新登录，规则页「不自动重试」）。
const oauthMaxFails = 3

// cachedToken 是内存中的 access token（凭证库读取的缓存层）。
type cachedToken struct {
	token     string
	expiresAt int64 // Unix 秒；0=未知
}

type refreshFlight struct {
	done chan struct{}
	err  error
}

// oauthRuntime 是 Server 的 OAuth 运行态（懒初始化，零值可用）。
type oauthRuntime struct {
	mu     sync.Mutex
	cache  map[string]cachedToken          // "provider/account" → token
	fails  map[string]int                  // "provider/account" → 连续刷新失败次数
	flight map[string]*refreshFlight       // single-flight
	specs  map[string]credential.RefreshSpec // 刷新规格覆盖（测试注入；缺省用内置）
	httpC  *http.Client                    // 刷新 HTTP 客户端（测试注入）；nil 用默认
}

func (o *oauthRuntime) ensure() {
	if o.cache == nil {
		o.cache = map[string]cachedToken{}
	}
	if o.fails == nil {
		o.fails = map[string]int{}
	}
	if o.flight == nil {
		o.flight = map[string]*refreshFlight{}
	}
}

// SetRefreshSpec 注入/覆盖某导入格式的刷新规格（测试与用户自定义入口；
// 缺省用各 Agent 公开配置的内置规格）。
func (s *Server) SetRefreshSpec(format string, spec credential.RefreshSpec) {
	s.oauth.ensure()
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	if s.oauth.specs == nil {
		s.oauth.specs = map[string]credential.RefreshSpec{}
	}
	s.oauth.specs[format] = spec
}

// refreshSpecFor 解析刷新规格：覆盖优先，回落内置。
func (s *Server) refreshSpecFor(format string) (credential.RefreshSpec, bool) {
	s.oauth.ensure()
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	if s.oauth.specs != nil {
		if sp, ok := s.oauth.specs[format]; ok {
			return sp, true
		}
	}
	return credential.RefreshSpecFor(format)
}

// credStore 返回凭证库（未装配时 false）。
func (s *Server) credStore() (credential.Store, bool) {
	if s.Creds == nil {
		return credential.Store{}, false
	}
	return *s.Creds, true
}

// oauthKey 组装运行态键（Candidate.Key 与刷新路径共用）。
func oauthKey(providerID, account string) string { return providerID + "/" + account }

// upstreamToken 返回候选应使用的上游凭证值：api_key 账号原样返回；
// oauth 账号经缓存/凭证库读取。读不懂=未知：凭证库不可读即报错，
// 绝不返回空 token（上层按失败休眠切换）。
func (s *Server) upstreamToken(cand Candidate) (string, error) {
	if cand.Account.Auth != provider.AuthOAuth {
		return cand.Account.APIKey, nil
	}
	cs, ok := s.credStore()
	if !ok {
		return "", fmt.Errorf("OAuth 账号 %s 未装配凭证库", cand.Key())
	}
	key := cand.Key()
	s.oauth.ensure()
	s.oauth.mu.Lock()
	if ct, ok := s.oauth.cache[key]; ok && (ct.expiresAt == 0 || ct.expiresAt > time.Now().Unix()) {
		s.oauth.mu.Unlock()
		return ct.token, nil
	}
	s.oauth.mu.Unlock()
	ref, err := credential.NewRef(cand.Provider.ID, cand.Account.Name)
	if err != nil {
		return "", err
	}
	o, err := cs.Load(ref)
	if err != nil {
		return "", fmt.Errorf("读取凭证库: %w", err)
	}
	s.oauth.mu.Lock()
	s.oauth.cache[key] = cachedToken{token: o.AccessToken, expiresAt: o.ExpiresAt}
	s.oauth.mu.Unlock()
	return o.AccessToken, nil
}

// triggerRefresh 异步触发被动刷新（401/403 时调用）。已在刷新中的
// 账号共享同一 flight（single-flight，并发 401 只打一次 token 端点）。
func (s *Server) triggerRefresh(cand Candidate) *refreshFlight {
	key := cand.Key()
	s.oauth.ensure()
	s.oauth.mu.Lock()
	if f, ok := s.oauth.flight[key]; ok {
		s.oauth.mu.Unlock()
		return f
	}
	f := &refreshFlight{done: make(chan struct{})}
	s.oauth.flight[key] = f
	s.oauth.mu.Unlock()
	go func() {
		defer close(f.done)
		f.err = s.refreshAccount(cand.Provider.ID, cand.Account.Name)
		s.oauth.mu.Lock()
		delete(s.oauth.flight, key)
		s.oauth.mu.Unlock()
	}()
	return f
}

// refreshAccount 同步刷新一个 OAuth 账号：凭证库读旧值 → token 端点
// 刷新 → 凭证库落新值 → providers.json 元数据更新 → 缓存更新。
// 成功解除该账号休眠（立即回归候选）；失败按次数冷却，连续超限标记
// bad-credential。全程审计不含 token。
func (s *Server) refreshAccount(providerID, accountName string) error {
	cs, ok := s.credStore()
	if !ok {
		return fmt.Errorf("OAuth 账号 %s/%s 未装配凭证库", providerID, accountName)
	}
	ref, err := credential.NewRef(providerID, accountName)
	if err != nil {
		return err
	}
	old, err := cs.Load(ref)
	if err != nil {
		return fmt.Errorf("读取凭证库: %w", err)
	}
	// 刷新规格：按账号元数据记录的导入格式（凭证从哪来就用谁的端点）
	format, _ := s.oauthMeta(providerID, accountName)
	spec, ok := s.refreshSpecFor(format)
	if !ok {
		return fmt.Errorf("格式 %q 无刷新规格", format)
	}
	tr, rerr := credential.Refresh(context.Background(), s.oauthHTTPClient(), spec, old.RefreshToken)
	if rerr != nil {
		s.oauthNoteFailure(providerID, accountName, rerr)
		return fmt.Errorf("刷新 %s/%s: %w", providerID, accountName, rerr)
	}
	next := credential.MergeRefresh(old, tr, time.Now())
	if err := cs.Save(ref, next); err != nil {
		return fmt.Errorf("凭证库写入: %w", err)
	}
	s.oauth.ensure()
	s.oauth.mu.Lock()
	s.oauth.cache[oauthKey(providerID, accountName)] = cachedToken{token: next.AccessToken, expiresAt: next.ExpiresAt}
	s.oauth.fails[oauthKey(providerID, accountName)] = 0
	s.oauth.mu.Unlock()
	s.writeOAuthMeta(providerID, accountName, func(m *provider.OAuthMeta) {
		m.ExpiresAt = next.ExpiresAt
		m.RefreshedAt = time.Now().Unix()
		m.State = provider.OAuthStateFor(next.ExpiresAt)
		m.LastError = ""
	})
	s.Planner.ClearByKey(oauthKey(providerID, accountName))
	slog.Info("OAuth token 已刷新", "provider", providerID, "account", accountName, "expires_at", next.ExpiresAt)
	return nil
}

// oauthHTTPClient 返回刷新用 HTTP 客户端（测试可注入；token 端点为
// 公共互联网地址，走默认出口，15s 超时）。
func (s *Server) oauthHTTPClient() *http.Client {
	if s.oauth.httpC != nil {
		return s.oauth.httpC
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// oauthMeta 读取账号的 OAuth 元数据（providers.json 现值；读不到返回
// 空——刷新规格随后按格式缺失报错）。
func (s *Server) oauthMeta(providerID, accountName string) (string, *provider.OAuthMeta) {
	f, err := store.LoadProvidersFile(s.ProvidersPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("读取 providers.json 失败（刷新规格按格式缺失处理）", "err", err)
		}
		return "", nil
	}
	for _, p := range f.Providers {
		if p.ID != providerID {
			continue
		}
		for _, a := range p.Accounts {
			if a.Name == accountName && a.OAuth != nil {
				return a.OAuth.Format, a.OAuth
			}
		}
	}
	return "", nil
}

// oauthNoteFailure 记录刷新失败：元数据 LastError（脱敏摘要）、连续
// 失败计数、超限标记 bad-credential 并告警。
func (s *Server) oauthNoteFailure(providerID, accountName string, rerr error) {
	msg := truncate(rerr.Error(), 200)
	s.writeOAuthMeta(providerID, accountName, func(m *provider.OAuthMeta) {
		m.LastError = msg
		m.State = provider.OAuthStateStale
	})
	key := oauthKey(providerID, accountName)
	s.oauth.ensure()
	s.oauth.mu.Lock()
	s.oauth.fails[key]++
	fails := s.oauth.fails[key]
	s.oauth.mu.Unlock()
	slog.Warn("OAuth token 刷新失败", "provider", providerID, "account", accountName, "fails", fails, "err", msg)
	if fails >= oauthMaxFails {
		s.Planner.MarkBadCredByKey(key)
		slog.Warn("OAuth 账号连续刷新失败，标记需人工处理（重新登录后用 add-oauth -replace 更新）",
			"provider", providerID, "account", accountName)
	}
}

// refreshDueAccounts 主动扫描：距到期不足 oauthRefreshAhead 且有
// refresh_token 的账号逐个刷新（单个失败不影响其余）。后台任务与
// 测试共用。
func (s *Server) refreshDueAccounts() {
	if s.Creds == nil || s.Planner == nil {
		return
	}
	now := time.Now().Unix()
	for _, p := range s.Planner.Providers() {
		for _, a := range p.Accounts {
			if a.Auth != provider.AuthOAuth || a.OAuth == nil || a.OAuth.ExpiresAt == 0 {
				continue
			}
			if a.OAuth.ExpiresAt > now+int64(oauthRefreshAhead.Seconds()) {
				continue
			}
			_ = s.refreshAccount(p.ID, a.Name) // 失败已在 oauthNoteFailure 记审计与冷却
		}
	}
}

// StartOAuthRefresher 启动到期前主动刷新后台任务（每分钟扫描一次；
// 随 ctx 退出；未装配凭证库时空转）。
func (s *Server) StartOAuthRefresher(ctx context.Context) {
	if s.Creds == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshDueAccounts()
			}
		}
	}()
}

// writeOAuthMeta 更新 providers.json 中账号的 OAuth 元数据（读改写，
// 写前备份；不覆盖其他字段——与 FlushUsage 同规）。失败只记日志，
// 不阻断刷新主流程（内存态已生效，下次刷新重试落盘）。
func (s *Server) writeOAuthMeta(providerID, accountName string, mut func(*provider.OAuthMeta)) {
	if s.ProvidersPath == "" {
		return
	}
	f, err := store.LoadProvidersFile(s.ProvidersPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("OAuth 元数据更新失败：providers.json 读不懂=未知，拒绝覆写", "err", err)
		}
		return
	}
	for i := range f.Providers {
		if f.Providers[i].ID != providerID {
			continue
		}
		for j := range f.Providers[i].Accounts {
			a := &f.Providers[i].Accounts[j]
			if a.Name != accountName {
				continue
			}
			if a.OAuth == nil {
				a.OAuth = &provider.OAuthMeta{}
			}
			mut(a.OAuth)
			if err := store.BackupConfig(s.ProvidersPath, filepath.Join(s.AppDir, "backup"), store.BaseName(s.ProvidersPath), 10); err != nil {
				slog.Warn("OAuth 元数据写前备份失败，已中止", "err", err)
				return
			}
			if err := store.SaveProvidersFile(s.ProvidersPath, f); err != nil {
				slog.Warn("OAuth 元数据落盘失败（下次刷新重试）", "err", err)
			}
			return
		}
	}
}

// truncate 限制错误摘要长度。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
