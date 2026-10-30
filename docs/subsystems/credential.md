# credential 子系统 —— OAuth 订阅凭证

> M3-W1（开发文档 §9.4）。包路径 `internal/credential`；路径常量在
> `store.Paths.CredentialsPath()`；账号元数据模型在 `provider.Account`。

## 职责

| 项 | 说明 |
| :-- | :-- |
| 凭证模型 | `OAuth{AccessToken, RefreshToken, ExpiresAt(Unix 秒), Scopes, Extra}` |
| 凭证库 | 单文件 `~/.yardmaster/credentials.bin`（Windows，DPAPI 密文）/ `credentials.json`（其余，0600 明文 JSON） |
| 键 | `Ref = "<provider-id>/<account>"`，两段禁止路径分隔符（防注入） |
| 导入解析器 | `RegisterParser/Lookup/Names` 注册表；内置解析器 M3-W2 落地（claude-code/codex/gemini-cli） |
| CLI | `provider add-oauth <id> <account> -from <格式>:<文件路径> [-replace]`；`provider list` 展示 oauth/过期标记 |

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
| 未知格式/缺格式/源缺失/形状不对 → 不落盘 | `TestProviderAddOAuthFailsClosed` |
| api_key 账号显式转换清除旧 key | `TestProviderAddOAuthReplaceConvertsAPIKey` |
