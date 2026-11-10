# agent — Agent 接线子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 声明式注册表 | 5 个 MVP Agent 的配置位置/格式/字段，全部数据化；引擎不感知具体 Agent | [`registry.go`](../../internal/agent/registry.go) |
| 接线引擎 | `Preview/Wire/Unwire/Drift/StateOf`：预览 diff、备份栈、原子写、审计 | [`wiring.go`](../../internal/agent/wiring.go) |
| TOML 编辑 | 行级拼接：表定位、键替换/插入/删除、行尾注释保留、CRLF 保留 | [`edit/toml.go`](../../internal/edit/toml.go) |
| JSONC 编辑 | 区间拼接 + `Get`（漂移比对） | [`edit/jsonc.go`](../../internal/edit/jsonc.go) |
| CLI | `yardmaster agents list/preview/wire/unwind/drift`；wire 缺省只打印预览（F4 强制预览+确认） | [`app/agents_cmd.go`](../../internal/app/agents_cmd.go) |

## 运行路径 [2026-10-29-声明式接线备份栈]

1. **接入**：`agents wire <id> -model <模型> -yes` → 打印 diff → 备份原文件到
   `stash/<agent>/0`（**已有备份不覆盖**：二次接线时首次原始值优先）→ 按
   声明式 Op 逐键写入 → `applied.json` 记录审计（时间/addr/key/model/文件/键）。
2. **断开**：`agents unwind <id>` → 有 stash 的文件逐字节还原；软件新建的
   文件走逆操作（Unset 名下键 → 级联清理空父对象 → 仅剩空对象则移除文件）。
3. **漂移**：`agents drift <id>` → 对名下键逐个 `Get` 当前值与最后写入值
   比对；任何漂移使状态变为 `drift`（状态机见 PRD 规则页）。
4. **重新应用**：unwind（还原）→ wire（覆盖），覆盖前的值仍在首次备份栈中。

## 约束与失效行为

- **字段形状经真实世界参考实现交叉校准 [2026-11-03-M9-W2]**：codex 与
  gemini-cli 的写入规格按 Magpie agent-wiring（codex.go/gemini.go，
  数千用户桌面应用的真实核实）定稿——codex 走 `experimental_bearer_token`
  + `wire_api="responses"`（`api_key` 非 Codex 字段）；gemini-cli 写
  settings.json `security.auth.selectedType="gemini-api-key"`+`model.name`，
  端点/key 经 CLI 自动加载的 `~/.gemini/.env` 注入。
- **cline 接线落地 [2026-11-09-cline-spec]**：Cline 已从 VS Code
  globalState（state.vscdb）迁移到文件化配置——
  `~/.cline/data/settings/providers.json` 是其跨 IDE/CLI/SDK 的统一
  provider 存储（docs.cline.bot「Config」页；官方测试夹具与
  vercel/vercel 生产 CLI 的 cline 写入器同证）。写入形状：
  `{version:1, lastUsedProvider, providers:{yardmaster:{settings:{
  provider, baseUrl(网关/v1), apiKey, model, protocol:"openai-chat",
  client:"openai-compatible"}, updatedAt, tokenSource:"manual"}}}`——
  schema 逐字段对照 cline/cline 源码 `sdk/packages/core/src/types/
  provider-settings.ts`（StoredProviderSettingsSchema）与
  `services/llms/provider-settings.ts`（ProviderSettingsSchema）：
  settings.provider 必填、baseUrl 须为合法 URL（`z.string().url()`）、
  protocol/client 枚举含 openai-chat / openai-compatible。updatedAt
  为条目必填时间戳（`z.string().datetime()`），随接线时刻变化——
  `Op.Volatile` 标记后漂移比对跳过（引擎新增机制）。CLINE_DIR 环境变量
  可迁移配置根，本接线按缺省根执行。
- **CLI 存在性检测 [2026-11-09-obs1]**：`Def.CLI`（claude/codex/gemini/
  opencode/cline）——配置文件未落盘但 CLI 已在 PATH（npm 全局装 shim，
  LookPath 经 PATHEXT 覆盖 .cmd/.exe）时状态报「未接入」而非「未安装」。
- **规格待核验拒绝**（§4.2）：注册表数据驱动——`Def.SpecNote` 非空即
  拒绝 wire/preview 并说明原因（当前注册表已无 TBD 条目，机制由合成
  用例覆盖）。不写猜出来的键。
- **多文件接线 [M9-W2]**：`Def.Extras` 支持附属文件（gemini 的 .env），
  同一接线记录按 Files 序展开（per-file Format+Ops，旧记录兼容回退）；
  软件创建的附属文件清空后随主文件语义一并移除。
- 只写名下键；断开还原后 `applied.json` 移除记录；stash 保留（供再次接入对照）。
- TOML 已知边界：多行数组/内嵌表不做行内匹配（目标键均为单行标量）；
  同名键多表按表头定位；行内多成员删除可能残留合法 JSONC 尾逗号。
- 逆操作对「软件创建文件」会重排 JSON（紧凑化）——该文件整体为本软件创建，
  格式归本软件管；来自 stash 的还原不受影响。

## 验证

```sh
go test ./internal/agent
go test ./internal/edit -run TOML
```
