# plugins 子系统——插件体系（M7-W2 设计稿）

> [2026-11-02-插件设计定稿] M7-W2 出稿（开发文档 §9.8 主线可行性复核
> 点，M5-W3 同款纪律：首日出稿评审，不可行切备选）。状态：**设计可行，
> 主线采纳——载体选型终局：子进程插件协议（长驻进程），yaegi 降为
> 未来「轻量内联钩子」备选，不作为 W3 载体**。依据见 §二。

> [2026-11-02-插件交付] M7-W3~W6 全部落地（提交 e7a4176 / 76caff3 /
> v0.7.0）。实测与设计偏差备注：
> - 四扩展点全部接线并测试；vendor.verdict 响应契约落地形态为
>   `{action:"error"|"retry","status","base_url"}`（比设计稿的
>   `verdict:"error:<code>"` 字符串编码更直接；语义不变：error 映射
>   伪装拒绝（400~599 校验），retry 端点纠偏落盘+就地重试一次，镜像
>   GLM 1113 内置机制）。
> - 协议实测确认：**信封 payload 为内联 JSON 值**（`"payload":{…}` 非
>   字符串——示例插件首版按字符串 parse 踩坑，冒烟抓出后修正并兼容
>   两种形态）；事件经 stderr 回执在冒烟中端到端验证。
> - 配置闸实测加固：PUT /api/config/plugins 对**损坏现值 409 拒覆写**
> （loadCurrentForWrite 同规——测试抓出初版缺失，fail-closed 补齐）；
> PUT 后注册表热替换（变更实例重建，禁用实例 StatusStopped 不 spawn）。
> - 崩溃隔离真机演练：crasher 插件退避 1s→2s→4s→…→60s 循环重启
> （crash_1h 累计入管理面），邻居插件 running 不受影响；启动失败
> （如路径错误）走同一崩溃路径，stderr（node MODULE_NOT_FOUND）入
> 日志尾可诊断。
> - 冒烟 PowerShell 陷阱备忘：Invoke-RestMethod 发 UTF-8 中文 body 须
> 传**字节数组**（`[Text.Encoding]::UTF8.GetBytes`）+ charset=utf-8，
> 字符串直发按 Latin-1 转码把中文路径变 `??????`（实机抓出）。

## 一、目标与边界

把 M3~M6 积淀的「内置硬编码逻辑」泛化为外部扩展点，让用户不重编译
网关即可接入自定义行为：厂商响应特判、任务意图分类、派发输出解析、
观测事件订阅。

- ToS 延伸：插件是用户**显式安装**的本机程序——网关不代下载、不代装
  （launch 失败报错指引，与派发 Agent 同款兜底）；插件市场/签名/远端
  分发非目标。
- 非目标（可砍清单固化）：插件热重载（改 plugins.json 重启生效）、
  Windows 服务模式、TUI 插件面板、内置逻辑全量迁移（内置实现保留，
  插件钩子并存——见 §四·四）。

## 二、载体选型（终局）

| 方案 | 优劣 | 结论 |
| :-- | :-- | :-- |
| Go plugin 包 | 原生 Go 接口 | **排除**：cgo + 不支持 Windows（交叉编译矩阵铁律） |
| yaegi 等嵌入式解释器 | 单二进制、调用 RTT 低；但解释器开销、panic 隔离不彻底（解释器自身缺陷可穿透 recover）、插件作者被绑死 Go、调试体验差 | 备选：留作未来「轻量内联钩子」（如路由表达式）场景，不承担本设计四类扩展点 |
| **子进程插件协议** | 语言无关（node/python/exe 任一可执行体）、OS 进程边界天然崩溃隔离、父侧 os/exec+bufio 纯 Go 零 cgo（与 dispatch.Run 进程管理经验同构） | **选型**：插件**长驻进程**（spawn 一次摊薄启动成本），NDJSON 管道通信；管道 RTT 亚毫秒，满足全部钩子预算（§七） |

可行性复核结论：四类扩展点中唯一的请求路径内联钩子（vendor.verdict）
预算 800ms，长驻进程管道往返 P99 <10ms，余量充足；事件订阅为
fire-and-forget 不占请求路径。**无不可行项，主线采纳。**

## 三、协议形状（NDJSON，行长上限 1MB）

