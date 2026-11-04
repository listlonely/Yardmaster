# protocol 子系统 —— 四协议互转（M4）

> M4-W1 定稿（开发文档 §9.5）。载体 `internal/gateway`（protocol.go /
> responses.go / translate.go）；协议枚举在 `internal/provider`。
> [2026-10-30-事件化中间流] M5-W1 起流式中间表示由 chat chunk 字节流
> 改为内存事件 `gwEvent`（见下文「流式事件层」），非流路径不变。

## 枢纽选型决策（W1 决策点）[2026-10-30-Chat枢纽选型]

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

## 流式事件层（M5-W1）

流式跨协议翻译自 M5-W1 起为「上游 decoder → 逐事件 → 客户端
encoder」（`internal/gateway/event.go`），中间表示为七类内存事件
`gwEvent`（start/text/toolStart/toolArgs/toolDone/stop/usage，参考
Magpie ir.go 的 Event 裁剪）。原「chat chunk 中间流」的
`splitSSEData`（SSE 字节再拆包的 JSON round-trip）已删除。

- 四协议各一对 decoder/encoder：chatDecoder/chatEncoder（event.go）、
  msgsDecoder/msgsEncoder（translate.go）、respDecoder/respEncoder
  （translate_responses.go）、geminiDecoder/geminiEncoder（gemini.go）。
- 事件层词表约定：stop 原因用 chat 词表（协议特有词表由 encoder 就地
  映射）；工具调用用 decoder 归一的顺序号 idx。
- 事件对照表单测 `event_test.go`；既有 4×4 流式矩阵与 M4 流式测试
  **原样通过**作为等价迁移证明（非流路径与同协议透传未动）。

## 协议清单与状态

| 协议 | 入站端点 | 上游路径 | 认证头 | 状态 |
| :-- | :-- | :-- | :-- | :-- |
| openai-chat | `POST /v1/chat/completions` | `/chat/completions` | `Authorization: Bearer` | M1 起全功能 |
| anthropic-messages | `POST /v1/messages` | `/v1/messages` | `x-api-key`（OAuth 走 Bearer+beta） | M1 起全功能 |
| openai-responses | `POST /v1/responses` | `/responses` | `Authorization: Bearer` | W2 交付：双向翻译 + 流式事件 + 工具调用 |
| gemini | `POST /v1beta/models/{model}:generateContent / :streamGenerateContent` | `/v1beta/models/{m}:generateContent(:streamGenerateContent?alt=sse)`（模型名在 URL） | `x-goog-api-key` | W3 交付：入站 + 上游 + 双向翻译 |

> W4 起 translateRequest/translateResponse 已重构为两步式（客户端→
> ChatRequest→上游），4×4 矩阵测试见 `m4w4_test.go`（32 组合全绿）。

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

## sealed reasoning 透传边界（M5-W2 决策）[2026-10-30-sealed-reasoning边界]

**选 (b)：仅标注透传边界，不做翻译透传。**

- Responses 的 reasoning 项（含 `encrypted_content`、`summary`）与
  Anthropic 的 `redacted_thinking` 属未建模形态：双向翻译路径按可砍项
  纪律**忽略**（不猜测字段形状）；同协议透传路径（Responses↔Responses、
  Messages↔Messages）原样到达，不受影响。
- 依据：原计划以 Codex OAuth 真实凭证冒烟核实 reasoning item 形状后
  二选一（M5 排期 §9.6 W2）；本机无已登录 Codex 订阅凭证，「读不懂=
  未知」纪律下不做形状猜测，降级为标注。真实凭证冒烟列入 W6 未实测
  项如实标注。
- 升级路径（(a) 翻译透传 `encrypted_content`）：事件层预留
  evToolDone 同级的扩展位（gwEvent 七类之外新增需过矩阵回归），触发
  条件为真实凭证核实 reasoning item 形状且用户反馈跨轮 reasoning
  上下文丢失造成质量回归。

**终局定稿 [2026-11-03-M9-W2]**：维持 (b) 为**正式终局**（非悬置）——
M9 盘点确认本机无 Codex 订阅账户（用户实证），形状核实条件不具备；
升级路径 (a) 的两项触发条件（真实凭证核实形状 + 用户反馈跨轮质量
回归）留档生效，任一具备时重开。同轮接线校准（agent/registry.go）
经 Magpie codex.go 交叉证实 Codex 走 Responses 原生协议
（`wire_api="responses"`），Responses↔Responses 同协议透传路径即
Codex 直连路径，reasoning 项不受翻译损耗——(b) 的实际影响面进一步
收窄为仅跨协议场景。

## IR v2：thinking/多模态透传（M10-W1 设计定稿）[2026-11-04-IR-v2]

> 兑现枢纽选型锚点（本文件头部：「Chat schema 即 IR v1；thinking/
> 多模态透传立项时再升级正式 IR」）。M10 即该立项（开发文档 §9.12）。
> 排期：W2 请求侧多模态、W3 响应侧 thinking 流出、W4 跨轮保留+C3、
> W5 C2 错误分级、W6 矩阵回归发布 v0.10.0。

### 现状与受损面

- 跨协议路径（两步式枢纽）仅建模文本+工具：图片块与 thinking 块
  **静默丢失**（`contentText`/`toolUseBlocks` 只取所需类型）；
  `translate.go` 头部 TODO(W5) 即此处（M4 范围裁剪的历史债）。
