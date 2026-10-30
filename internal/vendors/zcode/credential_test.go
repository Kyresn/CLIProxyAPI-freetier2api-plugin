package zcode

import (
	"encoding/json"
	"testing"
)

func TestLooksLikeCredential(t *testing.T) {
	cases := []struct {
		name     string
		raw      map[string]any
		fileName string
		provider string
		want     bool
	}{
		{
			name:     "explicit vendor",
			raw:      map[string]any{"vendor": "zcode", "api_key": "abc.123"},
			fileName: "any.json",
			want:     true,
		},
		{
			name:     "filename prefix",
			raw:      map[string]any{"api_key": "abc.123"},
			fileName: "zcode-acc1.json",
			want:     true,
		},
		{
			name:     "foreign vendor",
			raw:      map[string]any{"vendor": "qodercn"},
			fileName: "zcode-test.json",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LooksLikeCredential(tc.raw, tc.fileName, tc.provider, RegionGlobal)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestMaskedKey(t *testing.T) {
	if got := MaskedKey("1234567890abcdef"); got != "1234***cdef" {
		t.Fatalf("unexpected masked key: %s", got)
	}
}

func TestNewDeviceProfileIsCoherent(t *testing.T) {
	// 档案必须是「成套」组合，不能是字段笛卡尔积拼出的假电脑。
	validOS := map[string]map[string]bool{
		"darwin": {"22.6.0": true, "23.6.0": true, "24.5.0": true, "24.6.0": true, "25.5.0": true},
		"win32":  {"10.0.19045": true, "10.0.22000": true, "10.0.22621": true, "10.0.22631": true, "10.0.26100": true, "10.0.26200": true},
	}
	validLocale := map[string]bool{
		"zh-CN|Asia/Shanghai": true, "en-US|America/New_York": true, "en-US|America/Los_Angeles": true,
		"en-GB|Europe/London": true, "de-DE|Europe/Berlin": true, "ja-JP|Asia/Tokyo": true,
		"ko-KR|Asia/Seoul": true, "en-SG|Asia/Singapore": true,
	}

	for i := 0; i < 200; i++ {
		profile := NewDeviceProfile()
		if validOS[profile.Platform] == nil {
			t.Fatalf("平台不应为 %q（官方桌面端主形态是 darwin/win32）", profile.Platform)
		}
		if !validOS[profile.Platform][profile.OSVersion] {
			t.Fatalf("os_version %q 与平台 %q 不匹配", profile.OSVersion, profile.Platform)
		}
		if !validLocale[profile.Language+"|"+profile.Timezone] {
			t.Fatalf("语言/时区组合不真实: %s/%s", profile.Language, profile.Timezone)
		}
		if len(profile.DeviceMID) != 36 {
			t.Fatalf("device_mid 应为 UUID: %q", profile.DeviceMID)
		}
		if profile.Platform == "win32" && profile.Arch != "x64" {
			t.Fatalf("win32 不应配 %q", profile.Arch)
		}
	}
}

func TestDeviceProfileIdentityHeaders(t *testing.T) {
	profile := DeviceProfile{
		Platform: "win32", Arch: "x64", OSVersion: "10.0.22631",
		Language: "zh-CN", Timezone: "Asia/Shanghai",
		Screen: "1920x1080", DeviceMID: "11111111-2222-4333-8444-555555555555",
	}
	headers := map[string]string{}
	BuildIdentityHeaders(profile).Apply(headers)

	checks := map[string]string{
		"X-Platform":          "win32-x64",
		"X-Os-Category":       "windows",
		"X-Os-Version":        "10.0.22631",
		"X-Client-Language":   "zh-CN",
		"X-Client-Timezone":   "Asia/Shanghai",
		"X-Device-Mid":        "11111111-2222-4333-8444-555555555555",
		"X-ZCode-App-Version": ClientAppVersion,
		"X-Title":             ClientTitle,
	}
	for key, want := range checks {
		if headers[key] != want {
			t.Fatalf("%s = %q, want %q", key, headers[key], want)
		}
	}
}

func TestTraceHeadersOmitsForbiddenKeys(t *testing.T) {
	// Plan 通道（start-plan）只允许三个 trace 头；带上 x-query-id / x-session-id
	// 会触发上游 3012 风控。
	headers := BuildTraceHeaders()
	for _, forbidden := range []string{"x-query-id", "x-session-id"} {
		if _, exists := headers[forbidden]; exists {
			t.Fatalf("Plan 通道不得发送 %s（会触发上游 3012 风控）", forbidden)
		}
	}
	for _, required := range []string{"x-request-id", "x-zcode-session-type", "x-zcode-trace-id"} {
		if headers[required] == "" {
			t.Fatalf("缺少必需追踪头 %s", required)
		}
	}
	if headers["x-zcode-session-type"] != "main" {
		t.Fatalf("x-zcode-session-type = %q", headers["x-zcode-session-type"])
	}
}

func TestParseCredentialRoundTrip(t *testing.T) {
	original := &Credential{
		APIKey:   "abc12345.secret99",
		JWTToken: "header.payload.signature",
		Email:    "User@Example.com",
		UserID:   "u-1",
		AuthMode: "oauth",
		Label:    "me",
		Profile:  NewDeviceProfile(),
	}
	raw, errMarshal := original.StorageJSON()
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}

	parsed, errParse := ParseCredential(raw)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if parsed.APIKey != original.APIKey || parsed.JWTToken != original.JWTToken {
		t.Fatalf("凭证字段未往返: %+v", parsed)
	}
	if parsed.Email != "user@example.com" {
		t.Fatalf("邮箱应归一为小写: %q", parsed.Email)
	}
	// device_mid 必须持久化：变了上游即视为新设备。
	if parsed.Profile.DeviceMID != original.Profile.DeviceMID {
		t.Fatalf("device_mid 未往返: %q != %q", parsed.Profile.DeviceMID, original.Profile.DeviceMID)
	}
	if parsed.Profile.Platform != original.Profile.Platform {
		t.Fatalf("档案平台未往返: %q", parsed.Profile.Platform)
	}
}

func TestParseCredentialRequiresSomeToken(t *testing.T) {
	if _, errParse := ParseCredential([]byte(`{"vendor":"zcode"}`)); errParse == nil {
		t.Fatal("没有任何凭证时必须报错")
	}
}

func TestOutboundTokenPrefersAPIKey(t *testing.T) {
	// OutboundToken 用于账号标签脱敏：有 API Key 时优先展示 API Key，否则回退 JWT。
	cred := &Credential{APIKey: "key.s", JWTToken: "a.b.c"}
	if got := cred.OutboundToken(); got != "key.s" {
		t.Fatalf("OutboundToken = %q, want the API key", got)
	}
	// 只有 JWT 时回退 JWT。
	if got := (&Credential{JWTToken: "a.b.c"}).OutboundToken(); got != "a.b.c" {
		t.Fatalf("OutboundToken = %q, want the JWT", got)
	}
}

func TestLooksLikeCredentialRejectsForeign(t *testing.T) {
	// 必须偏严：误吞别家凭证会让宿主用本插件的结构覆盖对方账号。
	cases := []struct {
		raw      map[string]any
		fileName string
		want     bool
	}{
		{map[string]any{"vendor": "qodercn", "api_key": "a.b"}, "zcode-x.json", false},
		{map[string]any{"vendor": "zcode"}, "any.json", true},
		{map[string]any{"api_key": "a.b"}, "zcode-x.json", true},
		// 裸凭证无任何 ZCode 痕迹：不认领。
		{map[string]any{"api_key": "sk-abcdef"}, "random.json", false},
	}
	for _, tc := range cases {
		if got := LooksLikeCredential(tc.raw, tc.fileName, "", RegionGlobal); got != tc.want {
			t.Fatalf("LooksLikeCredential(%v, %s) = %v, want %v", tc.raw, tc.fileName, got, tc.want)
		}
	}
}

func TestUserIDFromJWT(t *testing.T) {
	// {"user_id":"u-42"} 的 base64url（无填充）。
	token := "header." + "eyJ1c2VyX2lkIjoidS00MiJ9" + ".signature"
	if got := UserIDFromJWT(token); got != "u-42" {
		t.Fatalf("UserIDFromJWT = %q, want u-42", got)
	}
	if got := UserIDFromJWT("not-a-jwt"); got != "" {
		t.Fatalf("非法 JWT 应返回空串，得到 %q", got)
	}
}

func TestMergeStorageJSONPreservesUserKeys(t *testing.T) {
	original := []byte(`{"type":"freetier","vendor":"zcode","api_key":"old","disabled":true,"note":"mine"}`)
	updated := &Credential{APIKey: "new", JWTToken: "a.b.c", AuthMode: "oauth"}
	merged, errMerge := MergeStorageJSON(original, updated)
	if errMerge != nil {
		t.Fatalf("merge: %v", errMerge)
	}

	var out map[string]any
	if errUnmarshal := json.Unmarshal(merged, &out); errUnmarshal != nil {
		t.Fatalf("decode merged: %v", errUnmarshal)
	}
	if out["api_key"] != "new" || out["jwt_token"] != "a.b.c" {
		t.Fatalf("凭证未更新: %v", out)
	}
	// 用户手工维护的字段必须保留，否则刷新一次就把配置抹了。
	if out["disabled"] != true || out["note"] != "mine" {
		t.Fatalf("用户字段丢失: %v", out)
	}
}

// TestAccountKeyPrioritizesIdentity 验证闸门键优先用 user_id，回退 email，
// 两者都缺时用出站凭证指纹，避免互不相关的账号共用同一把闸门。
func TestAccountKeyPrioritizesIdentity(t *testing.T) {
	cases := []struct {
		cred Credential
		want string
	}{
		{Credential{UserID: "u1", Email: "a@b.c", APIKey: "k1"}, "uid:u1"},
		{Credential{Email: "a@b.c", APIKey: "k1"}, "email:a@b.c"},
	}
	for _, tc := range cases {
		if got := tc.cred.AccountKey(); got != tc.want {
			t.Fatalf("AccountKey() = %q, want %q", got, tc.want)
		}
	}
	first := (&Credential{APIKey: "k1"}).AccountKey()
	second := (&Credential{APIKey: "k2"}).AccountKey()
	if first == "" || first == second {
		t.Fatalf("token fingerprint must be non-empty and distinct: %q vs %q", first, second)
	}
	if (&Credential{}).AccountKey() != "" {
		t.Fatal("credential without any identity must yield empty key")
	}
}

// TestRegionProviderNormalization 锁住国内外登录标识的归一化。
func TestRegionProviderNormalization(t *testing.T) {
	cases := []struct {
		raw      string
		region   string
		provider string
	}{
		{"cn", RegionCN, ""},
		{"global", RegionGlobal, ""},
		{"bigmodel", "", ProviderBigmodel},
		{"zai", "", ProviderZai},
		{"", "", ""},
		{"unknown", "", ""},
	}
	for _, testCase := range cases {
		if got := NormalizeRegion(testCase.raw); got != testCase.region {
			t.Errorf("NormalizeRegion(%q) = %q, want %q", testCase.raw, got, testCase.region)
		}
		if got := NormalizeProvider(testCase.raw); got != testCase.provider {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", testCase.raw, got, testCase.provider)
		}
	}
	if RegionForProvider(ProviderBigmodel) != RegionCN || RegionForProvider(ProviderZai) != RegionGlobal {
		t.Fatal("RegionForProvider 映射错误")
	}
}

// TestCredentialRoundTripKeepsRegionProvider 锁住 region/provider 落盘。
func TestCredentialRoundTripKeepsRegionProvider(t *testing.T) {
	cred := &Credential{
		Vendor:   VendorID,
		JWTToken: "jwt-token",
		AuthMode: "oauth",
		Region:   RegionCN,
		Provider: ProviderBigmodel,
		Profile:  NewDeviceProfile(),
	}
	storage, errJSON := cred.StorageJSON()
	if errJSON != nil {
		t.Fatalf("StorageJSON: %v", errJSON)
	}
	parsed, errParse := ParseCredential(storage)
	if errParse != nil {
		t.Fatalf("ParseCredential: %v", errParse)
	}
	if parsed.Region != RegionCN || parsed.Provider != ProviderBigmodel {
		t.Fatalf("region/provider = %q/%q, want cn/bigmodel", parsed.Region, parsed.Provider)
	}
}
