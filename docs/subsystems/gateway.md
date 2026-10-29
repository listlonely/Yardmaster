# gateway — 本地网关与路由子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 端点 | `/v1/chat/completions`、`/v1/messages`（推断）、`/v1/models`、`/healthz`、`/api/state`、`/`（快照页） | [`gateway.go`](../../internal/gateway/gateway.go) |
| 候选计划 | `Planner`：`provider/model` 解析；每可用账号一个候选；休眠表（进程内存）与 restLast 排序 | [`plan.go`](../../internal/gateway/plan.go) |
| 故障转移 | `attempt` + `serveWithFailover`：429/5xx/网络错 → 候选休眠并切下一候选；401/403 短休眠；其余 4xx 原样直返（请求问题换账号结果相同）；2xx 开始中继后**不再切换** | [`gateway.go`](../../internal/gateway/gateway.go) |
| 协议翻译 | Chat ↔ Messages：请求、非流响应、双向流式状态机（`chatToMsgsStream`/`msgsToChatStream`）；文本 + 工具（tool_use ↔ tool_calls、tool_result ↔ role:"tool"）；停止原因映射 | [`translate.go`](../../internal/gateway/translate.go) |
| SSE | `scanSSE`：单 Reader 逐事件扫描，注释/心跳跳过，EOF 前最后一条无换行事件可回收 | [`protocol.go`](../../internal/gateway/protocol.go) |
| 上游通道 | `netproxy.Transport`：账号→Provider 代理继承；W2 每次构建，W3 升级为缓存+健康检查 | [`transport.go`](../../internal/netproxy/transport.go) |

## 运行路径

1. `app` 装配：读取 `<数据目录>/providers.json`（损坏按未知处理，空配置启动并保留原文件）→ 构造 `Planner` → `Server.Serve`。
2. 请求进入：解析为对应协议请求 → `ResolveModel`（`provider/model`；group/裸 id 在 W5）→ `Candidates` 展开候选。
3. 逐候选 `attempt`：按上游协议翻译请求体（同协议透传原始字节），补认证头（chat=Bearer；messages=x-api-key+anthropic-version），路径按协议拼接（base_url 为完整前缀）。
4. 失败分类入休眠表（无上游重置时间时默认 5 分钟；429 用 `restQuota`；401/403 短休眠 30s，W3 升级为 bad-credential 标记）。
5. 2xx → 设置 `X-Gw-Provider/X-Gw-Account/X-Gw-Model` → 同协议流式逐字节透传 / 跨协议流式逐事件翻译（每事件 Flush）→ `Clear` 休眠。
6. 全部失败：最后一次的原始错误原样回给 Agent（不伪装 200）。

## 约束与失效行为

- 翻译类型只覆盖 W2 字段集：未覆盖字段（temperature/top_p 之外的采样参数、多模态块、thinking）不透传，多模态/thinking 在 W5 补；thinking 块当前被静默忽略（决策日志记录）。
- 同一物理流使用同一个 `bufio.Reader`（测试曾因每次新建 Reader 丢失缓冲数据而误报）。
- 休眠表键为 `providerID/accountName`，重启即清。
- 客户端断开经 `r.Context()` 传播到上游请求。
- 网关 key 校验（F10 的 gw-<agent> 区分调用方）在 W2 只解析不强制，TODO(P1) 接入 settings。

## 验证

```sh
go test ./internal/gateway
go test -cover ./internal/gateway   # 门槛 ≥75%
```

回放测试：假上游（httptest）+ 真实 Handler 栈经 HTTP 全链路；夹具为手工构造的真实格式 SSE（拿到真实抓包后逐步替换，TODO(W3)）。
