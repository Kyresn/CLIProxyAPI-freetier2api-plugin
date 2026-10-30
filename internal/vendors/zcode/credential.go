package zcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
)

// ZCode 凭证的解析、特征识别与脱敏。
//
// 一个 ZCode 账号可能同时持有两种凭证：
//
//   - JWTToken（zcode.z.ai，Bearer）—— Start Plan 对话通道、额度查询与套餐领取；
//   - APIKey（x-api-key）—— 纯 API Key 账号的对话通道。
//
// OAuth 登录一次拿到两者（JWT 直接下发，API Key 由 access_token 兑换）。

// ZCode 的区域与登录 provider。
const (
	// RegionCN 是国内版（智谱开放平台 bigmodel 登录）。
	RegionCN = "cn"
	// RegionGlobal 是国际版（z.ai 登录）。
	RegionGlobal = "global"
	// ProviderBigmodel 是智谱开放平台登录 provider。
	ProviderBigmodel = "bigmodel"
	// ProviderZai 是 z.ai 国际版登录 provider。
	ProviderZai = "zai"
)

// NormalizeRegion 归一化区域标识；未知值返回空串（由调用方回落默认值）。
func NormalizeRegion(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case RegionCN:
		return RegionCN
	case RegionGlobal:
		return RegionGlobal
	}
	return ""
}

// NormalizeProvider 归一化登录 provider；未知值返回空串。
func NormalizeProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ProviderBigmodel:
		return ProviderBigmodel
	case ProviderZai:
		return ProviderZai
	}
	return ""
}

// RegionForProvider 返回 provider 对应的区域。
func RegionForProvider(provider string) string {
	if NormalizeProvider(provider) == ProviderBigmodel {
		return RegionCN
	}
	return RegionGlobal
}

// Credential 是归一化后的 ZCode 账号凭证。
type Credential struct {
	Vendor   string        `json:"vendor,omitempty"`
	APIKey   string        `json:"api_key,omitempty"`
	JWTToken string        `json:"jwt_token,omitempty"`
	Email    string        `json:"email,omitempty"`
	UserID   string        `json:"user_id,omitempty"`
	Profile  DeviceProfile `json:"profile,omitempty"`
	Label    string        `json:"label,omitempty"`
	AuthMode string        `json:"auth_mode,omitempty"` // oauth / apikey
	// Region 是账号所属区域：cn（bigmodel 登录）/ global（z.ai 登录）。
	//
	// 两种登录都产出 zcode.z.ai 的 Coding Plan JWT，对话通道相同；region
	// 只用于账号展示与登录入口区分。
	Region string `json:"region,omitempty"`
	// Provider 是 OAuth 登录 provider：bigmodel（国内）/ zai（国际）。
	Provider string `json:"login_provider,omitempty"`
}

// UsesPlanChannel 报告本凭证是否具备 Start Plan（JWT）通道能力。
func (c *Credential) UsesPlanChannel() bool {
	return c != nil && strings.TrimSpace(c.JWTToken) != ""
}

// OutboundToken 返回对话出站凭证（优先 API Key，回退 JWT）。
func (c *Credential) OutboundToken() string {
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.APIKey) != "" {
		return c.APIKey
	}
	return strings.TrimSpace(c.JWTToken)
}

// AccountKey 返回按账号串行化时使用的稳定标识：user_id 优先，回退 email，
// 再回退出站凭证指纹（手工导入的凭证可能两者都缺）；完全无凭证字段的异常
// 对象返回空串，由调用方按空键兜底。
func (c *Credential) AccountKey() string {
	if c == nil {
		return ""
	}
	if key := strings.TrimSpace(c.UserID); key != "" {
		return "uid:" + key
	}
	if key := strings.TrimSpace(c.Email); key != "" {
		return "email:" + key
	}
	token := c.OutboundToken()
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "token:" + hex.EncodeToString(sum[:8])
}

