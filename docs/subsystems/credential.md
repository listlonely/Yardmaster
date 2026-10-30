# credential 子系统 —— OAuth 订阅凭证

> M3-W1（开发文档 §9.4）。包路径 `internal/credential`；路径常量在
> `store.Paths.CredentialsPath()`；账号元数据模型在 `provider.Account`。

## 职责

| 项 | 说明 |
| :-- | :-- |
| 凭证模型 | `OAuth{AccessToken, RefreshToken, ExpiresAt(Unix 秒), Scopes, Extra}` |
| 凭证库 | 单文件 `~/.yardmaster/credentials.bin`（Windows，DPAPI 密文）/ `credentials.json`（其余，0600 明文 JSON） |
| 键 | `Ref = "<provider-id>/<account>"`，两段禁止路径分隔符（防注入） |
| 导入解析器 | `RegisterParser/Lookup/Names` 注册表；内置：`claude-code`（`~/.claude/.credentials.json`，`claudeAiOauth` 嵌套、毫秒时间戳）、`codex`（`~/.codex/auth.json`，`tokens` 嵌套、API key 登录拒绝导入）、`gemini-cli`（`~/.gemini/oauth_creds.json`，平铺、`expiry_date` 毫秒）。形状样例见 `testdata/credentials/`（README 含 W2 真实凭证复核要求） |
| CLI | `provider add-oauth <id> <account> -from <格式>[:<路径>] [-replace]`（省略路径用 `DefaultPath`）；`provider list` 展示 oauth/过期标记；控制台 Provider 页 OAuth 徽标 + 过期时间 + stale 提示 |
| token 刷新（W3） | `credential.Refresh`（RFC6749 refresh_token grant）+ 每格式内置 `RefreshSpec`（端点/客户端 ID 取自各 Agent 公开配置，`SetRefreshSpec` 可覆盖）；网关 `upstreamToken` 缓存注入 + Claude OAuth 走 `Authorization: Bearer` + `anthropic-beta: oauth-2025-04-20` |
| 刷新触发 | 主动：`StartOAuthRefresher`（serve 起每分钟扫描，提前 `oauthRefreshAhead`=10min）；被动：上游 401/403 → `triggerRefresh`（single-flight）；api_key 账号 401 直接 `MarkBadCredByKey`（规则页：不自动重试） |
| 失败冷却 | 连续 `oauthMaxFails`=3 次刷新失败 → `bad-credential`；元数据 `LastError`（200 字符脱敏摘要，不含 token）；成功即 `ClearByKey` 解除休眠并清零计数 |

## 安全约束（不可协商）

1. **秘密隔离**：access/refresh token 永不进入 `providers.json` 及其
   `backup/` 快照（测试 `TestProviderAddOAuthPipeline` 断言泄漏）。
2. **Windows DPAPI**：`CryptProtectData`（当前用户上下文，
   CRYPTPROTECT_UI_FORBIDDEN），纯 syscall 无 cgo（保持交叉编译铁律）；
   密文跨用户/跨机不可解（设计预期）。
3. **读不懂 = 未知**（§4.2）：库损坏/解不开/版本未知 → `ErrUnreadable`，
   Load 拒绝、Save 拒绝覆写（防止静默替换可能仍可用的库）；解析器
   解析失败必须报错，绝不返回空凭证。
4. **覆写前留底**：Save/Delete 前经 `store.BackupConfig` 快照到
   `backup/credentials-*`（保留 10 份）。
5. **api_key 互斥**：`Account.Validate()` 强制 `auth=oauth` 时
   `APIKey==""`；未知 auth 形态拒绝。管理 API 写路径
   （`validateProviders`）与 CLI 同规。

## 运行路径

```
provider add-oauth -from fmt:path
  → expandHome → Lookup(fmt)（未注册 fail-closed）
  → os.ReadFile（读不到=未知，整体失败）
  → Parser(data)（解析失败=未知，整体失败）
  → oauth.Validate（空 token 拒绝）
  → credential.Store.Save（先凭证库，失败不动配置；写前留底）
  → providers.json 走 BackupConfig + 原子写（Account: Auth=oauth, OAuth 元数据）
  → slog.Info 审计（只记 provider/account/format，不含 token）
```

## 与其它包的边界

- `store`：只提供路径与备份栈原语，不理解凭证内容；
- `provider`：只存非密钥元数据（`OAuthMeta`），状态字典
  `ok|stale|unknown` 在本包体系内定义，禁止他处另立；
- `gateway`（W2/W3 接入）：上游请求需要 token 时经 `Store.Load` 现取；
  401 被动刷新走候选休眠机制（W3）。

## 验证

```sh
go test -race ./internal/credential/ ./internal/provider/ ./internal/app/ ./internal/store/
go test -race -run 'TestProviderAddOAuth|TestCredential|TestAccountValidate' ./...
```

关键用例 ↔ 边界对照：

| 边界 | 用例 |
| :-- | :-- |
| 跨 Save/Load 一致 + 双键共存 | `TestCredentialRoundtrip` |
| Windows 密文无明文 token | `TestCredentialRoundtrip` |
| 空库/缺键 | `TestCredentialLoadMissing` |
| 损坏库读与覆写均 fail-closed | `TestCredentialCorruptIsUnreadable` |
| 覆写前留底 | `TestCredentialSaveBacksUpPrevious`、`TestProviderAddOAuthReplaceConvertsAPIKey` |
| 删除 | `TestCredentialDelete` |
| 空 token 拒绝 | `TestCredentialRejectsEmptyToken` |
| 键注入 | `TestNewRefRejectsInjection` |
| oauth×api_key 互斥/未知形态 | `TestAccountValidateMutualExclusion` |
| 导入全管道 + 泄漏断言 | `TestProviderAddOAuthPipeline` |
| 内置解析器回放（形状样例） | `TestParseClaudeCodeFixture` / `TestParseCodexFixture` / `TestParseGeminiFixture` |
| 解析器 fail-closed（缺字段/空 token/API key 登录） | `TestParsersFailClosed` |
| BOM 容错 | `TestParsersBOMTolerant` |
| 毫秒→秒归一 / 默认路径 | `TestNormExpirySec` / `TestDefaultPath` |
| 省略路径走默认路径（缺失即败） | `TestProviderAddOAuthDefaultPathHint` |
| 未知格式/缺格式/源缺失/形状不对 → 不落盘 | `TestProviderAddOAuthFailsClosed` |
| api_key 账号显式转换清除旧 key | `TestProviderAddOAuthReplaceConvertsAPIKey` |
| 刷新成功（端点表单/合并/落库/元数据/缓存/休眠解除） | `TestRefreshSuccess`、`TestRefreshAccountSuccess` |
| 不轮换保留旧 refresh_token | `TestRefreshKeepsRefreshTokenWhenNotRotated` |
| 刷新失败（拒绝/非 JSON/缺 token）错误脱敏 | `TestRefreshFailures` |
| 连续失败超限 → bad-credential | `TestRefreshAccountFailsThenBadCred` |
| 并发 401 single-flight | `TestTriggerRefreshSingleFlight` |
| 到期前主动扫描 | `TestRefreshDueAccounts` |
| 401 集成：触发刷新且请求照常切换 | `TestOAuthCandidate401TriggersRefresh` |
| 凭证库损坏 → token 注入失败 | `TestUpstreamTokenOAuthMissingIsError` |
