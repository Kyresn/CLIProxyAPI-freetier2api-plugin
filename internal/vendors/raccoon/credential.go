package raccoon

// 本文件实现小浣熊凭证的解析、归属判定、脱敏与写回合并。
//
// 凭证形态（与 Agent2API / Agent2API 的账号文件对齐）：
//
//	{
//	  "vendor": "raccoon",
//	  "access_token": "eyJ…",
//	  "refresh_token": "eyJ…",
//	  "expires_at": 1791529021000,
//	  "uid": "7591340",
//	  "nickname": "小浣熊-1"
//	}
//
// access/refresh 都是上游直发的 JWT；每次续期**可能轮换** refresh_token，
// 新票必须写回。expires_at 是毫秒时间戳（0 = 未知，靠 401 触发续期）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"freetier2api-plugin/internal/core"
)

// VendorID 是供应商实例标识。同时是凭证文件名前缀（raccoon-<uid>.json）。
const VendorID = "raccoon"

// VendorName 是管理端展示名。
const VendorName = "小浣熊"

// protocolName 是家族别名（文件名中缀匹配用）。
const protocolName = "raccoon"

// ErrMissingToken 表示凭证里没有可用的 token。
var ErrMissingToken = errors.New("credential has neither accessToken nor refreshToken")

// Credential 是归一化后的小浣熊凭证。
//
// 并发约定与 core.Credential 一致：出站取值经取值方法加锁快照，
// 刷新线程会并发改写 token 字段。
type Credential struct {
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	// ExpiresAt 是 access token 过期时刻（Unix **秒**，解析时从毫秒归一）；
	// 0 表示未知。
	ExpiresAt int64
	UID       string
	Nickname  string
	// OfficeIdentity 是组织码（个人账号为空），作为 X-Org-Code 发送。
	OfficeIdentity string
	// DeviceID 是桌面客户端设备标识，非空时作为 X-Client-Device-ID 发送。
	DeviceID string
}

// ParseCredential 解析小浣熊凭证 JSON。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred := &Credential{}
	getStr := func(keys ...string) string {
		for _, key := range keys {
			if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	cred.AccessToken = getStr("access_token", "accessToken", "token")
	cred.RefreshToken = getStr("refresh_token", "refreshToken")
	cred.Nickname = getStr("nickname", "name", "label")
	cred.UID = getStr("uid", "user_id", "userId")
	cred.OfficeIdentity = getStr("office_identity", "officeIdentity", "org_code", "orgCode")
	cred.DeviceID = getStr("device_id", "deviceId")
	if cred.ExpiresAt = core.GetUnixSeconds(payload, "expires_at", "expiresAt"); cred.ExpiresAt == 0 {
		// 响应里可能只有 JWT：从 claims 读 exp 兜底。
		cred.ExpiresAt = core.JWTExpiry(cred.AccessToken)
	}
	if strings.TrimSpace(cred.AccessToken) == "" && strings.TrimSpace(cred.RefreshToken) == "" {
		return nil, ErrMissingToken
	}
	return cred, nil
}

// LooksLikeCredential 判断一份 JSON 是否属于小浣熊。
//
// **归属判定必须偏严**（与各供应商同一纪律）：宿主会把所有非内建格式的
// 凭证依次喂给每个插件，误吞别家的会让宿主用本插件的结构覆盖对方账号
// （且是静默的）。漏认只是加载失败，用户立刻会发现，代价小得多。
//
// 只接受小浣熊特有的证据：
//  1. 文件名含 raccoon / xiaohuanxiong 段；
//  2. 凭证显式声明 vendor: raccoon；
//  3. 凭证带小浣熊帐号文件特征组合（uid + snake_case token 对，且无别家特征字段）。
//
// 刻意不接受 accessToken / refreshToken 驼峰对做判据：workbuddy / trae 等
// 也用它们；snake_case 的 access_token/refresh_token + uid 组合是小浣熊
// 帐号文件的固定形态（Agent2API 系列的落盘格式）。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if core.FileNameMatchesConvention(fileName, VendorID, protocolName, "xiaohuanxiong") {
		return true
	}
	if raw == nil {
		return false
	}
	if vendor, okVendor := raw[core.VendorKey].(string); okVendor {
		if strings.EqualFold(strings.TrimSpace(vendor), VendorID) {
			return true
		}
	}
	// 特征组合：Agent2API 落盘的 snake_case 对 + uid。
	_, hasAccess := raw["access_token"]
	_, hasRefresh := raw["refresh_token"]
	_, hasUID := raw["uid"]
	if hasAccess && hasRefresh && hasUID {
		return true
	}
	_ = provider
	return false
}

