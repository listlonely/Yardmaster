# Yardmaster

**Yardmaster**（调车长）是一个运行在本机的 AI 网关：把多个 AI Agent（Claude Code、Codex、OpenCode 等）的请求，按任务类型自动路由到合适的模型与账号——Agent 只管发车，调车长负责把每趟请求拨上正确的股道。

```text
Agent ──▶ 127.0.0.1:8317 ──▶ Yardmaster ──▶ 按任务/额度/策略 ──▶ 多家上游模型
```

## 核心能力

- **双协议网关**：OpenAI Chat Completions ↔ Anthropic Messages 双向解析与翻译（文本 + 工具，流式/非流式）
- **Agent 接线**：声明式改写 Agent 配置指向网关（预览 diff、备份栈、一键还原、漂移检测）
- **路由引擎**：`provider/model`、裸 provider 自动组、`group/<id>` 跨上游路由组；order / rotate / smart / usage / weight（平滑加权）五策略
- **额度感知**：账号当日额度耗尽自动休眠至次日重置，提前切下一候选（不等上游 429）；smart 按剩余比例排序
- **成本核算**：模型单价表（pricing.json）× 决策 trace 聚合，按账号/模型出成本报告（零新增持久化）
- **故障转移**：429/5xx/网络错自动切换候选并休眠，额度断流无感接续；会话粘性（`X-Gw-Session`）
- **任务规则 + 意图分类**：rules.json 首条命中改道；自定义意图词表热生效；内置 L0 启发式 + 可选 L1 小模型分类（缓存/冷却/回落）
- **Web 控制台**：总览/Provider/路由/观测四页；配置读写全走管理 API（严格校验 + 写前备份栈 + 审计日志），不手改配置文件即可全量管理
- **决策追溯**：每次路由决策记录完整候选尝试链（谁调用、命中什么、试过谁、为何切换），落盘 30 天/200MB，`/api/trace`、控制台与 CLI 可查
- **代理挂载**：全局 → Provider → 账号三级继承（http/https/socks5/socks5h/direct），HTTP/2 半死连接健康检查
- **局域网共享**：`listen` + 强制令牌（token）；控制台/API/推断端点统一过闸，默认仅本机

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
# 1. 添加上游（预检验证凭证并拉取模型清单）
yardmaster provider add -id siliconflow -base-url https://api.siliconflow.cn/v1 -key sk-xxx
yardmaster provider probe siliconflow

# 2. 把 Agent 接到网关（预览确认后改写其配置）
yardmaster agents preview claude-code -model yardmaster-main claude-code
yardmaster agents wire claude-code -model yardmaster-main

# 3. 启动网关（默认仅监听 127.0.0.1:8317）
yardmaster serve

# 4. 浏览器打开控制台（总览 / Provider / 路由 / 观测）
#    http://127.0.0.1:8317/

# 5. 观测与经营
yardmaster trace list                 # 路由决策追溯
yardmaster cost --days 7              # 成本报告（先 price set 配单价）
yardmaster tui                        # 终端面板：状态 / 追溯 / 绑定切换
yardmaster update check               # 升级提示
```

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
