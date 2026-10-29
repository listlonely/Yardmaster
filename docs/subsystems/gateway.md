# gateway — 本地网关与路由子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 端点 | `/v1/chat/completions`、`/v1/messages`（推断）、`/v1/models`、`/healthz`、`/api/state`、`/`（快照页） | [`gateway.go`](../../internal/gateway/gateway.go) |
| 候选计划 | `Planner.Plan`：目标解析（`provider/model`、裸 provider id 自动组、`group/<id>`）→ 跨 provider 候选展开 → 醒/休两段 → 组策略（order/rotate/smart）→ 会话亲和提前；休眠表（进程内存） | [`plan.go`](../../internal/gateway/plan.go) |
| 路由组 | `provider.Group{ID,Strategy,Members}` 存 providers.json；CLI `group add/list/rm` | [`provider.go`](../../internal/provider/provider.go)、[`provider_cmd.go`](../../internal/app/provider_cmd.go) |
| 任务规则 | `RuleEngine`：rules.json 首条命中（intent/regex/max_tokens/caller_key 条件，留空=不限）；mtime 热重载（按 ID 保留 hits）；hits 写穿落盘 | [`rules.go`](../../internal/gateway/rules.go) |
| key 绑定 | `KeyStore`：keys.json 把调用方 key 绑定默认模型；每次读文件（天然热生效） | [`keys.go`](../../internal/gateway/keys.go) |
| 意图判定链 | `IntentChain`：intents.json 自定义（mtime 热生效）→ 内置 L0（[`classify.go`](../../internal/classify/classify.go)）→ L1 小模型（注入回调经网关自身端点调用；缓存 + 连错≥3 冷却 5min） | [`intent.go`](../../internal/gateway/intent.go) |
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

## 约束与失效行为

- 策略说明：order/rotate/smart 在 Planner 内置实现（rotate/smart 需要游标状态）；`internal/routes.Strategy` 接口保留给 P1 无状态策略插件。
- rotate/smart 游标键：组按组 ID；smart 无任何用量数据时回落 rotate（W6 接入用量前的常态）。
- 休眠候选全局排末尾；任何策略/亲和都不会把休眠候选提前。
- rules.json/intents.json 热重载以 mtime+size 判定；解析失败沿用旧配置并告警（保留原文件）。CLI 编辑 rules 建议在网关停止时进行（hits 写穿会互相覆盖）。
- 翻译类型只覆盖文本+工具；thinking/多模态块 TODO(P1)（thinking 块当前被静默忽略）。
- 同一物理流使用同一个 `bufio.Reader`（测试曾因每次新建 Reader 丢失缓冲数据而误报）。
- 休眠表/亲和表/游标均为进程内存态，重启即清；亲和表上限 4096 条，超出整体重置。
- 网关 key 校验（F10）仍只解析不强制，TODO(P1) 接入 settings。

## 验证

```sh
go test ./internal/gateway
go test -cover ./internal/gateway   # 门槛 ≥75%（W5 实测 83%）
```

回放测试：假上游（httptest）+ 真实 Handler 栈经 HTTP 全链路；W5 新增策略三态、会话粘性、跨 provider 组 failover、规则条件/热重载、key 默认模型、意图链缓存/冷却/热生效用例。
