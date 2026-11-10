# 管理 API —— 写响应即状态

> M4 立项设计（2026-10-30，参考 Magpie gui-shell 的「写响应即新状态」
> 模式）。实现载体 `internal/gateway/api.go`；本文件是该管理面的接口
> 契约，M4 各周新增写接口一律遵循。

## 数据流（唯一事实来源）

```
读：  GET /api/state ──────────────────────→ 前端 state → 整页重绘
写：  PUT /api/config/{name} 等 ─→ 2xx 响应体含写后 state → 前端 state → 重绘
```

前端**一次交互一个请求**：提交变更 → 响应即（脱敏后的）新状态 → 重绘。
写完不再 GET 回读——消除「写后读回窗口」里另一个写已生效、旧态覆盖
新态的竞态（Magpie `quotaRefresh` 同款问题的根治方式）。

## 接口清单

| 方法与路径 | 用途 | 响应（2xx） |
| :-- | :-- | :-- |
| `GET /api/state` | 全量 live 状态（版本/运行时长/Planner 锁内快照/账号用量/额度） | state 对象 |
| `GET /api/trace` | 调用链路追踪 | trace 列表 |
| `GET /api/cost?days=N` | 成本报告（1~30，默认 7）；[2026-11-批3] 含 `by_account_model` 二级聚合（键 `provider/account/model`，服务商页账号行「明细」数据源） | cost 对象 |
| `GET /api/config/{name}` | 配置脱敏视图（`providers \| settings \| rules \| keys \| intents \| pricing`） | 该配置的脱敏 JSON |
| `PUT /api/config/{name}` | 整文件写（键级编辑器的提交端点） | **写响应包**（见下） |
| `POST /api/reload` | 重读 providers.json 并热替换 Planner | **写响应包** |
| `POST /api/quota/probe` | 手动触发一次余额轮询（M9-W3；与后台 30min ticker 同 `pollQuotas` 路径） | **写响应包** + `probes` 数组 |
| `GET /api/provider-presets` | 内置厂商预设表（M9：前端「添加 Provider」表单预填；与 CLI `provider add -preset` 同一 `provider.Presets` 事实源，只读） | `Preset[]` |
| `POST /api/provider/probe` | 凭证验证 + 按协议拉取上游模型清单（2026-11 批1：添加服务商「检测并获取模型」，复用 CLI 同一 `ProbeAccount`；api_key 仅进内存，探测经全局代理）。**四协议全支持 [2026-11-09-probe-all]**：openai-chat / openai-responses（同源 `/models`+Bearer）、anthropic-messages（`/v1/models`）、gemini（`/v1beta/models`+`x-goog-api-key`，剥 `models/` 前缀并仅保留 generateContent 模型） | `{ok, models: [id]}` |
| `POST /api/tokens` | 生成命名令牌（2026-11 批2 多令牌；`{"name":"归属"}`；plain 仅本次响应返回，落盘只存 sha256 hash；即时热生效） | **写响应包** + `plain` + `token` + `tokens` |
| `POST /api/tokens/{id}/rotate` | 轮换命名令牌（旧 plain 即刻失效，新 plain 仅本次返回） | **写响应包** + `plain` + `token` + `tokens` |
| `DELETE /api/tokens/{id}` | 删除命名令牌（该客户端即刻失去访问；删光回到本机信任态） | **写响应包** + `tokens` |
| `GET /api/plugin-store` | 社区插件商店内置索引（2026-11 批3；前端商店页单一事实源，不再内嵌副本） | `{ok, store: [storeEntry]}` |
| `POST /api/plugin-store/{id}/install` | 一键安装：下载 zip（全局代理 + 32MiB 上限 + SHA256 强校验可选）→ zip slip 防护 + manifest 契约（id 一致 / command 非空 / `plugin.Validate` 全文档）→ 落盘 `<AppDir>/plugins/<id>/` → 注册 plugins.json（备份+审计）→ 热生效；相对 command 物化为插件目录绝对路径；409=已安装，503=安装包未发布 | **写响应包** + `installed` |

管理面非回环监听时全部路由强制 token（M2-W5 三口基线，不变）。

