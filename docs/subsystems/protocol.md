# protocol 子系统 —— 四协议互转（M4）

> M4-W1 定稿（开发文档 §9.5）。载体 `internal/gateway`（protocol.go /
> responses.go / translate.go）；协议枚举在 `internal/provider`。

## 枢纽选型决策（W1 决策点）

**主案（采纳）**：以 **Chat Completions 为内部枢纽协议**。

- Responses 与 Gemini 各实现两个方向：入站→Chat（请求归一）、Chat→
  出站（响应归一）；现有 **Chat↔Messages 直连翻译保留不动**（零回归
  风险，M1~M3 已在生产路径验证）。
- 手写方向数：直连 2 + Responses 双向 2 + Gemini 双向 2 = **6**，
  而非全对全 12；每个新协议接入成本恒定为 2。
- Chat schema 即 **IR v1**；thinking/多模态透传立项时再升级正式 IR
  （升级路径：Chat 的 `content any` 已兼容块数组，无需破坏性变更）。

**备案（留档，未采纳）**：内部 IR 全收口（四协议入站先归一 IR，出站
从 IR 派生）。优点是语义单点维护；缺点是 Chat↔Messages 既有行为必须
等价迁移（M1 以来的流式状态机重写），回归面大。触发升级条件：矩阵
测试（W4）发现「直连保留」方案出现语义缺口。

## 协议清单与状态

| 协议 | 入站端点 | 上游路径 | 认证头 | 状态 |
| :-- | :-- | :-- | :-- | :-- |
| openai-chat | `POST /v1/chat/completions` | `/chat/completions` | `Authorization: Bearer` | M1 起全功能 |
| anthropic-messages | `POST /v1/messages` | `/v1/messages` | `x-api-key`（OAuth 走 Bearer+beta） | M1 起全功能 |
| openai-responses | `POST /v1/responses` | `/responses` | `Authorization: Bearer` | W1 骨架：同协议透传可跑；跨协议翻译 W2 |
| gemini | —（W3） | `:generateContent`（含模型名，W3） | API key / OAuth（W3 核实） | W1 仅枚举可配置，fail-closed 不参与路由 |

## 四协议语义差异对照（翻译矩阵的差异源）

| 维度 | Chat | Messages | Responses | Gemini |
| :-- | :-- | :-- | :-- | :-- |
| 系统提示 | `messages[0].role=system` | 顶层 `system` | 顶层 `instructions` | 顶层 `systemInstruction` |
| 消息数组 | `messages` | `messages` | `input`（string 或项数组） | `contents` |
| 工具定义 | `tools[].function` | `tools[]{name,input_schema}` | `tools[]{type:function,name,parameters}` | `tools[].functionDeclarations` |
| 工具调用 | `tool_calls[]{id,function}` | 块 `tool_use{id,name,input}` | 项 `function_call{call_id,name,arguments}` | `functionCall{name,args}` |
| 工具结果 | `role:"tool"+tool_call_id` | 块 `tool_result{tool_use_id}` | 项 `function_call_output{call_id,output}` | `functionResponse{name,response}` |
| 停止原因 | `finish_reason`（stop/tool_calls/length） | `stop_reason`（end_turn/tool_use/max_tokens） | `status`+`incomplete_details`（completed/incomplete） | `finishReason`（STOP/MAX_TOKENS/SAFETY/RECITATION） |
| 流式形态 | `chat.completion.chunk`（data 行，`[DONE]` 结束） | 具名事件（message_start/content_block_delta/…） | 具名事件（response.output_text.delta/response.completed） | `alt=sse` data 行（无 [DONE]） |
| 用量字段 | `usage{prompt_tokens,completion_tokens}` | `usage{input_tokens,output_tokens}` | `usage{input_tokens,output_tokens}` | `usageMetadata{promptTokenCount,candidatesTokenCount}` |
| 上限字段 | `max_tokens` | `max_tokens`（必填语义） | `max_output_tokens` | `generationConfig.maxOutputTokens` |

> 差异对照是 W2/W3 翻译的实现依据；Responses/Gemini 列以官方公开文档
> 为初稿，W2/W3 各自「核实真实形状」时逐格复核（读不懂=未知纪律）。

## W1 交付边界（fail-closed）

- `POST /v1/responses` 已注册：非法 JSON 400；同协议（provider 协议为
  openai-responses）透传 + 模型名改写 + Bearer 认证可跑通。
- Responses × Chat/Messages 跨协议：attempt 层拦截，错误消息标注
  「M4-W2 支持」，不触达上游、不产生半翻译请求。
- gemini 协议：CLI/管理 API 可配置（值可落盘），attempt 层拦截标注
  「M4-W3 支持」，不静默走错路径。
- inputStats 支持 Responses（instructions + input 文本计入长度估计，
  最后 user 消息喂意图分类），规则引擎/意图链对新入站协议生效。

## 翻译矩阵目标（W4 验收）

```
入站 \ 上游    Chat      Messages    Responses( W2)   Gemini(W3)
Chat           透传      直连翻译    枢纽翻译         枢纽翻译
Messages       直连翻译  透传        枢纽翻译         枢纽翻译
Responses(W2)  枢纽翻译  枢纽翻译    透传             枢纽翻译
Gemini(W3)     枢纽翻译  枢纽翻译    枢纽翻译         透传
```

- 透传 = 原始字节 + 模型名改写（`rewriteModel`，W3 冒烟教训）。
- 直连翻译 = M1 的 Chat↔Messages 双向（translate.go，保留不动）。
- 枢纽翻译 = 经 Chat schema 中转，W2（Responses）/W3（Gemini）实现。
