# Yardmaster

**Yardmaster**（调车长）是一个运行在本机的 AI 网关：把多个 AI Agent（Claude Code、Codex、OpenCode 等）的请求，按任务类型自动路由到合适的模型与账号——Agent 只管发车，调车长负责把每趟请求拨上正确的股道。

```text
Agent ──▶ 127.0.0.1:8317 ──▶ Yardmaster ──▶ 按任务/额度/策略 ──▶ 多家上游模型
```

## 核心能力

- **四协议网关（v0.4.0）**：OpenAI Chat / OpenAI Responses / Anthropic Messages / Google Gemini 四协议任一入站可路由至任意协议上游（4×4 互转矩阵，文本 + 工具调用，流式/非流式）；流式经内存事件层（七类 gwEvent，四协议各一对 decoder/encoder，v0.5.0）逐事件翻译，无中间 JSON round-trip
- **任务→Agent 派发（v0.5.0）**：把任务文本交给本机已安装的 Agent（Claude Code / Codex / Gemini CLI 的 headless 模式）执行——意图→Agent 匹配（dispatch.json，fail-closed 校验）、headless 会话管理（stdin/stdout 协议、超时/取消、每 Agent 并发上限）、stdout 实时回传、终态记入决策 trace；控制台「派发」页提交/实时输出/取消，CLI `dispatch agents|init|run`；ToS 边界：仅启动已安装的 Agent，不代装不代登录
- **成本缓存扣除（v0.5.0）**：usage 捕获缓存命中细分（openai `cached_tokens` / gemini `cachedContentTokenCount` / anthropic `cache_read_input_tokens` 折叠统一语义），命中部分按 `cached_input_per_m` 计价（缺省 0=扣除），报告桶单列缓存命中数
- **Agent 接线**：声明式改写 Agent 配置指向网关（预览 diff、备份栈、一键还原、漂移检测）
- **订阅账号（OAuth）**：一键导入 Claude Code / Codex / Gemini CLI 的已登录会话凭证（加密存储：Windows DPAPI / 其余 0600），到期自动续期（主动扫描 + 401 被动刷新），.providers.json 永不落 token
- **厂商预设**：`provider add -preset glm-coding` 等 13 个预设开箱即用；GLM 1113 自动纠偏到 Coding Plan 端点；Zhipu 200 伪装拒绝识别；认证头（x-api-key/Bearer）自动学习
- **路由引擎**：`provider/model`、裸 provider 自动组、`group/<id>` 跨上游路由组；order / rotate / smart / usage / weight（平滑加权）五策略
- **额度感知与 SwitchWhenSpent**：账号当日额度耗尽自动休眠至次日重置；订阅窗口（响应头学习）与余额轮询（QuotaProbe）喂路由；按 key 并发上限
- **成本核算**：模型单价表（pricing.json）× 决策 trace 聚合，按账号/模型出成本报告（零新增持久化）
- **故障转移**：429/5xx/网络错自动切换候选并休眠，额度断流无感接续；会话粘性（`X-Gw-Session`）
- **任务规则 + 意图分类**：rules.json 首条命中改道；自定义意图词表热生效；内置 L0 启发式 + 可选 L1 小模型分类（缓存/冷却/回落）
- **Web 控制台**：总览/Provider/路由/观测/派发五页；配置读写全走管理 API（严格校验 + 写前备份栈 + 审计日志），不手改配置文件即可全量管理
- **决策追溯**：每次路由决策记录完整候选尝试链（谁调用、命中什么、试过谁、为何切换），落盘 30 天/200MB，`/api/trace`、控制台与 CLI 可查
- **代理挂载**：全局 → Provider → 账号三级继承（http/https/socks5/socks5h/direct），HTTP/2 半死连接健康检查
- **局域网共享**：`listen` + 强制令牌（token）；控制台/API/推断端点统一过闸，默认仅本机
- **桌面托盘**：`serve -tray` 系统托盘常驻（打开控制台/退出，纯 Go 无 cgo）；启动自动打开控制台（`-no-open` 可关）
- **OTLP traces 导出（v0.4.0，默认关）**：settings.json `otel{enabled,endpoint,headers}` 开启后，决策 trace 映射 OTel span（gen_ai.* 语义约定）推送 OTLP-HTTP 兼容后端（Jaeger/Tempo/Langfuse），手写 JSON 零 SDK 依赖，失败只降级不阻断推理

