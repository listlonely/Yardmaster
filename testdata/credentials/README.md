# 凭证样例（M3-W1 收集，仅形状，不含真实值）

三大 Agent OAuth 凭证文件的**字段形状样例**，供 M3-W2 内置解析器的
开发与回放测试使用（开发文档 §9.4 W1/W2；§4.2「引用另一个 Agent 的
配置时，字段类型与取值以该 Agent 自己的代码/数据/文档为准，不得凭
字段名猜测」）。

## 来源与脱敏声明

- 本目录所有 token 值均为构造的 `EXAMPLE` 假值，**不含任何真实凭证**；
- 本机未检出真实凭证文件（2026-10-30 核对），形状依据各 Agent 公开
  文档与其配置写入逻辑整理；
- W2 落地每个解析器前，必须用真实登录产生的凭证文件（人工脱敏后）
  复核字段名/类型/时间单位，与形状样例不一致时**以真实数据为准**
  并回改样例（还原后失败的新测试一并更新）。

## 文件与对应 Agent

| 文件 | Agent | 凭证文件（Linux/Windows 常见路径） | 形状要点 |
| :-- | :-- | :-- | :-- |
| claude-code.json | Claude Code | `~/.claude/.credentials.json` | 顶层键 `claudeAiOauth`；`expiresAt` 为 **Unix 毫秒**；scopes 数组 |
| codex-auth.json | Codex | `~/.codex/auth.json` | 顶层 `OPENAI_API_KEY` 可为 null；token 在 `tokens` 嵌套对象；`last_refresh` 为 RFC3339 字符串 |
| gemini-oauth_creds.json | Gemini CLI | `~/.gemini/oauth_creds.json` | 平铺结构；`expiry_date` 为 **Unix 毫秒** |

> macOS 上 Claude Code 凭证存 Keychain（本项目不支持 macOS，无需处理）。
> W2 解析器需处理：BOM/CRLF、字段缺失、null 值——读不懂 = 未知（§4.2），
> 解析失败必须报错，绝不返回空凭证。