- **握手**：父 spawn 插件 → 插件 stdout 首行
  `{"plugin":"<id>","proto":1,"capabilities":["events.trace",…]}`（5s
  超时未到=握手失败）；父回应
  `{"ok":true,"granted":[…],"settings":{…}}`——granted = plugins.json
  配置 ∩ 插件声明（用户值优先：以配置为准），越权声明记审计告警不
  中断。
- **信封**：`{"v":1,"type":"<消息类型>","id":"<req-id>","payload":{…}}`
  ——请求/响应以 id 关联；事件类消息单向无 id。
- **心跳**：父每 30s 发 `{"type":"ping"}`；插件回 `pong`；连续 2 个
  周期无响应判死（走崩溃重启路径）。
- **stderr**：插件 stderr 逐行转发网关日志（截尾 2KB/行，前缀插件
  id），用于 W5 管理面「日志尾」展示；插件不得用 stderr 传协议数据。

## 四、扩展点契约（W3 落第 1 个，W4 落后 3 个）

| 扩展点 | 方向 | 触发点 | 预算 | 失败语义 |
| :-- | :-- | :-- | :-- | :-- |
| events.trace | 父→插件单向 | 每条决策 trace 终态 | 入队 <1ms | 队列满 drop-oldest+计数（绝不阻塞推理路径） |
| dispatch.parse | 请求/响应 | 派发输出每完整行 | 1s | 该行 raw 回退（M6-W3 纪律不变） |
| classify.intent | 请求/响应 | L1 分类不可用/冷却时 | 300ms | 回落默认组（连续 3 次失败冷却 5 分钟，L1 同款容错） |
| vendor.verdict | 请求/响应 | 上游响应判定点 | 800ms | 无 verdict/超时=pass（内置词表仍先行） |

### 四·一、events.trace（事件订阅，W3 首个落地）

- 现状约束：TraceLog.OnTrace 为**单观察者槽**且 Record 内联调用
  （OTLP 经异步导出器适配）。W3 在其外套扇出 dispatcher：OTLP 与
  各插件各持一条有界队列（1024 条，满则 drop-oldest 并累计丢弃计数
  ——丢弃计数入 W5 管理面），分发入队 <1ms。
- payload = Trace 结构（元数据：候选链/结局/耗时/用量）——**无对话
  内容**（§4.4 决策记录纪律，插件侧同规）。

### 四·二、dispatch.parse（派发输出解析，W4）

- 请求 `{line:"…"}` → 响应
  `{events:[{kind,text,tool,subtype,final}]}`（kind 枚举与 M6-W3 六类
  一致，外加 `raw`）；一行可出多事件（assistant content 拆分同款）。
- 超时/错误/非法枚举 → 该行按 raw 回退，内容零丢失；plugins.json 中
  agent 条目可指定 `parser:"<插件id>"`（dispatch 配置扩展，同走
  PUT /api/config/dispatch 校验闸）。

### 四·三、classify.intent（意图分类，W4）

- 请求 `{text:"…"}` → 响应 `{intent:"…"}`；intent 必须命中现词表
  （内置+自定义），未知值视同失败回落默认组（不猜）。
- 位置：内置链顺序不变（用户自定义 → 内置 L0 → L1 → 默认组），插件
  分类器在 **L1 不可用/冷却期间**作为兜底（不动热路径主链）。

### 四·四、vendor.verdict（响应特判，W4）

- 请求 `{status:200, body:"…≤8KB 节选"}` → 响应
  `{verdict:"pass"|"error:<状态码>"|"retry:<base_url>", quota_reset_at?}`——
  分别对应内置的三类模式：放行 / 伪装拒绝映射（errorReply 同款语义）/
  端点纠偏就地重试（GLM 1113 同款语义）。
- **内置先行**：内置词表命中即出结果，不问插件；插件只补内置不认识
  的长尾厂商（内置实现保留，迁移是可砍项）。
- **凭证零接触**：请求头/密钥/账号 env 一律不进 payload；响应体节选
  ≤8KB（截断留痕）。

## 五、fail-closed 权限模型

- plugins.json 形态：
  `{"plugins":[{"id","command[]","enabled","capabilities[]","timeout_ms","env[]"}]}`
  ——校验：id 非空唯一、command 非空、capability ∈ 枚举
  （events.trace / dispatch.parse / classify.intent / vendor.verdict）、
  timeout ∈ [100ms, 30s]；**解析失败=全部插件禁用**+报错指引（绝不
  当作空配置静默跑）；写路径走管理 API 同一闸门（校验+备份+审计）。