// ParseCredential 解析凭证 JSON。
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("zcode credential is empty")
	}

	var m map[string]any
	if errUnmarshal := json.Unmarshal(raw, &m); errUnmarshal != nil {
		return nil, fmt.Errorf("decode zcode credential json: %w", errUnmarshal)
	}

	c := &Credential{
		Vendor:   core.FirstNonEmpty(core.GetString(m, "vendor"), VendorID),
		APIKey:   core.FirstNonEmpty(core.GetString(m, "api_key"), core.GetString(m, "apiKey")),
		JWTToken: core.FirstNonEmpty(core.GetString(m, "jwt_token"), core.GetString(m, "jwtToken")),
		Email:    strings.ToLower(core.GetString(m, "email")),
		UserID:   core.GetString(m, "user_id"),
		Label:    core.GetString(m, "label"),
		AuthMode: core.GetString(m, "auth_mode"),
		Region:   NormalizeRegion(core.GetString(m, "region")),
		Provider: NormalizeProvider(core.FirstNonEmpty(core.GetString(m, "login_provider"), core.GetString(m, "provider"))),
	}

	c.Profile = parseProfile(m)
	c.Profile.ensure()

	if c.APIKey == "" && c.JWTToken == "" {
		return nil, fmt.Errorf("zcode credential missing api_key or jwt_token")
	}
	if c.AuthMode == "" {
		c.AuthMode = "apikey"
	}
	return c, nil
}

// parseProfile 读取设备档案（兼容指纹对象嵌在顶层或 profile/fingerprint 键下）。
func parseProfile(m map[string]any) DeviceProfile {
	nested := m
	for _, key := range []string{"profile", "fingerprint", "device"} {
		if sub, ok := m[key].(map[string]any); ok && sub != nil {
			nested = sub
			break
		}
	}
	return DeviceProfile{
		Platform:  core.GetString(nested, "platform"),
		Arch:      core.GetString(nested, "arch"),
		OSVersion: core.FirstNonEmpty(core.GetString(nested, "os_version"), core.GetString(nested, "osVersion")),
		Language:  core.GetString(nested, "language"),
		Timezone:  core.GetString(nested, "timezone"),
		Screen:    core.GetString(nested, "screen"),
		DeviceMID: core.FirstNonEmpty(
			core.GetString(nested, "device_mid"),
			core.GetString(nested, "deviceMid"),
			core.GetString(m, "device_mid"),
		),
	}
}

// StorageJSON 导出规范落盘 JSON。
func (c *Credential) StorageJSON() ([]byte, error) {
	m := map[string]any{
		"type":      "freetier",
		"vendor":    core.FirstNonEmpty(c.Vendor, VendorID),
		"auth_mode": core.FirstNonEmpty(c.AuthMode, "apikey"),
	}
	if c.APIKey != "" {
		m["api_key"] = c.APIKey
	}
	if c.JWTToken != "" {
		m["jwt_token"] = c.JWTToken
	}
	if c.Email != "" {
		m["email"] = c.Email
	}
	if c.UserID != "" {
		m["user_id"] = c.UserID
	}
	if c.Label != "" {
		m["label"] = c.Label
	}
	if c.Region != "" {
		m["region"] = c.Region
	}
	if c.Provider != "" {
		m["login_provider"] = c.Provider
	}
	// 档案恒定落盘：device_mid 变了上游即视为新设备，必须持久化。
	m["profile"] = c.Profile
	return json.MarshalIndent(m, "", "  ")
}

// MergeStorageJSON 把刷新后的凭证合并回原文件。
//
// 只更新会变化的字段，保留用户在 auth 文件里手工维护的其它键。
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	// type / vendor 无条件写：归属判定依赖这两个键，即使用户从别处拷来的
	// 旧凭证缺了它们，合并结果也必须补上。
	setFields := func(m map[string]any) {
		m["type"] = "freetier"
		m["vendor"] = core.FirstNonEmpty(updated.Vendor, VendorID)
		if updated.APIKey != "" {
			m["api_key"] = updated.APIKey
		}
		if updated.JWTToken != "" {
			m["jwt_token"] = updated.JWTToken
		}
		if updated.Email != "" {
			m["email"] = updated.Email
		}
		if updated.UserID != "" {
			m["user_id"] = updated.UserID
		}
		if updated.AuthMode != "" {
			m["auth_mode"] = updated.AuthMode
		}
		if updated.Label != "" {
			m["label"] = updated.Label
		}
		if updated.Region != "" {
			m["region"] = updated.Region
		}
		if updated.Provider != "" {
			m["login_provider"] = updated.Provider
		}
		if updated.Profile.DeviceMID != "" {
			m["profile"] = updated.Profile
		}
	}
	// fresh 与 apply 共用 setFields：原文件缺失或损坏时，产出的对象与
	// 「合并成功」时字段完全一致，type/vendor 一定在场。
	return core.MergeCredentialJSON(original, func() ([]byte, error) {
		fresh := map[string]any{}
		setFields(fresh)
		return json.MarshalIndent(fresh, "", "  ")
	}, setFields)
}

