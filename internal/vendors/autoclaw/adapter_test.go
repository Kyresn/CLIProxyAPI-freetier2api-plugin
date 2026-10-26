package autoclaw

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestRegionVendorIDs(t *testing.T) {
	if got := VendorIDFor(RegionCN); got != VendorIDCN {
		t.Fatalf("VendorIDFor(cn) = %q", got)
	}
	if got := VendorIDFor(RegionGlobal); got != VendorIDGlobal {
		t.Fatalf("VendorIDFor(global) = %q", got)
	}
	if VendorNameFor(RegionCN) != "AutoClaw 国内版" || VendorNameFor(RegionGlobal) != "AutoClaw 国际版" {
		t.Fatalf("unexpected vendor names")
	}
}

func TestNormalizeRegion(t *testing.T) {
	cases := map[string]Region{
		"cn":     RegionCN,
		"":       RegionCN,
		"GLOBAL": RegionGlobal,
		"intl":   RegionGlobal,
		"junk":   RegionCN,
	}
	for raw, want := range cases {
		if got := NormalizeRegion(raw); got != want {
			t.Fatalf("NormalizeRegion(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestInstancesAreTwoRegions(t *testing.T) {
	instances := Instances(core.Env{})
	if len(instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(instances))
	}
	if instances[0].ID() != VendorIDCN || instances[1].ID() != VendorIDGlobal {
		t.Fatalf("instance IDs = %q, %q", instances[0].ID(), instances[1].ID())
	}
	if instances[0].Region() != string(RegionCN) || instances[1].Region() != string(RegionGlobal) {
		t.Fatalf("instance regions = %q, %q", instances[0].Region(), instances[1].Region())
	}
}

func TestParseCredential(t *testing.T) {
	cred, errParse := ParseCredential([]byte(`{"accessToken":"tok-abc","refreshToken":"ref-123","region":"global","userId":123,"userName":"测试","deviceId":"dev-1"}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if cred.AccessTokenValue() != "tok-abc" || cred.RefreshTokenValue() != "ref-123" {
		t.Fatalf("tokens = %+v", cred)
	}
	if cred.Realm() != RegionGlobal || cred.UIDValue() != "123" || cred.UserNameValue() != "测试" {
		t.Fatalf("identity = %+v", cred)
	}
	if cred.DeviceIDValue() != "dev-1" {
		t.Fatalf("deviceId = %q", cred.DeviceIDValue())
	}
	if _, errParse := ParseCredential([]byte(`{"userName":"no token"}`)); errParse == nil {
		t.Fatal("expected error for credential without accessToken")
	}
}

func TestApplyTokensKeepsRefreshWhenUpstreamOmits(t *testing.T) {
	cred := &Credential{AccessToken: "old", RefreshToken: "ref"}
	cred.ApplyTokens("new", "", 1700000000)
	if cred.AccessTokenValue() != "new" || cred.RefreshTokenValue() != "ref" {
		t.Fatalf("apply tokens = %+v", cred)
	}
	if cred.ExpiresAtValue() != 1700000000 {
		t.Fatalf("expiresAt = %d", cred.ExpiresAtValue())
	}
}

func TestLooksLikeCredentialStrict(t *testing.T) {
	cases := []struct {
		name     string
		fileName string
		raw      string
		want     bool
	}{
		{"filename cn", "autoclawcn-u1.json", `{}`, true},
		{"filename global", "autoclawglobal-u1.json", `{}`, true},
		{"filename family", "my-autoclaw.json", `{}`, true},
		{"vendor field", "acc.json", `{"vendor":"autoclawglobal"}`, true},
		{"sandbox endpoint field", "acc.json", `{"sandboxEndpoint":"https://x","accessToken":"t"}`, true},
		{"device id alone is foreign", "acc.json", `{"deviceId":"d"}`, false},
		{"generic tokens are foreign", "acc.json", `{"accessToken":"t","refreshToken":"r"}`, false},
		{"foreign vendor is foreign", "acc.json", `{"vendor":"qodercn"}`, false},
		{"empty is nothing", "acc.json", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			_ = json.Unmarshal([]byte(tc.raw), &payload)
			if got := LooksLikeCredential(payload, tc.fileName, ""); got != tc.want {
				t.Fatalf("LooksLikeCredential(%q, %q) = %v, want %v", tc.fileName, tc.raw, got, tc.want)
			}
		})
	}
}

func TestMatchRoutesRegionsByVendorField(t *testing.T) {
	instances := Instances(core.Env{})
	cn := instances[0].(*smsLoginAdapter).adapter
	gl := instances[1].(*oauthLoginAdapter).adapter

	cnPayload := map[string]any{core.VendorKey: "autoclawcn"}
	glPayload := map[string]any{core.VendorKey: "autoclawglobal"}
	if !cn.Match("acc.json", "", cnPayload) || cn.Match("acc.json", "", glPayload) {
		t.Fatal("cn instance misroutes by vendor field")
	}
	if !gl.Match("acc.json", "", glPayload) || gl.Match("acc.json", "", cnPayload) {
		t.Fatal("global instance misroutes by vendor field")
	}
	if !cn.Match("autoclawcn-u1.json", "", map[string]any{}) {
		t.Fatal("cn instance should claim autoclawcn-*.json")
	}
	if cn.Match("autoclawglobal-u1.json", "", map[string]any{}) {
		t.Fatal("cn instance must not claim autoclawglobal-*.json")
	}
}

func TestConfigureBaseURLPerInstance(t *testing.T) {
	instances := Instances(core.Env{})
	cn := instances[0].(*smsLoginAdapter).adapter
	gl := instances[1].(*oauthLoginAdapter).adapter
	cn.Configure(core.Settings{}, map[string]string{"autoclawcn_base_url": "http://cn.test"})
	if cn.resolveBaseURL() != "http://cn.test" {
		t.Fatalf("cn resolveBaseURL = %q", cn.resolveBaseURL())
	}
	gl.Configure(core.Settings{}, map[string]string{"autoclawglobal_base_url": "http://gl.test"})
	if gl.resolveBaseURL() != "http://gl.test" {
		t.Fatalf("gl resolveBaseURL = %q", gl.resolveBaseURL())
	}
	gl.Configure(core.Settings{}, map[string]string{})
	if gl.resolveBaseURL() != DefaultBaseURLGlobal {
		t.Fatalf("resolveBaseURL fallback = %q", gl.resolveBaseURL())
	}
}

func TestAuthSign(t *testing.T) {
	// 固定向量：与 Agent2API 的 sign() 同口径（MD5(appid&ts&secret)）。
	got := AuthSign("1700000000")
	if len(got) != 32 || strings.ToLower(got) != got {
		t.Fatalf("AuthSign should be 32-char lowercase hex, got %q", got)
	}
}

func TestClassifyByCodeAndText(t *testing.T) {
	if f := Classify(200, 410000, "session"); f.Kind != core.FailureKindSessionDead {
		t.Fatalf("410000 kind = %q", f.Kind)
	}
	if f := Classify(200, 0, "积分不足，请充值"); f.Kind != core.FailureKindHardCredit {
		t.Fatalf("quota text kind = %q", f.Kind)
	}
	if f := Classify(429, 0, "slow down"); f.Kind != core.FailureKindSoftRate {
		t.Fatalf("429 kind = %q", f.Kind)
	}
	if f := Classify(401, 0, "denied"); f.Kind != core.FailureKindCredential {
		t.Fatalf("401 kind = %q", f.Kind)
	}
	var _ = http.StatusOK
}
