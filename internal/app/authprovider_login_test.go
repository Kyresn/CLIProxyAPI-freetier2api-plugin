package app

// 短信验证码登录的凭证文件名回归（#55）：
// 文件名必须以账号身份为准——同一账号重登同名、不同账号不同名。
// 展示名会重复（两个账号同昵称）、也会被 SanitizeFileComponent 洗成空串，
// 单拿它当文件名会让第二个账号静默覆盖第一个。

import (
	"context"
	"encoding/json"
	"testing"

	"freetier2api-plugin/internal/core"
)

// smsLoginStubVendor 复用 writeStubVendor 的 core.Vendor 最小实现，
// 补上短信登录与凭证解析。
type smsLoginStubVendor struct {
	*writeStubVendor
	uid   string
	token string
	label string
}

func (v *smsLoginStubVendor) ID() string { return "smsstub" }

func (v *smsLoginStubVendor) Parse(raw []byte, _ string) (*core.Credential, error) {
	var payload struct {
		UID   string `json:"uid"`
		Token string `json:"accessToken"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &core.Credential{UID: payload.UID, Token: payload.Token}, nil
}

func (v *smsLoginStubVendor) SendLoginCode(context.Context, string) (string, error) {
	return "session-1", nil
}

func (v *smsLoginStubVendor) VerifyLoginCode(context.Context, string, string, string) ([]byte, string, error) {
	storage := map[string]any{"accessToken": v.token}
	if v.uid != "" {
		storage["uid"] = v.uid
	}
	raw, errMarshal := json.Marshal(storage)
	return raw, v.label, errMarshal
}

// TestVerifyLoginSMSNamesFileByCredentialUID 覆盖三条判定：
// 不同 UID → 不同文件名；同一 UID 重登 → 同名；UID 缺失 → 凭证指纹兜底，
// 不再退到固定占位名。
func TestVerifyLoginSMSNamesFileByCredentialUID(t *testing.T) {
	vendor := &smsLoginStubVendor{writeStubVendor: &writeStubVendor{}, label: "同名用户"}
	registry := core.NewRegistry()
	if errRegister := registry.Register(vendor); errRegister != nil {
		t.Fatalf("register stub vendor: %v", errRegister)
	}
	a := &App{registry: registry}
	login := func(uid, token string) string {
		t.Helper()
		vendor.uid, vendor.token = uid, token
		fileName, _, _, errVerify := a.VerifyLoginSMS(context.Background(), vendor.ID(), "session-1", "13800000000", "123456")
		if errVerify != nil {
			t.Fatalf("VerifyLoginSMS(uid=%q token=%q): %v", uid, token, errVerify)
		}
		return fileName
	}

	first := login("u-1", "token-1")
	if first != "smsstub-u-1.json" {
		t.Fatalf("文件名 = %q, want smsstub-u-1.json（UID 必须是主键）", first)
	}
	if second := login("u-2", "token-2"); second == first {
		t.Fatalf("不同账号落到同一文件名 %q（后登的会覆盖前一个）", first)
	}
	if again := login("u-1", "token-3"); again != first {
		t.Fatalf("同一账号重登换了文件名：%q / %q（会多出重复账号）", first, again)
	}

	// UID 缺失且展示名洗成空串：退到 access token 的指纹，仍然一账号一名。
	fingerprintOne := login("", "token-4")
	fingerprintTwo := login("", "token-5")
	if fingerprintOne == fingerprintTwo {
		t.Fatalf("无 UID 时不同凭证落到同一文件名 %q", fingerprintOne)
	}
	if fingerprintOne != login("", "token-4") {
		t.Fatalf("同一凭证重登换了文件名（%q），重登会多出重复账号", fingerprintOne)
	}
	if fingerprintOne == "smsstub-account.json" {
		t.Fatalf("文件名退回固定占位名：%q", fingerprintOne)
	}
}
