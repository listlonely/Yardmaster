// OAuth token 刷新（M3-W3）：标准 RFC6749 refresh_token grant。
// 端点与客户端标识取自各 Agent 自身的公开配置（installed-app 客户端
// 凭证本就随 Agent 分发）；刷新的只是用户已登录会话的 access token，
// 不做任何订阅桥接（ToS 边界，见开发文档 §9.4）。
package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RefreshSpec 是一个厂商的刷新端点与客户端标识。
type RefreshSpec struct {
	TokenURL     string `json:"token_url"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"` // 仅 installed-app 型客户端（如 Gemini CLI）需要
}

// builtinRefreshSpecs 以导入格式名为键。值来自各 Agent 自身源码中的
// 公开 OAuth 客户端配置；若厂商更换，更新此处即可（也可在测试中注入）。
var builtinRefreshSpecs = map[string]RefreshSpec{
	"claude-code": {
		TokenURL: "https://console.anthropic.com/v1/oauth/token",
		ClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e", // Claude Code 公开 OAuth client
	},
	"codex": {
		TokenURL: "https://auth.openai.com/oauth/token",
		ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", // Codex CLI 公开 OAuth client
	},
	"gemini-cli": {
		TokenURL: "https://oauth2.googleapis.com/token",
		ClientID: "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com",
		// ClientSecret 不内置：installed-app secret 属公开常量但会触发
		// GitHub secret scanning（推送拦截）。经 settings 注入：
		// `yardmaster settings set gemini_client_secret <值>`（取自
		// Gemini CLI 自身源码），runServe 装配时覆盖本规格。
	},
}

// RefreshSpecFor 返回导入格式对应的刷新规格；未注册返回 false。
func RefreshSpecFor(format string) (RefreshSpec, bool) {
	s, ok := builtinRefreshSpecs[format]
	return s, ok
}

// RefreshTokenResponse 是 token 端点的归一响应（各厂商字段名同源）。
type RefreshTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"` // 部分厂商不轮换：缺省保留旧值
	ExpiresIn    int64  `json:"expires_in"`    // 秒
	Scope        string `json:"scope"`
}

// Refresh 用 refresh_token 换新 access token。HTTP/解析失败一律返回
// 错误（含端点响应体片段，不含任何 token 值——响应体只取错误摘要）。
func Refresh(ctx context.Context, hc *http.Client, spec RefreshSpec, refreshToken string) (RefreshTokenResponse, error) {
	if spec.TokenURL == "" || spec.ClientID == "" {
		return RefreshTokenResponse{}, fmt.Errorf("refresh spec 不完整（缺 token_url 或 client_id）")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return RefreshTokenResponse{}, fmt.Errorf("缺少 refresh_token，无法刷新")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {spec.ClientID},
	}
	if spec.ClientSecret != "" {
		form.Set("client_secret", spec.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("构造刷新请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("刷新请求失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("读取刷新响应: %w", err)
	}
	var tr RefreshTokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return RefreshTokenResponse{}, fmt.Errorf("刷新响应解析失败（HTTP %d）: %s", resp.StatusCode, Snippet(b))
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		return RefreshTokenResponse{}, fmt.Errorf("刷新被拒绝（HTTP %d）: %s", resp.StatusCode, Snippet(b))
	}
	return tr, nil
}

// MergeRefresh 把刷新结果并入旧凭证：token/过期/scopes 以新值为准，
// 厂商特有 Extra 保留（不轮换 refresh_token 时保留旧值）。
func MergeRefresh(old OAuth, tr RefreshTokenResponse, now time.Time) OAuth {
	next := old
	next.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		next.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		next.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	if tr.Scope != "" {
		next.Scopes = strings.Fields(tr.Scope)
	}
	return next
}

// Snippet 是响应体单行摘要（脱敏：限长，调用方不落 token——错误信息
// 通常只有 error code；完整响应体不外泄）。
func Snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
