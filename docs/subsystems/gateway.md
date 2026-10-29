# gateway — 本地网关与路由子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 端点 | `/v1/chat/completions`、`/v1/messages`（推断）、`/v1/models`、`/healthz`、`/api/state`、`/api/trace`、`/api/config/*`、`/api/reload`（管理 API，M2-W1）、`/`（控制台）、`/web/*`（静态资源） | [`gateway.go`](../../internal/gateway/gateway.go)、[`api.go`](../../internal/gateway/api.go) |
| 候选计划 | `Planner.Plan`：目标解析（`provider/model`、裸 provider id 自动组、`group/<id>`）→ 跨 provider 候选展开 → 醒/休两段 → 组策略（order/rotate/smart）→ 会话亲和提前；休眠表（进程内存） | [`plan.go`](../../internal/gateway/plan.go) |
| 路由组 | `provider.Group{ID,Strategy,Members}` 存 providers.json；CLI `group add/list/rm` | [`provider.go`](../../internal/provider/provider.go)、[`provider_cmd.go`](../../internal/app/provider_cmd.go) |
| 任务规则 | `RuleEngine`：rules.json 首条命中（intent/regex/max_tokens/caller_key 条件，留空=不限）；mtime 热重载（按 ID 保留 hits）；hits 写穿落盘 | [`rules.go`](../../internal/gateway/rules.go) |
| key 绑定 | `KeyStore`：keys.json 把调用方 key 绑定默认模型；每次读文件（天然热生效） | [`keys.go`](../../internal/gateway/keys.go) |
| 意图判定链 | `IntentChain`：intents.json 自定义（mtime 热生效）→ 内置 L0（[`classify.go`](../../internal/classify/classify.go)）→ L1 小模型（注入回调经网关自身端点调用；缓存 + 连错≥3 冷却 5min） | [`intent.go`](../../internal/gateway/intent.go) |
| 决策追溯 | `TraceLog`：每次推断请求一条 trace（调用方、请求模型、意图/规则命中、候选尝试链含结果/原因/耗时/出口、实际服务方、首字节、token）；异步 JSONL 按天落盘，保留 30 天/200MB；内存环形缓冲供 `/api/trace` | [`trace.go`](../../internal/gateway/trace.go) |
| 当日用量 | `NoteUsage`/`FlushUsage`：从上游响应（流式/非流式）提取 usage，按日累计进账号字段，每分钟+退出前合并落盘 providers.json；smart 策略数据源（W5 字段 W6 起填充） | [`plan.go`](../../internal/gateway/plan.go) |
| 故障转移 | `attempt` + `serveWithFailover`：429/5xx/网络错 → 候选休眠并切下一候选；401/403 短休眠；其余 4xx 原样直返；2xx 开始中继后**不再切换** | [`gateway.go`](../../internal/gateway/gateway.go) |
| 协议翻译 | Chat ↔ Messages：请求、非流响应、双向流式状态机；文本 + 工具；停止原因映射 | [`translate.go`](../../internal/gateway/translate.go) |
| SSE | `scanSSE`：单 Reader 逐事件扫描，注释/心跳跳过，EOF 前最后一条无换行事件可回收 | [`protocol.go`](../../internal/gateway/protocol.go) |
| 上游通道 | `netproxy.SharedTransport`：账号→Provider→全局代理继承；连接池复用 + HTTP/2 健康检查 | [`transport.go`](../../internal/netproxy/transport.go) |

## 路由判定顺序（W5 定稿）

1. 任务规则命中 → 规则 `target` 覆盖（响应带 `X-Gw-Rule`）。
2. 未命中 → 客户端请求的 `model`。
3. `model` 为空或 `"default"` → keys.json 中该调用方 key 的绑定。
4. 仍无目标 → 404。

意图判定：自定义 intents.json → 内置 L0（词表 + 代码围栏/diff/文件路径结构特征）→ L1（settings `l1_classifier` 配置才启用，默认超时 300ms）。内部 L1 调用带 `X-Gw-Internal: l1` 标记头，网关见到该头跳过分类与规则（防递归）。

会话粘性：`X-Gw-Session` 头优先，缺省 `callerKey+"|"+model`；成功后 `NoteAffinity`，下次同会话把亲和候选提前（休眠中不提前）。token 估算 = 字符数 ÷2（最新用户消息供规则/分类）。

## 运行路径

1. `app` 装配：providers.json（含 groups）→ `Planner`；rules.json → `RuleEngine`；keys.json → `KeyStore`；intents.json + settings L1 → `IntentChain`（L1 回调指向自身端点）→ `Server.Serve`。
2. 请求进入：解析协议请求 → 提取 caller key（`x-api-key` 优先，其次 `Authorization: Bearer`）→ 路由判定（上节顺序）→ `Plan(target, stickyKey)`。
3. 逐候选 `attempt`：按候选改写上游模型名（同协议改原始字节、跨协议改结构体——组内各候选模型名可不同），补认证头，路径按协议拼接。
4. 失败分类入休眠表（429 默认 5 分钟；5xx/网络错 5 分钟；401/403 短休眠 30s）。smart 策略读取账号 `used_tokens/quota_tokens_day`（W6 由响应 usage 填充）按剩余比例降序。
5. 2xx → `X-Gw-Provider/X-Gw-Account/X-Gw-Model`（+`X-Gw-Rule` 若规则命中）→ 同协议透传 / 跨协议翻译 → `Clear` 休眠 + 记录亲和。
6. 全部失败：最后一次的原始错误原样回给 Agent（不伪装 200）。

## 决策追溯与用量（W6）

