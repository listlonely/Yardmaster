# netproxy — 代理与上游通道子系统

## 职责与真源

| 部分 | 职责 | 真源 |
| --- | --- | --- |
| 代理解析 | `Parse`：http/https/socks5/socks5h + direct；空=继承上一级；其他 scheme 输入即拒 | [`netproxy.go`](../../internal/netproxy/netproxy.go) |
| 凭证脱敏 | `Redact`：任何输出只显示 `scheme://***@host:port` | [`netproxy.go`](../../internal/netproxy/netproxy.go) |
| 共享通道 | `SharedTransport`：按代理端点（scheme+host）缓存 RoundTripper，同端点多账号共享连接池；凭证随请求走不参与缓存键 | [`cache.go`](../../internal/netproxy/cache.go) |
| HTTP/2 健康检查 | 每个 Transport 经 `http2.ConfigureTransports` 启用 ReadIdleTimeout=20s / PingTimeout=10s：静默连接 ping、无 pong 判死；长等待响应（预检/生图类）不受影响 | [`cache.go`](../../internal/netproxy/cache.go) |
| 连通性预检 | `TestConnectivity`：经代理对目标 GET 一次，任何状态码都算连通；一次性 Transport，不占缓存 | [`health.go`](../../internal/netproxy/health.go) |
| 响应摘要 | `Snippet`：错误信息中上游响应体截断为 ≤200 字节单行 | [`health.go`](../../internal/netproxy/health.go) |

## 继承链（F3）[2026-10-29-代理继承链direct阻断]

解析在 `gateway.ResolveProxyRef`：**账号 → Provider → 全局**（`settings.json` 的
`global_proxy`），任一层 `direct` 阻断向上继承；全部为空时跟随系统代理直连。
网关侧 `Server.transportFor` 每候选调用；预检侧 `provider probe` 按同一链路。

## 约束与失效行为

- 共享缓存键不含凭证：同端点不同凭证的账号共享连接池（HTTP 代理凭证经
  Proxy-Authorization 随请求走，SOCKS5 凭证在建连时给出——共享时以先建
  者为准，同端点异凭证账号需配置为不同端点书写形式；W3 已知边界）。
- 预检失败的账号标记 `prefail`（仍参与路由，持续提示）；凭证 401/403 在
  网关运行期短休眠 30s，W3 后续升级为 bad-credential 标记。
- 日志与错误信息中代理凭证、上游 key 一律经 `Redact`/掩码（§4.4）。

## 验证

```sh
go test ./internal/netproxy
go test ./internal/gateway -run 'Probe|ResolveProxyRef'
```
