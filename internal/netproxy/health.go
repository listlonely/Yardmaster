// 代理连通性预检（PRD F3）：保存前/随时可对代理发一次探测请求，
// 让代理故障在配置时被发现而不是在请求失败时。任何 HTTP 状态码都算
// 连通（网络层通即通过，上层鉴权问题是上游的事）。
package netproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// probeTimeout 是单次预检的硬上限。
const probeTimeout = 15 * time.Second

// TestConnectivity 经 ref 代理对 target 发起 GET 探测，返回耗时。
// ref 为空或 direct 时直连。探测使用一次性 Transport（预检低频，
// 不占用共享连接池缓存）。
func TestConnectivity(ctx context.Context, ref Ref, target string) (time.Duration, error) {
	rt, err := Transport(ref)
	if err != nil {
		return 0, err
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("构造探测请求: %w", err)
	}
	start := time.Now()
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return time.Since(start), nil
}

// Snippet 把上游响应体截断为安全的单行摘要（≤200 字节，控制字符转空格），
// 供错误信息与日志使用——绝不输出完整响应体（§4.4）。
func Snippet(body []byte) string {
	const max = 200
	b := body
	if len(b) > max {
		b = b[:max]
	}
	var out []byte
	for _, c := range b {
		if c < 0x20 && c != '\t' {
			c = ' '
		}
		out = append(out, c)
	}
	s := string(out)
	if len(body) > max {
		s += "…"
	}
	return s
}