## 统一响应形状

### 写响应包（本设计的核心增量）[2026-10-30-写响应即状态]

所有写操作 2xx 响应为同一形状：

```json
{
  "ok": true,
  "effect": "immediate",
  "config": { "…该配置写后的脱敏视图（仅 PUT config 返回）…": "" },
  "state":  { "…与 GET /api/state 完全同构的写后快照…": "" }
}
```

- `ok`/`effect`：保留现状（`immediate` 立即生效 / `restart` 待重启），
  老调用方不破坏。
- `config`：仅 `PUT /api/config/{name}` 携带；与 `GET /api/config/{name}`
  同构、同脱敏（`api_key` 掩码形），前端可直接回填编辑器基线。
- `state`：写操作落盘/热替换完成**之后**、在 Planner 锁内取的
  `StateSnapshot`——与 `GET /api/state` 复用同一构建函数，保证两端口
  看到同一形状、同一时刻语义。`effect=restart` 的写也返回 state，
  反映「已写入、待重启」的真实中间态。
- `POST /api/reload` 现有的 `providers` 计数字段保留（增量兼容）。
- `POST /api/quota/probe` 附加 `probes`：逐账号探测结果
  `[{provider, account, balance?, error?}]`（失败账号 error 留痕、
  保留上次余额）；仅刷新内存余额，无配置写语义。

### 错误响应（现状不变）

```json
{ "error": "人类可读的中文说明（含修复指引）" }
```

| 状态码 | 语义 | 写路径对应铁律 |
| :-- | :-- | :-- |
| 400 | 请求体解析失败 / 校验失败（未知字段、非法正则、auth 冲突等） | 写边界宁严勿宽（`strictDecode`） |
| 401 | token 缺失或不符 | 三口基线 |
| 404 | 未知配置名 | — |
| 409 | 当前配置文件解析失败，**拒绝覆写** | fail-closed：读不懂=未知 |
| 500 | 写前备份失败（已中止写入）/ 原子写失败 / 读路径解析失败 | 覆写前留底 |

错误时文件**绝不被触碰**（备份失败→中止；解析失败→409 拒绝）。

## 脱敏与密钥往返（用户值优先）[2026-10-30-视图值拷贝脱敏]

写响应包里的 `config.providers` 与 GET 同一脱敏函数：

- `api_key` 返回掩码形；PUT 回传时空值/掩码形 = 保留旧值，其余 = 采纳
  新值（`keepProviderKeys`，现状不变）；
- OAuth 凭证永不出现在任何 API 响应（`internal/credential` 秘密隔离，
  状态页只呈现过期时间/刷新状态元数据）。

## 一致性与防竞态

1. **写响应 state 的取快照时机**：写路径完成 `writeConfig`（备份→原子
   写→审计）与 `Planner.Reload` 之后，同一请求内联取 `StateSnapshot`，
   不经过前端二次往返。
2. **不引入版本号/乐观锁**：管理面单操作者假设下，写响应即状态已消除
   主要竞态；若未来出现多控制台并发编辑，再考虑 `If-Match`（config
   内容哈希），不预先实现。
3. **响应中 `config` 与落盘文件的一致性**：`config` 视图从**刚写入的
   内存对象**序列化（而非写后重读文件），与备份/审计快照同源。

## 兼容策略

- 增量字段：老前端忽略 `config`/`state` 即回到现状行为，无破坏性。
- 新前端（M4 控制台改造）切换为「写响应驱动重绘」后，删除各页写后
  的回读 GET。

## 测试要求（对应工程规范测试规则）

- 写响应包形状断言：`PUT` 五类 config 与 `reload` 各返回 `ok`/`effect`
  /`config`/`state`，`state` 与 `GET /api/state` 字段等价。
- 竞态回归：写入→响应 state 必须包含刚写入的变更（不得是写前快照）。
- 密钥往返：PUT 回传掩码形 → 响应 `config` 中仍为掩码形、落盘文件中
  为原值（防泄漏双断言）。
- 失败路径：备份失败/解析失败时响应为错误形状且 `state` 缺席，文件
  字节不变。
- 全部 httptest，不碰真机、不碰网络。