## 安装

从 [Releases](../../releases) 下载，或用安装脚本（自动识别平台，加入 PATH）：

```sh
# Linux
curl -fsSL https://github.com/listlonely/Yardmaster/releases/latest/download/install.sh | bash -s -- --user

# Windows (PowerShell)
irm https://github.com/listlonely/Yardmaster/releases/latest/download/install.ps1 -OutFile install.ps1; .\install.ps1
```

## 快速开始

```sh
# 1. 添加上游（厂商预设开箱即用，或手填 base-url；预检验证凭证并拉取模型清单）
yardmaster provider presets                        # 查看全部厂商预设
yardmaster provider add -preset deepseek -key sk-xxx
yardmaster provider probe deepseek

# 2. 导入订阅账号（读 Agent 已登录会话，自动续期）
yardmaster provider add-oauth anthropic -from claude-code   # 或 codex / gemini-cli

# 3. 把 Agent 接到网关（预览确认后改写其配置）
yardmaster agents preview claude-code -model yardmaster-main claude-code
yardmaster agents wire claude-code -model yardmaster-main

# 4. 启动网关（默认仅监听 127.0.0.1:8317；自动打开控制台；-tray 启用托盘常驻）
yardmaster serve

# 5. 观测与经营
yardmaster trace list                 # 路由决策追溯
yardmaster cost --days 7              # 成本报告（先 price set 配单价）
yardmaster tui                        # 终端面板：状态 / 追溯 / 绑定切换
yardmaster update check               # 升级提示

# 6. 任务→Agent 派发（v0.5.0，可选）
yardmaster dispatch init              # 预览三家建议配置（形状未实测，按本机安装修改 command）
yardmaster dispatch agents            # 查看已配置的派发 Agent
yardmaster dispatch run "把 TODO 整理成清单"   # 派发（或在控制台「派发」页提交）
echo "任务文本" | yardmaster run -- claude -p  # headless 原语直通（stdin→stdout 透传）
```

> **派发未实测项（v0.5.0 如实标注）**：三家 Agent 的 headless 输出形状（`claude -p` / `codex exec --json` / `gemini -p`）按官方文档建模，本机未安装未做真实冒烟；`dispatch init` 的建议 command 请按本机实际安装确认。OTLP 真实后端联调与真实上游的缓存命中对账仍为未实测项（沿用 v0.4.0 标注）。

数据目录：`~/.yardmaster`（providers/keys/rules/intents/pricing/settings 均为可 diff 的 JSON，写前自动备份到 `backup/`，原子写入）。

## 局域网共享（可选）

默认仅本机可访问。共享给局域网时**强制访问令牌**（三口校验：serve 启动、CLI 保存、管理 API 写回均拒绝无令牌的非回环配置）：

```sh
yardmaster settings set token <随机长令牌>
yardmaster settings set listen 0.0.0.0:8317
yardmaster serve   # Agent 与浏览器请求携带该令牌（Authorization: Bearer / x-api-key / X-Gw-Token）
```

## 安全基线（不可协商）

- 默认仅监听 `127.0.0.1`；非回环监听必须配置访问令牌
- 上游密钥与令牌仅存本机，日志与界面输出经脱敏（API 输出 `***+末 4 位`，脱敏往返不破坏原值）
- Agent 配置改写只动自己名下的键，原始配置进备份栈
- 配置文件损坏时拒绝覆写（读不懂 = 未知），绝不当作空配置覆盖

## 平台

Linux（amd64/arm64）与 Windows（amd64），见 [Releases](../../releases)。开发文档见 `docs/`。

## 构建

```sh
go build -o bin/ ./cmd/yardmaster
go test ./...
```