- 数据暴露最小化（每 capability 一张清单，白名单之外零暴露）：

| capability | 暴露数据 | 明确不暴露 |
| :-- | :-- | :-- |
| events.trace | Trace 元数据（候选链/结局/耗时/用量） | 对话内容、密钥 |
| dispatch.parse | 派发输出的单行文本 | Agent env、任务全文 |
| classify.intent | 任务文本 | 密钥 |
| vendor.verdict | 状态码+响应体节选 ≤8KB | 请求头、密钥、账号 env |

- 插件进程环境：默认**最小 env**（不透传父进程全量环境）；需要变量
  经 plugins.json `env[]` 显式声明——含凭证语义的 env 不进持久化与
  管理面回显（同 dispatch env 纪律）。
- 信任边界直白陈述：插件与网关同用户身份运行，能做的用户手工也能做
  ——插件体系的价值在**可观测、可开关、可隔离崩溃**，不在沙箱逃逸
  防御（明示，不过度承诺）。

## 六、生命周期与隔离

- **装载**：serve/app 启动装配（enabled 逐个 spawn+握手）；握手失败
  =该插件降级禁用+日志，不阻断启动；CLI 侧不装载插件（事件订阅与
  钩子是网关运行期能力）。
- **崩溃/退出**：重启退避 1s→2s→4s…上限 60s；1 小时内 >10 次 → 标记
  failed 直至配置重载；崩溃计数与状态入管理面（W5）。
- **超时**：每请求/响应钩子独立 deadline（按 plugins.json timeout_ms，
  缺省 1s）；到时按各契约失败语义处理，不杀进程（连续超时走冷却/
  判死）。
- **停止**：父优雅关停时杀直接子进程；孙进程残留=已知边界（与派发
  同款，跨平台纯 Go 约束，不引入 cgo）。
- **panic 隔离**：OS 进程边界天然隔离；父侧读循环任何错误不外溢出
  插件注册表。

## 七、性能预算与背压（汇总）

| 路径 | 预算 | 超限语义 |
| :-- | :-- | :-- |
| OnTrace 扇出入队 | <1ms | 有界队列满 drop-oldest+计数 |
| vendor.verdict 往返 | 800ms | pass（内置先行兜底） |
| classify.intent 往返 | 300ms | 回落默认组+冷却 |
| dispatch.parse 往返 | 1s | raw 回退 |
| 握手 | 5s | 插件降级禁用 |
| 内存 | 每插件队列 ≤1MB（1024×1KB 均值） | 计数留痕 |

W3 验收实测口径：长驻进程握手 <200ms、事件转发 P99 <5ms、钩子往返
P99 <10ms（httptest 计时，不碰网络）。

## 八、交付切分

| 周 | 交付 | 状态 |
| :-- | :-- | :-- |
| W2 | 本设计稿（主线可行性复核） | 已交付 |
| W3 | internal/plugin 运行时（注册表/生命周期/NDJSON/重启退避/崩溃隔离）+ plugins.json fail-closed + 扇出 dispatcher + events.trace 落地 + 网关接线（配置闸/状态视图/启停 API） | 已交付（e7a4176） |
| W4 | 三个钩子契约（dispatch.parse / classify.intent / vendor.verdict）+ dispatch 配置 parser 字段 | 已交付（e7a4176） |
| W5 | 管理面（控制台插件页：清单/启停/日志尾/崩溃计数；CLI plugin list/enable/disable）+ examples/ 示例插件 | 已交付（76caff3） |
| W6 | 全链路冒烟（示例插件→装载→事件落盘→崩溃隔离演练→管理面启停，浏览器 4/4 PASS）+ 发布 v0.7.0 | 已交付 |

## 九、边界与铁律对照

- ToS：插件用户显式安装，不代装不代下载；launch 失败报错指引。
- 纯 Go：父侧仅 os/exec+bufio，零 cgo，交叉编译矩阵不变。
- fail-closed：plugins.json 解析失败=全部禁用；握手失败=降级；越权
  声明=裁剪+审计告警；未知 capability/枚举外值=拒绝装载该插件。
- 五铁律：plugins.json 为新配置，写路径走管理 API 同一闸门（校验+
  备份+审计），CLI 同规。
- 凭证安全：四类 payload 白名单外零暴露；插件 env 显式声明且不落盘
  回显（同 dispatch env 纪律）。
