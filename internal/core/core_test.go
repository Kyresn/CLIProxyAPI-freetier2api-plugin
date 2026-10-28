package core

// 本文件覆盖 core 的共享契约：归属判定、注册表、模型键协议、错误映射、
// 可选能力断言与通用助手。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// fakeVendor 是最小可用的 Vendor 实现（只实现必需能力）。
//
// 内容嗅探按 marker 命中：注册表是全局的，测试之间会累积注册，
// 一个「恒返回 true」的假供应商会污染后续用例。
type fakeVendor struct {
	Meta
	marker string
	models []pluginapi.ModelInfo
}

func (v *fakeVendor) Match(_ string, _ string, raw map[string]any) bool {
	return v.marker != "" && GetString(raw, "test_marker") == v.marker
}

func (v *fakeVendor) Parse(raw []byte, fileName string) (*Credential, error) {
	return &Credential{VendorID: v.ID(), FileID: fileName, Native: string(raw)}, nil
}

func (v *fakeVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return v.models, nil
}

func (v *fakeVendor) Execute(context.Context, *Credential, *ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}

func (v *fakeVendor) ExecuteStream(context.Context, *Credential, *ExecuteRequest, StreamSink) error {
	return nil
}

func newTestRegistry(t *testing.T, vendors ...Vendor) *Registry {
	t.Helper()
	registry := NewRegistry()
	for _, vendor := range vendors {
		if errRegister := registry.Register(vendor); errRegister != nil {
			t.Fatalf("register %s: %v", vendor.ID(), errRegister)
		}
	}
	return registry
}

func TestResolveVendorPrefersVendorField(t *testing.T) {
	registry := newTestRegistry(t,
		&fakeVendor{Meta: Meta{VendorID: "testfield"}},
		&fakeVendor{Meta: Meta{VendorID: "testsniff"}, marker: "sniff"},
	)

	got, ok := registry.Resolve("whatever.json", "", map[string]any{VendorKey: "testfield"})
	if !ok || got.ID() != "testfield" {
		t.Fatalf("vendor field resolution = %v %v, want testfield", got, ok)
	}
}

func TestResolveVendorFallsBackToFileNameThenSniff(t *testing.T) {
	registry := newTestRegistry(t,
		&fakeVendor{Meta: Meta{VendorID: "testfile"}},
		&fakeVendor{Meta: Meta{VendorID: "testsniffonly"}, marker: "sniffonly"},
	)

	got, ok := registry.Resolve("testfile-abc.json", "", map[string]any{})
	if !ok || got.ID() != "testfile" {
		t.Fatalf("file name resolution = %v %v, want testfile", got, ok)
	}

	got, ok = registry.Resolve("acc.json", "", map[string]any{"test_marker": "sniffonly"})
	if !ok || got.ID() != "testsniffonly" {
		t.Fatalf("sniff resolution = %v %v, want testsniffonly", got, ok)
	}
}

func TestResolveVendorRejectsUnknown(t *testing.T) {
	registry := newTestRegistry(t)
	if _, ok := registry.Resolve("zzz.json", "", map[string]any{"foo": "bar"}); ok {
		t.Fatal("unknown credential must not resolve")
	}
}

func TestRegisterVendorRejectsDuplicate(t *testing.T) {
	registry := newTestRegistry(t, &fakeVendor{Meta: Meta{VendorID: "testdup"}})
	if errDuplicate := registry.Register(&fakeVendor{Meta: Meta{VendorID: "testdup"}}); errDuplicate == nil {
		t.Fatal("duplicate vendor registration must fail")
	}
}

func TestFileNameForAndSanitize(t *testing.T) {
	if got := FileNameFor("Vendor", "a/b c"); got != "vendor-a-b-c.json" {
		t.Fatalf("FileNameFor = %q", got)
	}
	if got := SanitizeFileComponent("../etc/passwd"); got == "" || got == "../etc/passwd" {
		t.Fatalf("SanitizeFileComponent must strip unsafe characters, got %q", got)
	}
}