// VendorIDForRegion 返回区域对应的实例标识。
func VendorIDForRegion(region string) string {
	if NormalizeRegion(region) == RegionCN {
		return VendorIDCN
	}
	return VendorIDGlobal
}

// LooksLikeCredential 判断文件或数据是否属于本实例。
//
// 先做家族级证据判定（vendor / 文件名 / 凭证特征），再按 region 决定归属：
// 显式 vendor=zcodecn|zcodeglobal 直接比对；旧 vendor=zcode 或文件名
// zcode-* 按 region 归属，缺 region 默认国际版。
//
// **必须偏严**：宿主会把所有非内建格式的凭证依次喂给插件，误吞别家的会让
// 宿主用本插件的结构覆盖对方账号。因此只在有明确归属证据时才认领。
func LooksLikeCredential(raw map[string]any, fileName, provider, region string) bool {
	_ = provider
	declared := strings.ToLower(strings.TrimSpace(core.GetString(raw, "vendor")))
	if declared != "" {
		switch declared {
		case VendorID, VendorIDCN, VendorIDGlobal:
		default:
			return false
		}
	}

	baseName := strings.ToLower(strings.TrimSpace(fileName))
	hasFamilyEvidence := declared != "" || core.FileNameMatchesConvention(fileName, VendorID, VendorIDCN, VendorIDGlobal)
	if !hasFamilyEvidence {
		// 带 ZCode 专有痕迹：档案键或 auth_mode 标记。
		if _, ok := raw["profile"]; ok {
			if core.GetString(raw, "device_mid") != "" || core.GetString(raw, "jwt_token") != "" {
				hasFamilyEvidence = true
			}
		}
		// 同时带 jwt_token 与 api_key（本插件 ZCode 的双凭证形态）。
		if !hasFamilyEvidence && core.GetString(raw, "jwt_token") != "" && core.GetString(raw, "api_key") != "" {
			hasFamilyEvidence = true
		}
		// 智谱/Z.AI API Key 形态（xxx.xxx）+ 文件名含 zcode/zai
		key := core.FirstNonEmpty(core.GetString(raw, "api_key"), core.GetString(raw, "apiKey"))
		if !hasFamilyEvidence && key != "" && strings.Count(key, ".") == 1 &&
			(strings.Contains(baseName, "zcode") || strings.Contains(baseName, "zai")) {
			hasFamilyEvidence = true
		}
	}
	if !hasFamilyEvidence {
		return false
	}

	if declared == VendorIDCN || declared == VendorIDGlobal {
		return declared == VendorIDForRegion(region)
	}

	credRegion := NormalizeRegion(core.GetString(raw, "region"))
	if credRegion == "" {
		switch {
		case core.FileNameMatchesConvention(fileName, VendorIDCN):
			credRegion = RegionCN
		case core.FileNameMatchesConvention(fileName, VendorIDGlobal):
			credRegion = RegionGlobal
		default:
			// 旧凭证（vendor=zcode / 无标记）默认国际版，保持原有行为。
			credRegion = RegionGlobal
		}
	}
	return credRegion == NormalizeRegion(region)
}

// fileNameFor 生成 ZCode 凭证文件名（沿用插件的 <vendor>-<标识>.json 约定）。
func fileNameFor(vendorID, identifier string) string {
	return core.FileNameFor(core.FirstNonEmpty(vendorID, VendorID), identifier)
}