- trace 记录时机：路由判定起（解析失败/全败也留痕，Outcome=fail）；候选链逐次尝试记录结果分类（ok/net/quota/5xx/cred/client）、状态码、原因摘要（错误响应片段，`netproxy.Snippet` 截断）、耗时、实际代理出口（脱敏）。
- 首字节（ttfb_ms）= 请求进入到上游 2xx 响应头到达；total_ms = 到响应写完。token：非流式与流式均尽力从上游响应体提取（usage 正则、取最后一次出现；流式只扫尾部 8KB 窗口，usage 跨块截断时可能漏计——尽力而为语义）。
- 观测通道：`GET /api/trace?limit=&agent=&result=(ok|fail|switch)`；CLI `yardmaster trace list [-agent -result -limit]` / `trace show <id>`（失败链 ✗ 标注原因，成功 ✓）。
- 落盘：`<数据目录>/trace/trace-YYYYMMDD.jsonl`（0600）；启动时清理 + 跨天滚动时清理；只存元数据与失败原因，永不落盘对话内容（§4.4）。
- 用量：`NoteUsage` 累计（内存 + 账号快照字段，跨日按 UsageDay 重置）；`FlushUsage` 重读 providers.json 只更新跟踪账号（不覆盖运行期 CLI 编辑），serve 期间每分钟 + 退出前各一次；`yardmaster provider quota <p> <acc> <tokens|0>` 配置账号当日额度，>0 后 smart 策略按剩余比例排序。

## 管理 API 与控制台（M2-W1）

- 配置读写：`GET/PUT /api/config/{providers|settings|rules|keys|intents}`。PUT 为全文档类型化写回：严格解码（未知字段拒绝）、语义校验（协议/策略枚举、成员引用存在、正则可编译、意图示例 ≤5）、写前备份快照（`<数据目录>/backup/<名>-<纳秒时间戳>.json`，每文件保留 10 份，备份失败即中止写入）、`SaveJSON` 原子写、审计日志。
- 密钥安全：GET providers 输出脱敏密钥（`***+末 4 位`）；PUT 对「空值/脱敏形」保留现存密钥（脱敏往返不破坏密钥，用户值优先）；响应与日志永不回显完整密钥。
- fail-closed：当前文件解析失败时 GET 报 500、PUT/`/api/reload` 报 409 拒绝覆写（读不懂=未知，绝不把损坏文件当空配置覆盖）。
- 热重载：`POST /api/reload` 重读 providers.json → `Planner.Reload` 热替换快照；进程内运行态按「键仍存在才保留」剪枝（休眠表、当日用量、亲和表），策略游标保留。补齐 CLI 编辑 providers「重启 serve 生效」缺口。
- 生效语义：providers PUT 即时生效（写后自动 Reload）；rules/keys/intents 引擎热读取即时生效；settings 装配期读取，响应 `effect:"restart"`。
- 控制台：`/` 出入口 + `/web/*` 静态资源（embed 进二进制）；总览（数据卡/代理继承链/休眠/设置保存）/Provider（账号、脱敏密钥、实时用量进度条、有效代理标注、模型清单、额度设置）/路由（组/规则/意图词表的完整编辑：新建/编辑/删除，成员引用有效性标注）/观测（trace 查询、caller 筛选、行展开候选链详情、自动刷新）四页。
- 控制台编辑安全（W3）：所有写操作经管理 API（严格校验 + 写前备份 + 审计）；提交前 fresh GET 构造全文档（last-writer-wins 窗口最小化）；providers 的密钥以脱敏形往返由服务端保留；删除类操作二次确认；客户端预校验（正则编译/成员引用/示例 ≤5）失败即拦截不发请求，服务端校验失败不落盘，错误内联显示于表单。
- 账号实时状态：`/api/state` 附 `accounts` 数组（key/status/used_tokens/quota_tokens_day/usage_day，Planner 内存快照），控制台 join 该数据展示实时用量——配置文件里的 used_tokens 经 60s 节流落盘，直接读会滞后。

## 约束与失效行为

- 策略说明：order/rotate/smart 在 Planner 内置实现（rotate/smart 需要游标状态）；`internal/routes.Strategy` 接口保留给 P1 无状态策略插件。
- rotate/smart 游标键：组按组 ID；smart 无任何用量数据时回落 rotate（W6 接入用量前的常态）。
- 休眠候选全局排末尾；任何策略/亲和都不会把休眠候选提前。
- rules.json/intents.json 热重载以 mtime+size 判定；解析失败沿用旧配置并告警（保留原文件）。CLI 编辑 rules 建议在网关停止时进行（hits 写穿会互相覆盖）。
- store.LoadJSON 对 UTF-8 BOM 容错（Windows 编辑器常态，剥 BOM 后解析）；CRLF 由 JSON 空白语义天然容忍；解析失败仍按「读不懂=未知」处理。
- 翻译类型只覆盖文本+工具；thinking/多模态块 TODO(P1)（thinking 块当前被静默忽略）。
- 同一物理流使用同一个 `bufio.Reader`（测试曾因每次新建 Reader 丢失缓冲数据而误报）。
- 休眠表/亲和表/游标均为进程内存态，重启即清；亲和表上限 4096 条，超出整体重置。
- 网关 key 校验（F10）仍只解析不强制，TODO(P1) 接入 settings。

## 验证

```sh
go test ./internal/gateway
go test -cover ./internal/gateway   # 门槛 ≥75%（W5 实测 83%）
```

回放测试：假上游（httptest）+ 真实 Handler 栈经 HTTP 全链路；W5 新增策略三态、会话粘性、跨 provider 组 failover、规则条件/热重载、key 默认模型、意图链缓存/冷却/热生效用例；M2-W1 新增配置读写/脱敏往返/备份留底/fail-closed/热重载剪枝用例（`webapi_test.go`、`store/backup_test.go`）。