// TestFileNameForFallsBackToStableFingerprint 锁定「取不到 UID 时的文件名兜底」：
// 必须是凭证指纹而不是固定 "account"，否则第二个账号会静默覆盖第一个（#55）。
func TestFileNameForFallsBackToStableFingerprint(t *testing.T) {
	first := FileNameFor("autoclawcn", "", "token-a")
	again := FileNameFor("autoclawcn", "", "token-a")
	other := FileNameFor("autoclawcn", "", "token-b")
	if first != again {
		t.Fatalf("同一凭证两次生成的文件名不同：%q / %q（重登会多出重复账号）", first, again)
	}
	if first == other {
		t.Fatalf("不同凭证拿到同一个文件名：%q（后登的账号会覆盖前一个）", first)
	}
	if strings.Contains(first, "account") {
		t.Fatalf("FileNameFor 退回了固定占位名：%q", first)
	}
	if !strings.HasPrefix(first, "autoclawcn-t") {
		t.Fatalf("指纹文件名缺少 t 前缀（会与真实 uid 撞名）：%q", first)
	}
	// 有标识时种子不参与，行为与加兜底之前一致。
	if got := FileNameFor("autoclawcn", "u1", "token-a"); got != "autoclawcn-u1.json" {
		t.Fatalf("有 UID 时 FileNameFor = %q, want autoclawcn-u1.json", got)
	}
	// 既没有标识也没有种子时只能保证唯一：每个账号落到各自的文件，不共用占位名。
	noneA, noneB := FileNameFor("autoclawcn", ""), FileNameFor("autoclawcn", "")
	if noneA == noneB || strings.Contains(noneA, "account") {
		t.Fatalf("无标识无种子时应退到随机串：%q / %q", noneA, noneB)
	}
}

// TestTokenFingerprint 锁定指纹长度与稳定性（文件名后缀依赖它）。
func TestTokenFingerprint(t *testing.T) {
	if got := TokenFingerprint("   "); got != "" {
		t.Fatalf("空 token 的指纹 = %q, want empty", got)
	}
	first := TokenFingerprint("token-a")
	if len(first) != 12 {
		t.Fatalf("指纹长度 = %d, want 12（%q）", len(first), first)
	}
	if first != TokenFingerprint(" token-a ") {
		t.Fatalf("指纹应忽略首尾空白：%q", first)
	}
	if first == TokenFingerprint("token-b") {
		t.Fatal("不同 token 的指纹相同")
	}
}

// TestFileIDFromFileNameTrimsExtensionCaseInsensitively 锁定扩展名大小写不敏感。
//
// Windows 上 x.JSON 与 x.json 是同一个文件；身份里留着扩展名会让同一账号
// 在去重、签到记录与 uid 归一处被算成两个。
func TestFileIDFromFileNameTrimsExtensionCaseInsensitively(t *testing.T) {
	cases := map[string]string{
		"zcode-AbC.json":     "zcode-AbC",
		"zcode-AbC.JSON":     "zcode-AbC",
		"  zcode-AbC.json  ": "zcode-AbC",
		"zcode-AbC":          "zcode-AbC",
		".json":              ".json",
		"":                   "",
	}
	for fileName, want := range cases {
		if got := FileIDFromFileName(fileName); got != want {
			t.Fatalf("FileIDFromFileName(%q) = %q, want %q", fileName, got, want)
		}
	}
}

func TestSplitModelIDAndModelKey(t *testing.T) {
	registry := newTestRegistry(t, &fakeVendor{Meta: Meta{VendorID: "testmodel"}})
	if prefix, bare := registry.SplitModelID("testmodel:glm-5.2"); prefix != "testmodel" || bare != "glm-5.2" {
		t.Fatalf("SplitModelID = %q %q", prefix, bare)
	}
	if prefix, bare := registry.SplitModelID("gpt-5.5:turbo"); prefix != "" || bare != "gpt-5.5:turbo" {
		t.Fatalf("unknown prefix must not split: %q %q", prefix, bare)
	}
	if got := registry.ModelKeyFor("testmodel", "testmodel:glm-5.2"); got != "testmodel:glm-5.2" {
		t.Fatalf("ModelKeyFor = %q", got)
	}
}

func TestParseCredentialFillsFileIDAndAttributes(t *testing.T) {
	registry := newTestRegistry(t, &noFileIDVendor{Meta: Meta{VendorID: "testnofileid"}})

	credential, ok := registry.ParseCredential([]byte(`{"vendor":"testnofileid"}`), "testnofileid-abc", map[string]string{"realm": "global"})
	if !ok {
		t.Fatal("credential should parse")
	}
	if got := credential.FileIDValue(); got != "testnofileid-abc" {
		t.Fatalf("FileID = %q", got)
	}
	if got := credential.RegionValue(); got != "global" {
		t.Fatalf("region override = %q", got)
	}
}

type noFileIDVendor struct{ Meta }

func (v *noFileIDVendor) Match(_ string, _ string, raw map[string]any) bool {
	return GetString(raw, "vendor") == "testnofileid"
}
func (v *noFileIDVendor) Parse(_ []byte, _ string) (*Credential, error) {
	return &Credential{VendorID: "testnofileid"}, nil
}
func (v *noFileIDVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *noFileIDVendor) Execute(context.Context, *Credential, *ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *noFileIDVendor) ExecuteStream(context.Context, *Credential, *ExecuteRequest, StreamSink) error {
	return nil
}

func TestMaskToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"short", "*****"},
		{"12345678", "********"},
		{"1234567890abcdef", "1234********cdef"},
	}
	for _, tc := range cases {
		if got := MaskToken(tc.in); got != tc.want {
			t.Fatalf("MaskToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJWTHelpers(t *testing.T) {
	// eyJ… payload = {"exp":1791529021,"user_id":"7591340"}
	token := "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3OTE1MjkwMjEsInVzZXJfaWQiOiI3NTkxMzQwIn0.sig"
	if got := JWTExpiry(token); got != 1791529021 {
		t.Fatalf("JWTExpiry = %d", got)
	}
	if got := JWTUserID(token); got != "7591340" {
		t.Fatalf("JWTUserID = %q", got)
	}
	if got := JWTExpiry("not-a-jwt"); got != 0 {
		t.Fatalf("JWTExpiry(garbage) = %d", got)
	}
	if got := JWTUserID("not-a-jwt"); got != "" {
		t.Fatalf("JWTUserID(garbage) = %q", got)
	}
}

func TestJSONHelpers(t *testing.T) {
	payload := map[string]any{
		"a":          "  x  ",
		"b":          "",
		"c":          float64(1700000000000),
		"nested":     map[string]any{"k": "v"},
		"legacy_key": "y",
	}
	if got := GetString(payload, "missing", "b", "a"); got != "x" {
		t.Fatalf("GetString = %q", got)
	}
	if got := GetMap(payload, "nested"); got["k"] != "v" {
		t.Fatalf("GetMap = %v", got)
	}
	if got := GetInt64(payload, "c"); got != 1700000000000 {
		t.Fatalf("GetInt64 = %d", got)
	}
	if got := GetUnixSeconds(payload, "c"); got != 1700000000 {
		t.Fatalf("GetUnixSeconds = %d", got)
	}
	if got := FirstNonEmpty("", "  ", "v"); got != "v" {
		t.Fatalf("FirstNonEmpty = %q", got)
	}
}

func TestVendorFieldVerdict(t *testing.T) {
	if decided, claimed := VendorFieldVerdict(map[string]any{"vendor": "other"}, "mine"); !decided || claimed {
		t.Fatal("foreign vendor must be rejected")
	}
	if decided, claimed := VendorFieldVerdict(map[string]any{"vendor": "mine"}, "mine"); !decided || !claimed {
		t.Fatal("own vendor must be claimed")
	}
	if decided, _ := VendorFieldVerdict(map[string]any{"vendor": "family"}, "mine", "family"); decided {
		t.Fatal("alias must defer to other evidence")
	}
	if decided, _ := VendorFieldVerdict(map[string]any{}, "mine"); decided {
		t.Fatal("missing vendor must defer")
	}
}

// testUpstreamError 是用于错误映射测试的分类错误。
type testUpstreamError struct {
	status    int
	retryable bool
	kind      string
	message   string
}

func (e *testUpstreamError) Error() string        { return e.message }
func (e *testUpstreamError) HTTPStatus() int      { return e.status }
func (e *testUpstreamError) Retryable() bool      { return e.retryable }
func (e *testUpstreamError) UpstreamKind() string { return e.kind }

func TestToPluginError(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantRetry  bool
	}{
		{&testUpstreamError{status: 401, kind: "credential", message: "bad"}, 401, false},
		{&testUpstreamError{status: 429, retryable: true, kind: "rate_limit", message: "slow"}, 429, true},
		{&testUpstreamError{status: 503, retryable: true, kind: "transient", message: "down"}, 503, true},
		{errors.New("plain"), http.StatusBadGateway, true},
	}
	for _, tc := range cases {
		err := ToPluginError(tc.err)
		pluginErr, okPlugin := err.(*PluginError)
		if !okPlugin {
			t.Fatalf("%v -> %T, want *PluginError", tc.err, err)
		}
		if pluginErr.HTTPStatus != tc.wantStatus || pluginErr.Retryable != tc.wantRetry {
			t.Fatalf("%v -> status=%d retry=%v", tc.err, pluginErr.HTTPStatus, pluginErr.Retryable)
		}
	}
	if ToPluginError(nil) != nil {
		t.Fatal("nil must map to nil")
	}
}

// quotaVendor 只实现额度能力，用于验证能力断言。
type quotaVendor struct{ fakeVendor }

func (v *quotaVendor) Quota(context.Context, *Credential) (*pluginapi.QuotaFetchResponse, error) {
	return &pluginapi.QuotaFetchResponse{}, nil
}

func TestCapabilityAssertions(t *testing.T) {
	plain := &fakeVendor{Meta: Meta{VendorID: "plain"}}
	if _, okQuota := As[QuotaProvider](plain); okQuota {
		t.Fatal("plain vendor must not implement quota")
	}
	if SupportsCheckin(plain) {
		t.Fatal("plain vendor must not support checkin")
	}

	withQuota := &quotaVendor{fakeVendor{Meta: Meta{VendorID: "withquota"}}}
	if _, okQuota := As[QuotaProvider](withQuota); !okQuota {
		t.Fatal("quota vendor must implement quota")
	}
}

// accountCheckinVendor 模拟「供应商支持签到，但某类账号不支持」。
type accountCheckinVendor struct {
	fakeVendor
	blocked bool
}

func (v *accountCheckinVendor) SupportsCheckin() bool { return true }

func (v *accountCheckinVendor) Checkin(context.Context, *Credential) (*CheckinResult, error) {
	return &CheckinResult{}, nil
}

func (v *accountCheckinVendor) SupportsCheckinFor(*Credential) bool { return !v.blocked }

func (v *accountCheckinVendor) CheckinUnsupportedReason(*Credential) string {
	if v.blocked {
		return "企业账号不支持签到"
	}
	return ""
}

// TestSupportsCheckinFor 验证「供应商级能力 + 账号级能力」的两级判定。
func TestSupportsCheckinFor(t *testing.T) {
	plain := &fakeVendor{Meta: Meta{VendorID: "plain"}}
	if SupportsCheckinFor(plain, &Credential{}) {
		t.Fatal("没有签到能力的供应商必须判为不支持")
	}

	vendor := &accountCheckinVendor{fakeVendor: fakeVendor{Meta: Meta{VendorID: "acct"}}}
	if !SupportsCheckinFor(vendor, &Credential{}) {
		t.Fatal("未实现分账号判定的供应商应回落到供应商级能力")
	}
	vendor.blocked = true
	if SupportsCheckinFor(vendor, &Credential{}) {
		t.Fatal("账号级判定为假时必须判为不支持")
	}
	if !SupportsCheckin(vendor) {
		t.Fatal("供应商级能力不因账号级判定而改变")
	}
	if reason := CheckinUnsupportedReason(vendor, &Credential{}); reason != "企业账号不支持签到" {
		t.Fatalf("reason = %q", reason)
	}
	if reason := CheckinUnsupportedReason(plain, &Credential{}); reason != "" {
		t.Fatalf("reason for plain vendor = %q, want empty", reason)
	}
}

func TestUnsupportedErrors(t *testing.T) {
	vendor := &fakeVendor{Meta: Meta{VendorID: "x", VendorName: "X"}}
	quotaErr := QuotaUnsupported(vendor)
	if pluginErr, okPlugin := quotaErr.(*PluginError); !okPlugin || pluginErr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("quota unsupported = %v", quotaErr)
	}
	loginErr := LoginUnsupported(vendor)
	if pluginErr, okPlugin := loginErr.(*PluginError); !okPlugin || pluginErr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("login unsupported = %v", loginErr)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("empty payload = %d", got)
	}
	if got := EstimateTokens([]byte("123456")); got != 2 {
		t.Fatalf("estimate = %d", got)
	}
}

