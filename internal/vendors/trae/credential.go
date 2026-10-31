package trae

import (
	"encoding/json"
	"fmt"
	"strings"

	"freetier2api-plugin/internal/core"
)

// 本文件实现 Trae 家族凭证的解析、特征识别与存储。

type Credential struct {
	Vendor       string `json:"vendor,omitempty"`
	Region       Region `json:"region,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	MachineID    string `json:"machine_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`
}

// ParseCredential 解析 JSON 凭证（保留给协议层测试使用）。
func ParseCredential(raw []byte, defaultRegion Region) (*Credential, error) {
	return parseCredential(raw, VendorIDFor(defaultRegion), defaultRegion)
}

// ParseCredentialFor 按实例 profile 解析凭证。
func ParseCredentialFor(raw []byte, cfg Config) (*Credential, error) {
	return parseCredential(raw, cfg.VendorID, cfg.Region)
}

func parseCredential(raw []byte, defaultVendor string, defaultRegion Region) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("trae credential is empty")
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode trae credential json: %w", err)
	}

	authObj := core.GetMap(m, "auth")
	if authObj == nil {
		authObj = m
	}
	accountObj := core.GetMap(m, "account")

	vendor := strings.ToLower(core.FirstNonEmpty(core.GetString(m, "vendor"), defaultVendor))
	r := defaultRegion
	regionStr := strings.ToLower(core.FirstNonEmpty(
		core.GetString(m, "region"),
		core.GetString(m, "realm"),
		core.GetString(m, "_edition"),
		string(defaultRegion),
	))
	switch {
	case regionStr == "global" || regionStr == "sg" || regionStr == "traeglobal":
		r = RegionGlobal
	default:
		r = RegionCN
	}
	c := &Credential{
		Vendor:       core.FirstNonEmpty(vendor, defaultVendor),
		Region:       r,
		AccessToken:  core.FirstNonEmpty(core.GetString(authObj, "access_token"), core.GetString(authObj, "accessToken"), core.GetString(authObj, "token")),
		RefreshToken: core.FirstNonEmpty(core.GetString(authObj, "refresh_token"), core.GetString(authObj, "refreshToken")),
		ExpiresAt:    core.GetUnixSeconds(authObj, "expires_at", "expiresAt", "TokenExpireAt"),
		UID:          core.FirstNonEmpty(core.GetString(authObj, "uid"), core.GetString(accountObj, "uid"), core.GetString(m, "uid")),
		Nickname:     core.FirstNonEmpty(core.GetString(authObj, "nickname"), core.GetString(accountObj, "nickname"), core.GetString(m, "nickname")),
		EnterpriseID: core.FirstNonEmpty(core.GetString(authObj, "enterprise_id"), core.GetString(accountObj, "enterprise_id"), core.GetString(m, "enterprise_id")),
		MachineID:    core.FirstNonEmpty(core.GetString(authObj, "machine_id"), core.GetString(authObj, "machineId"), core.GetString(m, "machine_id")),
		DeviceID:     core.FirstNonEmpty(core.GetString(authObj, "device_id"), core.GetString(authObj, "deviceId"), core.GetString(m, "device_id")),
		Label:        core.GetString(m, "label"),
	}

	if c.AccessToken == "" && c.RefreshToken == "" {
		return nil, fmt.Errorf("trae credential missing access_token and refresh_token")
	}
	return c, nil
}

// VendorIDFor 返回某区域对应的 VendorID。
func VendorIDFor(r Region) string {
	if r == RegionGlobal {
		return VendorIDGlobal
	}
	return VendorIDCN
}

// LooksLikeCredential 判断文件或数据是否属于本 Trae 区域实例。
func LooksLikeCredential(raw map[string]any, fileName, provider string, targetRegion Region) bool {
	return LooksLikeCredentialFor(raw, fileName, provider, ConfigFor(targetRegion))
}

// LooksLikeCredentialFor 判断文件或数据是否属于某个 Trae profile。
func LooksLikeCredentialFor(raw map[string]any, fileName, provider string, cfg Config) bool {
	if decided, claimed := core.VendorFieldVerdict(raw, cfg.VendorID, "trae"); decided {
		return claimed
	}
	if core.FileNameMatchesConvention(fileName, cfg.VendorID) {
		return true
	}
	// 旧的家族前缀 trae-*.json：按文件内 region 判定。
	if core.FileNameMatchesConvention(fileName, "trae") {
		rStr := strings.ToLower(core.FirstNonEmpty(core.GetString(raw, "region"), core.GetString(raw, "realm"), core.GetString(raw, "_edition")))
		if cfg.Region == RegionGlobal {
			return rStr == "global" || rStr == "sg"
		}
		return rStr == "cn" || rStr == ""
	}
	return false
}

func (c *Credential) StorageJSON() ([]byte, error) {
	vendor := c.Vendor
	if vendor == "" {
		vendor = VendorIDFor(c.Region)
	}
	m := map[string]any{
		"type":          "freetier",
		"vendor":        vendor,
		"region":        string(c.Region),
		"access_token":  c.AccessToken,
		"refresh_token": c.RefreshToken,
		"expires_at":    c.ExpiresAt,
		"uid":           c.UID,
		"nickname":      c.Nickname,
		"enterprise_id": c.EnterpriseID,
		"machine_id":    c.MachineID,
		"device_id":     c.DeviceID,
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	return json.MarshalIndent(m, "", "  ")
}

// 签到：国内版支持，国际版上游无对应端点；额度两端都有（路径不同）。

// RegionHeader 返回客户端上报的区域码：国内 CN，国际 SG。
//
// 国际版额度端点按区域路由，带 CN 会被网关当成跨区请求拒绝。
func (c *Credential) RegionHeader() string {
	if c != nil && c.Region == RegionGlobal {
		return "SG"
	}
	return "CN"
}

// PackageTypeHeader 返回客户端的 Package-Type：国内谱系 stable_cn，国际谱系 stable。
//
// 取值对齐参考实现的抓包结论（Agent2API usage.rs：intl/solo-intl → stable）。
// 本机同一账号实测：改前额度请求返回 401/1001，改后返回 200 与空额度包。该账号
// 期间也可能被保活刷新过 token，因此这条只作为「端点和头部可用」的证据，
// 不构成因果结论。
func (c *Credential) PackageTypeHeader() string {
	if c != nil && c.Region == RegionGlobal {
		return "stable"
	}
	return "stable_cn"
}
