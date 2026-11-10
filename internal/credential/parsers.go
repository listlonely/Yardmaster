// 内置导入解析器（M3-W2）：把三大 Agent 的 OAuth 凭证文件归一化为
// credential.OAuth。字段与形状依据各 Agent 真实凭证数据（样例见
// testdata/credentials/，来源与复核要求见其 README），参照 Magpie
// internal/provider/account.go 的 claudeAuth 结构对齐。
//
// 解析器契约（§4.2 读不懂=未知）：任何缺字段/空 token/形状不符都必须
// 返回错误，绝不返回空凭证；BOM/CRLF 容错；时间戳毫秒自动归一到秒。
package credential

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	for name, fn := range map[string]Parser{
		"claude-code": parseClaudeCode,
		"codex":       parseCodex,
		"gemini-cli":  parseGeminiCLI,
	} {
		if err := RegisterParser(name, fn); err != nil {
			panic(fmt.Sprintf("credential: 内置解析器注册失败: %v", err))
		}
	}
}

// trimJSONBOM 去掉 UTF-8 BOM（Windows 编辑器常态，与 store.LoadJSON 同规）。
func trimJSONBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
}

// normExpirySec 归一过期时间：Agent 凭证多用 Unix 毫秒（>1e12 判为毫秒）。
func normExpirySec(v int64) int64 {
	if v > 1_000_000_000_000 {
		return v / 1000
	}
	return v
}

// DefaultPath 返回各 Agent 凭证文件的默认路径（-from <格式> 省略路径时
// 使用）。仅覆盖 Linux/Windows 形态；macOS Keychain 不在本产品范围。
func DefaultPath(format string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	switch format {
	case "claude-code":
		return filepath.Join(home, ".claude", ".credentials.json"), true
	case "codex":
		return filepath.Join(home, ".codex", "auth.json"), true
	case "gemini-cli":
		return filepath.Join(home, ".gemini", "oauth_creds.json"), true
	}
	return "", false
}

// parseClaudeCode 解析 Claude Code 的 ~/.claude/.credentials.json：
// {"claudeAiOauth":{"accessToken","refreshToken","expiresAt"(ms),
// "refreshTokenExpiresAt"(ms),"scopes",["subscriptionType","rateLimitTier"]}}。
func parseClaudeCode(data []byte) (OAuth, error) {
	var f struct {
		ClaudeAiOauth *struct {
			AccessToken      string   `json:"accessToken"`
			RefreshToken     string   `json:"refreshToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			RefreshExpiresAt int64    `json:"refreshTokenExpiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
			RateLimitTier    string   `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(trimJSONBOM(data), &f); err != nil {
		return OAuth{}, fmt.Errorf("claude-code 凭证解析失败: %w", err)
	}
	if f.ClaudeAiOauth == nil || strings.TrimSpace(f.ClaudeAiOauth.AccessToken) == "" {
		return OAuth{}, fmt.Errorf("claude-code 凭证缺少 claudeAiOauth.accessToken（可能未登录或文件损坏）")
	}
	c := f.ClaudeAiOauth
	o := OAuth{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		ExpiresAt:    normExpirySec(c.ExpiresAt),
		Scopes:       c.Scopes,
		Extra:        map[string]string{},
	}
	if c.RefreshExpiresAt > 0 {
		o.Extra["refresh_expires_at"] = fmt.Sprint(normExpirySec(c.RefreshExpiresAt))
	}
	if c.SubscriptionType != "" {
		o.Extra["subscription_type"] = c.SubscriptionType
	}
	if c.RateLimitTier != "" {
		o.Extra["rate_limit_tier"] = c.RateLimitTier
	}
	return o, nil
}

// parseCodex 解析 Codex 的 ~/.codex/auth.json：
// {"OPENAI_API_KEY":null,"auth_mode":"chatgpt","tokens":{"id_token",
// "access_token","refresh_token","account_id"},"last_refresh":RFC3339}。
// OPENAI_API_KEY 非 null（API key 登录）不是 OAuth 凭证，拒绝导入。
func parseCodex(data []byte) (OAuth, error) {
	var f struct {
		OPENAI_API_KEY *string `json:"OPENAI_API_KEY"`
		AuthMode       string  `json:"auth_mode"`
		Tokens         *struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(trimJSONBOM(data), &f); err != nil {
		return OAuth{}, fmt.Errorf("codex 凭证解析失败: %w", err)
	}
	if f.OPENAI_API_KEY != nil && *f.OPENAI_API_KEY != "" {
		return OAuth{}, fmt.Errorf("codex 为 API key 登录（OPENAI_API_KEY 非空），请用 provider add -key 导入")
	}
	if f.Tokens == nil || strings.TrimSpace(f.Tokens.AccessToken) == "" {
		return OAuth{}, fmt.Errorf("codex 凭证缺少 tokens.access_token（可能未登录或文件损坏）")
	}
	o := OAuth{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		Extra:        map[string]string{},
	}
	if f.Tokens.AccountID != "" {
		o.Extra["account_id"] = f.Tokens.AccountID
	}
	if f.AuthMode != "" {
		o.Extra["auth_mode"] = f.AuthMode
	}
	if f.LastRefresh != "" {
		o.Extra["last_refresh"] = f.LastRefresh
	}
	return o, nil
}

// parseGeminiCLI 解析 Gemini CLI 的 ~/.gemini/oauth_creds.json（平铺）：
// {"access_token","refresh_token","scope","token_type","id_token",
// "expiry_date"(ms)}。
func parseGeminiCLI(data []byte) (OAuth, error) {
	var f struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
		ExpiryDate   int64  `json:"expiry_date"`
	}
	if err := json.Unmarshal(trimJSONBOM(data), &f); err != nil {
		return OAuth{}, fmt.Errorf("gemini-cli 凭证解析失败: %w", err)
	}
	if strings.TrimSpace(f.AccessToken) == "" {
		return OAuth{}, fmt.Errorf("gemini-cli 凭证缺少 access_token（可能未登录或文件损坏）")
	}
	o := OAuth{
		AccessToken:  f.AccessToken,
		RefreshToken: f.RefreshToken,
		ExpiresAt:    normExpirySec(f.ExpiryDate),
		Extra:        map[string]string{},
	}
	if f.Scope != "" {
		o.Scopes = strings.Fields(f.Scope)
	}
	if f.TokenType != "" {
		o.Extra["token_type"] = f.TokenType
	}
	return o, nil
}