func TestModelsForFallbackAndRefreshFallback(t *testing.T) {
	models := []pluginapi.ModelInfo{{ID: "m1"}}
	vendor := &fakeVendor{Meta: Meta{VendorID: "fallback"}, models: models}

	// 未实现 ModelsProvider 时回落 StaticModels。
	got, errModels := ModelsFor(vendor, context.Background(), &Credential{})
	if errModels != nil || len(got) != 1 {
		t.Fatalf("ModelsFor fallback = %v %v", got, errModels)
	}
	// 空凭证不注册模型。
	if gotNil, _ := ModelsFor(vendor, context.Background(), nil); gotNil != nil {
		t.Fatalf("nil credential must yield nil models")
	}
	// 未实现 Refresher 视为已校验。
	cred := &Credential{}
	updated, refreshed, errRefresh := RefreshVendor(vendor, context.Background(), cred)
	if errRefresh != nil || refreshed || updated != cred {
		t.Fatalf("RefreshVendor fallback = %v %v %v", updated, refreshed, errRefresh)
	}
}

func TestFormatRate(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"x0.71 credits", "0.71X"},
		{"x0.05", "0.05X"},
		{"0.08", "0.08X"},
		{"0.08X", "0.08X"},
		{"0.5x", "0.50X"},
		{"1", "1.00X"},
		{"免费", "免费"},
		{"订阅", "订阅"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := FormatRate(tc.input); got != tc.want {
			t.Errorf("FormatRate(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
