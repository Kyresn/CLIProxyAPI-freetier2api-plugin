package cline

// 本文件实现 Cline 凭证的解析与归属判定。
//
// Cline 的凭证来自 WorkOS 设备码登录：真正的长期凭据是 refreshToken，
// accessToken 有有效期、由续期链路轮换。
//
// **出站 token 必须带 workos: 前缀**：上游按这个前缀识别凭证来源，
// 漏掉会被当成无效令牌。前缀在 credential.go 里统一补，调用方不必记。

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/core"
)

// VendorID 是供应商实例标识。同时是凭证文件名前缀。
//
// Cline 只有一个部署（api.cline.bot），没有国内/国际之分。
const VendorID = "cline"

// VendorName 是管理端展示名。
const VendorName = "Cline"

// workosTokenPrefix 是出站 Bearer 令牌必须携带的前缀。
//
// 这是上游的硬要求（不是装饰）：Cline 用它区分 WorkOS 签发的令牌与
// 其它来源的令牌。登录、续期、导入三条路径都要补。
const workosTokenPrefix = "workos:"

// Credential 是归一化后的 Cline 凭证。
type Credential struct {
	// AccessToken 是出站令牌（**不含** workos: 前缀，发送时由 BearerToken 补）。
	AccessToken string
	// RefreshToken 是长期凭据（登录产物，续期用它换新的 access token）。
	RefreshToken string
	// ExpiresAt 是 AccessToken 的过期时刻（Unix 秒）；0 表示未知。
	ExpiresAt int64
	// Email 是账号邮箱（登录流程会填）。
	Email string
	// Label 是展示名。
	Label string
	// AccountID 是账号唯一标识（登录产物，缺失时用邮箱）。
	AccountID string
}

// BearerToken 返回可直接放进 Authorization 头的令牌。
//
// 补 workos: 前缀是**幂等**的：从文件导入的凭证可能已经带了前缀
// （cline2api 的导出格式就带），重复补会变成 workos:workos:…。
func (c *Credential) BearerToken() string {
	if c == nil {
		return ""
	}
	return WithWorkOSPrefix(c.AccessToken)
}

// WithWorkOSPrefix 给令牌补上 workos: 前缀（已有则原样返回）。
func WithWorkOSPrefix(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, workosTokenPrefix) {
		return trimmed
	}
	return workosTokenPrefix + trimmed
}

// StripWorkOSPrefix 去掉 workos: 前缀（导入时用）。
func StripWorkOSPrefix(token string) string {
	return strings.TrimPrefix(strings.TrimSpace(token), workosTokenPrefix)
}

// ParseCredential 解析 Cline 凭证 JSON。
//
// 字段名候选是刻意的：Cline 的凭证在不同来源里写法不同——
//   - 本插件登录落盘：accessToken / refreshToken（驼峰，与上游一致）；
//   - cline2api 导出：access_token / refresh_token（下划线）；
//   - 手写：可能只给一个 refreshToken。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{
		AccessToken:  core.GetString(payload, "accessToken", "access_token"),
		RefreshToken: core.GetString(payload, "refreshToken", "refresh_token"),
		Email:        core.GetString(payload, "email", "account", "userEmail"),
		Label:        core.GetString(payload, "label", "name"),
		AccountID:    core.GetString(payload, "accountId", "account_id", "uid"),
	}
	cred.ExpiresAt = parseExpiry(payload["expiresAt"])
	if cred.ExpiresAt == 0 {
		cred.ExpiresAt = parseExpiry(payload["expires_at"])
	}
	// 令牌里的 workos: 前缀在归一化时剥掉，发送时由 BearerToken 统一补。
	// 这样文件里存的是裸令牌，不会出现 workos:workos: 的累积。
	cred.AccessToken = StripWorkOSPrefix(cred.AccessToken)

	if cred.AccessToken == "" && cred.RefreshToken == "" {
		return nil, fmt.Errorf("credential has neither accessToken nor refreshToken")
	}
	return cred, nil
}

// LooksLikeCredential 判断一份 JSON 是否属于 Cline。
//
// **判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，
// 误吞别家的会让宿主用本插件的结构覆盖对方账号（且是静默的）。
//
// 只接受 Cline 特有的证据：
//  1. 显式声明了别的 vendor 则绝不认领；
//  2. 令牌带 workos: 前缀（Cline 独有）；
//  3. 文件名含 cline 段；
//  4. 绝不认领明显属于其它供应商的文件名（如 workbuddy-*.json 等）；
//  5. 绝不认领含其它供应商特征（domain, device_token, auth/account 对象等）的凭证；
//  6. 在排除全部其它供应商特征后，才允许无异义的 refreshToken 兜底。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	// 显式 vendor 字段优先：声明了别家就绝不认领。
	if decided, claimed := core.VendorFieldVerdict(raw, VendorID); decided {
		return claimed
	}
	if core.FileNameMatchesConvention(fileName, VendorID) {
		return true
	}
	if raw == nil {
		return false
	}
	if hasWorkOSPrefix(raw) {
		return true
	}
	// 跨供应商误吞防护：若文件名或字段带有其它供应商的特征，绝不认领。
	if isForeignFileName(fileName) || hasForeignMarker(raw) {
		return false
	}
	// 驼峰 refreshToken 在排除全部别家特征后作为通用兜底。
	if _, okRefresh := raw["refreshToken"]; okRefresh {
		return true
	}
	_ = provider
	return false
}

// isForeignFileName 报告文件名是否符合其它已知供应商的命名约定。
func isForeignFileName(fileName string) bool {
	return core.FileNameMatchesConvention(fileName,
		"workbuddy", "workbuddycn", "workbuddyglobal", "codebuddy",
		"qoder", "qodercn", "qoderglobal",
		"trae", "traecn", "traeglobal", "traesolo",
		"minimax", "minimaxcode", "minimaxcodecn", "minimaxcodeglobal",
		"opencode", "opencodezen", "zen",
		"zcode", "zcodecn", "zcodeglobal", "codearts",
	)
}

// hasForeignMarker 报告凭证里是否带有其它供应商的专属或特征字段。
func hasForeignMarker(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	for _, key := range []string{
		"device_token", "enterpriseId", "enterprise_id",
		"domain", "realm", "region", "secret", "zen_key",
		"agent_id", "agentId", "realUserID", "real_user_id",
		"access_key_id", "secret_access_key", "dpop_private_key",
	} {
		if val, exists := raw[key]; exists && val != nil && val != "" {
			return true
		}
	}
	// WorkBuddy 的嵌套形态：auth 或 account 是对象。
	if authMap, ok := raw["auth"].(map[string]any); ok && len(authMap) > 0 {
		return true
	}
	if accountMap, ok := raw["account"].(map[string]any); ok && len(accountMap) > 0 {
		return true
	}
	// 其它供应商的 type 声明。
	if typeVal, ok := raw["type"].(string); ok {
		trimmed := strings.ToLower(strings.TrimSpace(typeVal))
		if trimmed != "" && trimmed != "freetier" && trimmed != "cline" {
			return true
		}
	}
	return false
}

// hasWorkOSPrefix 判断凭证里是否有带 workos: 前缀的令牌。
func hasWorkOSPrefix(raw map[string]any) bool {
	for _, key := range []string{"accessToken", "access_token", "token", "refreshToken", "refresh_token"} {
		if value, okValue := raw[key].(string); okValue {
			if strings.HasPrefix(strings.TrimSpace(value), workosTokenPrefix) {
				return true
			}
		}
	}
	return false
}