- 同协议透传路径（`rewriteModel` 字节保真）不受影响。
- 主受损面 = messages 客户端 → chat 上游（claude-code → glm-coding，
  用户日常场景）：贴图丢失、GLM 思考不可见、多轮 thinking 历史丢失。

### 真机形状采样（2026-11-04，glm-coding/glm-4.7 经本机网关透传）

| 采样 | 结果 |
| :-- | :-- |
| 流式（默认） | `delta.reasoning_content` 字符串增量，先于 `content` 出现；usage 带 `completion_tokens_details.reasoning_tokens:59` |
| 流式 + `thinking:{"type":"disabled"}` | 参数经透传被上游接受：reasoning_content 消失、reasoning_tokens:0、completion_tokens 89→41 |
| 非流式（默认） | `message.reasoning_content`（318 字符）与 `content` 并列；reasoning_tokens:110 |
| 上游模型名 | chunk `model` = 上游真实名 glm-5.3-flash（服务 glm-4.7 请求） |

结论：GLM 思考流出为 DeepSeek 系形状（`reasoning_content`，vendor
扩展），请求侧 `thinking.type` 可控开关，usage 自带 reasoning_tokens
细分。Magpie v0.1.2890 全仓**零** `reasoning_content`——此为超出
Magpie 参考的自研增量（Magpie 不服务 GLM chat 思考场景）。

### Magpie 交叉参考（ir.go / anthropic.go，v0.1.2890）

- `Part.Kind`: text / image / file / tool_call / tool_result / thinking
  / web_search；Image = MediaType+Data(base64) 或 URL（+Detail 低/高/auto）。
- thinking 三态：Text（明文增量）/ Signature(+SignedBy)（anthropic 签名
  与 gemini thoughtSignature）/ Sealed(+SealedBy)（redacted_thinking 与
  responses reasoning item 原样密封，仅同 API 编码器写回）。
- 事件：KThinkStart / KThink / KSig / KSealed。
- **chat 客户端编码器零 thinking 输出**（chat.go 无 reasoning 字段）——
  思考只输出给有思考形状的协议。
- effortOf/fitEffort：reasoning effort 归一化（ultra→max 等）；
  GLM-5.3 仅接受 low/high/max。

### IR v2 设计（定稿）

1. **内容分块（请求侧）**：枢纽 `ChatMessage.Content` 升级分块感知——
   新增 `ChatContentPart{Type: text|image_url, Text, ImageURL}`；
   解析对齐 Magpie：chat `image_url` 接受字符串或 `{url, detail}` 对象、
   messages `image` 块 `source{media_type, data|url}`、gemini
   `inline_data{mime_type, data}`、responses `input_image{image_url,
   detail}`。中间形态 `IRPart{Kind, Text, MediaType, Data, URL, Detail}`
   （Magpie Part 的裁剪：**不做** file/web_search/tool 级扩展/Loads/
   cache——砍项留档，需要时再立项）。
2. **事件扩展（响应侧）**：`gwEvent` 新增 `evThinking`（文本增量，语义
   同 evText）；GLM `delta.reasoning_content` → evThinking；无 reasoning
   上游零新增事件（可选段，不影响既有七类）。Signature/Sealed 事件
   **不引入**（无跨协议来源，sealed (b) 边界维持）。
3. **用量扩展**：`ChatUsage` 增 `completion_tokens_details.reasoning_tokens`
   （真机证据 59/110）；缺失时零值不虚报。
4. **停止原因**：零变化（thinking 不产生新停止原因，GLM finish_reason
   不变）。

### 决策（W2~W4 执行依据）

- **D1 请求侧 thinking 参数**（anthropic `thinking.budget_tokens` →
  chat）：fail-closed 忽略 + 决策留痕。chat 无标准对应；GLM
  `thinking.type` 属 vendor 私有形态，翻译层不做 vendor 耦合（调用方
  直连 GLM 走透传路径自带该参数，不受影响）。
- **D2 thinking 块跨轮保留**（anthropic assistant 历史 → 非 anthropic
  上游）：丢弃 + 决策留痕（chat/gemini 无需思考历史；带签名的回传仅
  对真实 anthropic 上游有意义，而那走透传路径不经翻译）。
- **D3 messages thinking 块输出**（W3）：仅文本（signature 空串）；
  `redacted_thinking` 维持 sealed (b) 忽略。
- **D4 reasoning_tokens 入账**（W3 顺带）：有则入账，缺失零值。
- **D5 chat 客户端 thinking 输出：不做**（对齐 Magpie 立场——chat 无
  标准思考字段，`reasoning_content` 是 vendor 扩展，hub 不发明协议；
  chat 客户端 + GLM 上游走透传自带）。
- **D6 未知内容块**：fail-closed 忽略 + 决策 trace 留痕（不再静默）。

### 验收矩阵（W2~W3 扩展口径）

4×4 协议矩阵 × 三行内容维度：

| 内容维度 | 覆盖 |
| :-- | :-- |
| 文本+工具 | 现状回归（32 组合不变为基线） |
| 图片（请求侧） | 全组合构造请求（不碰真机）：四种入站形态 × 四种上游形态 |
| thinking（响应侧） | GLM 上游 × {messages, responses, gemini} 客户端（chat 客户端按 D5 不输出）；非 GLM 上游零变化 |

透传路径（同协议）字节保真语义不回归为全程红线。