// UserIDFromJWT 从 JWT payload 解 user_id（user_id 优先，sub 兜底）。
//
// user_id 是官方客户端激活上报的用户标识，每次从 token 实时解出而不是缓存：
// token 刷新后 user_id 会自动跟随。
func UserIDFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	claims := decodeJWTPayload(parts[1])
	for _, key := range []string{"user_id", "sub"} {
		if v, ok := claims[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// EmailFromJWT 从 JWT payload 解邮箱（OAuth 用户资料缺失时的备援）。
func EmailFromJWT(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	claims := decodeJWTPayload(parts[1])
	for _, key := range []string{"email", "user_email", "mail"} {
		if v, ok := claims[key].(string); ok && strings.Contains(v, "@") {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

// decodeJWTPayload 解 base64url 编码的 payload；失败返回空 map。
func decodeJWTPayload(segment string) map[string]any {
	decoded, errDecode := base64URLDecode(segment)
	if errDecode != nil {
		return nil
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(decoded, &claims); errUnmarshal != nil {
		return nil
	}
	return claims
}

// 计费族（billing/*、usage）的公共请求层。
//
// 这些端点都在 zcode.z.ai 的 zcode-plan 之下，共用同一套身份头，且都**必须**
// 带 X-Device-Mid——缺失时上游返回 code=3001。鉴权用 Coding Plan JWT
// （API Key 通道没有套餐概念，不参与计费族）。
//
// event/report 不在此列：它不在 zcode-plan 之下、也不需要鉴权，见 activation.go。
//
// 参考 zcode2api/app/quota.py 的 _auth_headers 与 app/claim.py 的 _billing_request。

const (
	// billingHTTPTimeout 是计费族单次请求的上限。
	billingHTTPTimeout = 20 * time.Second
	// billingMaxBody 是计费响应的读取上限。
	billingMaxBody = 4 << 20
)

// 额度查询：billing/balance（持有套餐 + 余量）。
//
// 只对 Coding Plan（JWT）账号有意义——API Key 是用户自带的按量付费 Key，
// 没有套餐窗口。参考 zcode2api/app/quota.py。

// 每账号客户端设备档案（一号一台设备）与上游身份头构造。
//
// 为什么需要它：billing 族（balance/preview/claim）**必需** X-Device-Mid，
// 缺失时上游返回 code=3001；同时上游按设备聚类做风控，多个账号共用同一套
// 平台/内核/分辨率会聚成「一台机器开了 N 个账号」的关联信号。
//
// 因此档案从**成套 SKU** 抽样（platform × arch × os_version × screen 绑定），
// 禁止字段笛卡尔积——「darwin-arm64 + 1366x768」这类组合不是真实电脑。
// 池内不含 linux：官方桌面端主形态是 Mac / Windows，服务器内核特征反而扎眼。
//
// 参考 zcode2api/app/fingerprint.py 与 app/identity.py。

// DeviceProfile 是单个账号的设备档案，字段直接映射上游身份头与事件字段。
type DeviceProfile struct {
	Platform  string `json:"platform"`   // darwin / win32
	Arch      string `json:"arch"`       // arm64 / x64
	OSVersion string `json:"os_version"` // X-Os-Version（os.release() 语义）
	Language  string `json:"language"`   // X-Client-Language
	Timezone  string `json:"timezone"`   // X-Client-Timezone（IANA）
	Screen    string `json:"screen"`     // 激活事件 screen_resolution
	DeviceMID string `json:"device_mid"` // X-Device-Mid，一号一台
}

// PlatformFull 返回 X-Platform 取值形态（darwin-arm64）。
func (p DeviceProfile) PlatformFull() string {
	if p.Platform == "" {
		return DefaultPlatform
	}
	return p.Platform + "-" + p.Arch
}

// OSCategory 返回 X-Os-Category（darwin→macos / win32→windows）。
func (p DeviceProfile) OSCategory() string {
	switch p.Platform {
	case "darwin", "macos":
		return "macos"
	case "win32", "windows":
		return "windows"
	default:
		return "linux"
	}
}

// sku 是一套完整可用的桌面设备组合。
type sku struct {
	weight    int
	platform  string
	arch      string
	osVersion string
	screen    string
}

// desktopSKUs 是从官方桌面端常见形态归纳的成套组合。
//
// 权重即抽样概率：Apple silicon 与 Win11 主流机型占多数，老旧机型少量。
var desktopSKUs = []sku{
	// Apple silicon MacBook Air/Pro 13–14"（darwin 24 = Sequoia，25 = Tahoe）
	{10, "darwin", "arm64", "24.5.0", "1512x982"},
	{10, "darwin", "arm64", "24.6.0", "1512x982"},
	{8, "darwin", "arm64", "24.5.0", "1728x1117"},
	{8, "darwin", "arm64", "24.6.0", "1728x1117"},
	{8, "darwin", "arm64", "25.5.0", "1512x982"},
	{6, "darwin", "arm64", "25.5.0", "1728x1117"},
	{5, "darwin", "arm64", "23.6.0", "1512x982"},
	{4, "darwin", "arm64", "24.5.0", "2560x1440"},
	{3, "darwin", "arm64", "24.6.0", "2560x1600"},
	// Intel Mac 存量（darwin 24+ 不再配 x64）
	{2, "darwin", "x64", "23.6.0", "1920x1080"},
	{2, "darwin", "x64", "22.6.0", "1440x900"},
	// Windows 11 主流 + 少量 Win10
	{8, "win32", "x64", "10.0.22631", "1920x1080"},
	{7, "win32", "x64", "10.0.26100", "1920x1080"},
	{5, "win32", "x64", "10.0.22631", "2560x1440"},
	{4, "win32", "x64", "10.0.26200", "1920x1080"},
	{3, "win32", "x64", "10.0.26100", "2560x1440"},
	{3, "win32", "x64", "10.0.22621", "1920x1080"},
	{2, "win32", "x64", "10.0.22631", "3840x2160"},
	{2, "win32", "x64", "10.0.19045", "1920x1080"},
	{1, "win32", "x64", "10.0.22000", "1920x1080"},
}

// locales 是真实地区对（语言 ↔ 时区同源，激活事件共用）。
var locales = []struct{ language, timezone string }{
	{"zh-CN", "Asia/Shanghai"},
	{"en-US", "America/New_York"},
	{"en-US", "America/Los_Angeles"},
	{"en-GB", "Europe/London"},
	{"de-DE", "Europe/Berlin"},
	{"ja-JP", "Asia/Tokyo"},
	{"ko-KR", "Asia/Seoul"},
	{"en-SG", "Asia/Singapore"},
}

// NewDeviceProfile 抽样生成一份成套桌面档案（登录入池时调用一次并持久化）。
func NewDeviceProfile() DeviceProfile {
	total := 0
	for _, item := range desktopSKUs {
		total += item.weight
	}
	picked := desktopSKUs[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(total))); err == nil {
		remaining := n.Int64()
		for _, item := range desktopSKUs {
			remaining -= int64(item.weight)
			if remaining < 0 {
				picked = item
				break
			}
		}
	}
	locale := locales[0]
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(len(locales)))); err == nil {
		locale = locales[n.Int64()]
	}
	return DeviceProfile{
		Platform:  picked.platform,
		Arch:      picked.arch,
		OSVersion: picked.osVersion,
		Language:  locale.language,
		Timezone:  locale.timezone,
		Screen:    picked.screen,
		DeviceMID: NewUUID(),
	}
}

// ensureProfile 就地补齐档案中缺失的字段（旧凭证升级用）。
//
// 整台设备都缺失时补一套完整 SKU；否则只在**同一平台内**补空字段——
// 混搭平台会产生自相矛盾的档案（如 win32 + macOS 内核号），比字段缺失更糟。
func (p *DeviceProfile) ensure() {
	if p.Platform == "" && p.Arch == "" && p.OSVersion == "" && p.Screen == "" {
		fresh := NewDeviceProfile()
		fresh.DeviceMID = core.FirstNonEmpty(p.DeviceMID, fresh.DeviceMID)
		*p = fresh
		return
	}
	if p.Platform == "" {
		p.Platform = "darwin"
	}
	if p.Arch == "" {
		if p.Platform == "win32" {
			p.Arch = "x64"
		} else {
			p.Arch = "arm64"
		}
	}
	if p.OSVersion == "" || !osVersionMatchesPlatform(p.Platform, p.OSVersion) {
		p.OSVersion = defaultOSVersionFor(p.Platform)
	}
	if p.Screen == "" {
		p.Screen = "1920x1080"
	}
	if p.Language == "" {
		p.Language = DefaultLanguage
	}
	if p.Timezone == "" {
		p.Timezone = DefaultTimezone
	}
	if p.DeviceMID == "" {
		p.DeviceMID = NewUUID()
	}
}

// osVersionMatchesPlatform 报告内核版本串是否与平台相符。
//
// 上游按平台解析该字段的形态，混搭（win32 + 25.5.0）是明显的伪造信号。
func osVersionMatchesPlatform(platform, osVersion string) bool {
	switch platform {
	case "win32", "windows":
		return strings.HasPrefix(osVersion, "10.")
	case "darwin", "macos":
		// darwin 内核版本为两位主版本号（22–25）。
		return !strings.HasPrefix(osVersion, "10.")
	default:
		return true
	}
}

// defaultOSVersionFor 返回某平台的默认内核版本。
func defaultOSVersionFor(platform string) string {
	switch platform {
	case "win32", "windows":
		return "10.0.22631"
	default:
		return DefaultOSVersion
	}
}

// IdentityHeaders 是上游要求的 X- 身份头集合（不含鉴权头）。
type IdentityHeaders struct {
	values map[string]string
}

// Set 写入一个身份头。
func (h *IdentityHeaders) Set(key, value string) {
	if h.values == nil {
		h.values = map[string]string{}
	}
	h.values[key] = value
}

// Apply 把身份头写入目标 map（键名保持上游要求的原样大小写）。
func (h *IdentityHeaders) Apply(target map[string]string) {
	for key, value := range h.values {
		target[key] = value
	}
}

// upstreamAuthHeaders 是上游通用的身份与鉴权头。
//
// JWT 优先（Coding Plan 通道），没有 JWT 时回退 API Key：两条链路的网关都认
// 这两种凭证，只是 billing 族仅对 JWT 有意义。请求级追踪头每请求重新生成。
func upstreamAuthHeaders(cred *Credential) map[string]string {
	headers := map[string]string{}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	if strings.TrimSpace(cred.JWTToken) != "" {
		headers["Authorization"] = "Bearer " + cred.JWTToken
	} else if strings.TrimSpace(cred.APIKey) != "" {
		headers["x-api-key"] = cred.APIKey
	}
	headers["x-request-id"] = NewUUID()
	return headers
}

// BuildIdentityHeaders 构造完整身份头（对齐官方桌面端 companion 头集合）。
//
// 有账号时取该账号的设备档案；profile 为零值时回退全局伪装常量。
func BuildIdentityHeaders(profile DeviceProfile) *IdentityHeaders {
	profile.ensure()
	headers := &IdentityHeaders{}
	headers.Set("HTTP-Referer", HTTPReferer)
	headers.Set("User-Agent", UserAgent)
	headers.Set("X-ZCode-App-Version", ClientAppVersion)
	headers.Set("X-Title", ClientTitle)
	headers.Set("X-ZCode-Agent", ClientAgent)
	headers.Set("X-Platform", profile.PlatformFull())
	headers.Set("X-Release-Channel", ReleaseChannel)
	headers.Set("X-Client-Language", profile.Language)
	headers.Set("X-Client-Timezone", profile.Timezone)
	headers.Set("X-Os-Category", profile.OSCategory())
	headers.Set("X-Os-Version", profile.OSVersion)
	headers.Set("X-Device-Mid", profile.DeviceMID)
	return headers
}

// BuildTraceHeaders 构造追踪头。
//
// JWT/Plan 通道（= 上游的 start-plan）**只允许**这三个头：官方客户端不带
// x-query-id / x-session-id，误发会触发上游 3012「unusual activity」风控。
// 每请求重新生成。
func BuildTraceHeaders() map[string]string {
	return map[string]string{
		"x-request-id":         NewUUID(),
		"x-zcode-session-type": "main",
		"x-zcode-trace-id":     NewUUID(),
	}
}

// NewUUID 生成 RFC 4122 v4 UUID。
func NewUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// NewHexID 生成 n 字节的随机十六进制串。
func NewHexID(n int) string { return core.RandomHex(n) }
