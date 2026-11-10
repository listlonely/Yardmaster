// Provider 预检（PRD F2 第④步）：验证凭证 + 拉取模型清单。
// 探测请求经账号→Provider→全局继承链上的代理（F3）。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"yardmaster/internal/netproxy"
	"yardmaster/internal/provider"
)

// probeTimeout 是预检的硬上限。
const probeTimeout = 20 * time.Second

// ProbeAccount 验证账号凭证并拉取模型清单。非 2xx 返回带状态码与
// 响应片段的错误；成功返回发现的模型列表。authStyle 覆盖 Anthropic
// 协议的认证头（""=默认 x-api-key；"bearer"=Authorization Bearer，
// W4 自动学习数据源；OpenAI 协议恒为 Bearer）。
//
// 四协议端点 [2026-11-09-probe-all]：openai-chat / openai-responses
// 同源 /models + Bearer；anthropic-messages /v1/models；gemini
// /v1beta/models（x-goog-api-key 头，剥 "models/" 前缀，仅保留支持
// generateContent 的模型——与出站 /v1beta/models/{model}:generateContent
// 的裸模型名形状对齐）。
func ProbeAccount(ctx context.Context, prov *provider.Provider, acc provider.Account,
	globalProxy string, rt http.RoundTripper, authStyle string) ([]provider.Model, error) {
	var path string
	switch prov.Protocol {
	case provider.ProtocolMessages:
		path = "/v1/models"
	case provider.ProtocolGemini:
		path = "/v1beta/models"
	default:
		path = "/models" // openai-chat / openai-responses：同一 models 端点
	}
	url := strings.TrimRight(prov.BaseURL, "/") + path

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造预检请求: %w", err)
	}
	switch prov.Protocol {
	case provider.ProtocolMessages:
		if authStyle == "bearer" {
			req.Header.Set("Authorization", "Bearer "+acc.APIKey)
		} else {
			req.Header.Set("x-api-key", acc.APIKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	case provider.ProtocolGemini:
		req.Header.Set("x-goog-api-key", acc.APIKey)
	default:
		req.Header.Set("Authorization", "Bearer "+acc.APIKey)
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("预检失败（HTTP %d）: %s", resp.StatusCode, netproxy.Snippet(body))
	}
	if prov.Protocol == provider.ProtocolGemini {
		return parseGeminiModels(body)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	models := make([]provider.Model, 0, len(list.Data))
	for _, d := range list.Data {
		if d.ID != "" {
			models = append(models, provider.Model{ID: d.ID})
		}
	}
	return models, nil
}

// parseGeminiModels 解析 Google models.list 响应：剥 "models/" 前缀
// （出站按 /v1beta/models/{裸名}:generateContent 调用），过滤出支持
// generateContent 的条目（embedding 等非对话模型不进目录）。
func parseGeminiModels(body []byte) ([]provider.Model, error) {
	var list struct {
		Models []struct {
			Name      string   `json:"name"`
			Methods   []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	models := make([]provider.Model, 0, len(list.Models))
	for _, m := range list.Models {
		if !slices.Contains(m.Methods, "generateContent") {
			continue
		}
		if id := strings.TrimPrefix(m.Name, "models/"); id != "" {
			models = append(models, provider.Model{ID: id})
		}
	}
	return models, nil
}

// ResolveProxyRef 解析代理继承链（F3）：账号 → Provider → 全局；
// 任一层「显式直连」阻断向上继承。返回生效的 ref 与来源说明。
func ResolveProxyRef(account, prov, global string) (effective, source string) {
	for _, c := range []struct {
		ref, src string
	}{{account, "账号"}, {prov, "Provider"}, {global, "全局"}} {
		if c.ref == "" {
			continue
		}
		if c.ref == string(netproxy.Direct) {
			return c.ref, c.src + "（显式直连）"
		}
		return c.ref, c.src
	}
	return "", "系统代理/直连"
}