// ---- 加锁取值（供并发出站使用） ----

// AccessTokenValue 返回加锁快照的 access token。
func (c *Credential) AccessTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken
}

// RefreshTokenValue 返回加锁快照的 refresh token。
func (c *Credential) RefreshTokenValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.RefreshToken
}

// ExpiresAtValue 返回加锁快照的过期时刻（Unix 秒）。
func (c *Credential) ExpiresAtValue() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ExpiresAt
}

// UIDValue 返回加锁快照的用户 ID。
func (c *Credential) UIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.UID
}

// NicknameValue 返回加锁快照的展示名。
func (c *Credential) NicknameValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Nickname
}

// OfficeIdentityValue 返回加锁快照的组织码（个人账号为空）。
func (c *Credential) OfficeIdentityValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.OfficeIdentity
}

// DeviceIDValue 返回加锁快照的客户端设备标识。
func (c *Credential) DeviceIDValue() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DeviceID
}

// Snapshot 返回加锁快照的 token 对。
func (c *Credential) Snapshot() (access, refresh string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken, c.RefreshToken
}

// UnchangedSince 报告刷新前后 token 是否都没变。
func (c *Credential) UnchangedSince(accessBefore, refreshBefore string) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.AccessToken == accessBefore && c.RefreshToken == refreshBefore
}

// ApplyTokens 写入续期结果（refresh 为空表示上游未轮换，沿用旧值）。
func (c *Credential) ApplyTokens(access, refresh string, expiresAt int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(access) != "" {
		c.AccessToken = strings.TrimSpace(access)
	}
	if strings.TrimSpace(refresh) != "" {
		c.RefreshToken = strings.TrimSpace(refresh)
	}
	if expiresAt > 0 {
		c.ExpiresAt = expiresAt
	}
}

// ApplyIdentity 补齐 uid / 昵称（续期或登录响应里拿到的）。
func (c *Credential) ApplyIdentity(uid, nickname string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(uid) != "" && c.UID == "" {
		c.UID = strings.TrimSpace(uid)
	}
	if strings.TrimSpace(nickname) != "" && c.Nickname == "" {
		c.Nickname = strings.TrimSpace(nickname)
	}
}

// UIDOrTokenID 返回账号标识：UID 优先，退回 JWT claims 里的用户 id。
func (c *Credential) UIDOrTokenID() string {
	if c == nil {
		return ""
	}
	if uid := c.UIDValue(); uid != "" {
		return uid
	}
	return core.JWTUserID(c.AccessTokenValue())
}

// StorageJSON 把凭证序列化成落盘 JSON（vendor 字段冗余声明归属）。
func (c *Credential) StorageJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("credential is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := map[string]any{
		core.VendorKey:  VendorID,
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt * 1000,
	}
	if c.UID != "" {
		payload["uid"] = c.UID
	}
	if c.Nickname != "" {
		payload["nickname"] = c.Nickname
	}
	if c.OfficeIdentity != "" {
		payload["office_identity"] = c.OfficeIdentity
	}
	if c.DeviceID != "" {
		payload["device_id"] = c.DeviceID
	}
	return json.MarshalIndent(payload, "", "  ")
}

// ---------------------------------------------------------------------------
// 凭证写回合并
// ---------------------------------------------------------------------------

// 复用 core.MergeCredentialJSON 骨架：只覆盖 token 相关字段，
// 用户手工维护的 note / disabled 等字段原样保留。

// MergeCredentialJSON 把续期后的令牌合并回原凭证 JSON。
func MergeCredentialJSON(original []byte, cred *Credential) ([]byte, error) {
	return core.MergeCredentialJSON(original, cred.StorageJSON, func(target map[string]any) {
		target["access_token"] = cred.AccessTokenValue()
		target["refresh_token"] = cred.RefreshTokenValue()
		target["expires_at"] = cred.ExpiresAtValue() * 1000
	})
}
