package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge/hostbridgetest"
)

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"13812345678":    "13812345678",
		" 138-1234-5678": "13812345678",
		"+8613812345678": "13812345678",
		"8613812345678":  "13812345678",
	}
	for raw, want := range cases {
		got, err := normalizePhone(raw)
		if err != nil || got != want {
			t.Fatalf("normalizePhone(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"12345", "23812345678", "1381234567a", ""} {
		if _, err := normalizePhone(bad); err == nil {
			t.Fatalf("normalizePhone(%q) should fail", bad)
		}
	}
}

func TestNormalizeSMSCode(t *testing.T) {
	if got, err := normalizeSMSCode(" 123456 "); err != nil || got != 123456 {
		t.Fatalf("normalizeSMSCode = %d, %v", got, err)
	}
	// 前导零无所谓：服务端只看数值。
	if got, err := normalizeSMSCode("000000"); err != nil || got != 0 {
		t.Fatalf("normalizeSMSCode(000000) = %d, %v", got, err)
	}
	if _, err := normalizeSMSCode("12345"); err == nil {
		t.Fatal("5-digit code should fail")
	}
	if _, err := normalizeSMSCode("12345a"); err == nil {
		t.Fatal("non-digit code should fail")
	}
}

func TestDescribeSMSLoginError(t *testing.T) {
	// 400001 是参数错误而不是「验证码不正确」——混为一谈会把排查方向带偏。
	if got := describeLoginError(loginFlowSMS, 400001, "请求数据有问题"); !strings.Contains(got, "参数") {
		t.Fatalf("400001 message = %q", got)
	}
	if got := describeLoginError(loginFlowSMS, 630202, ""); !strings.Contains(got, "验证码不正确") {
		t.Fatalf("630202 message = %q", got)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := MaskPhone("13812345678"); got != "138****5678" {
		t.Fatalf("MaskPhone = %q", got)
	}
	if got := MaskPhone("123"); got != "123" {
		t.Fatalf("MaskPhone(short) = %q", got)
	}
}

func TestSMSLoginProviderCapability(t *testing.T) {
	instances := Instances(core.Env{})
	// 登录能力随实例类型分派：cn → smsLoginAdapter，global → oauthLoginAdapter。
	if _, cnOK := core.As[core.SMSLoginProvider](instances[0]); !cnOK {
		t.Fatal("cn instance should implement SMSLoginProvider")
	}
	if _, glSMS := core.As[core.SMSLoginProvider](instances[1]); glSMS {
		t.Fatal("global instance must NOT implement SMSLoginProvider (it has OAuth instead)")
	}
	if _, glLogin := core.As[core.LoginProvider](instances[1]); !glLogin {
		t.Fatal("global instance should implement LoginProvider")
	}
	if _, cnLogin := core.As[core.LoginProvider](instances[0]); cnLogin {
		t.Fatal("cn instance must NOT implement LoginProvider (it has SMS login instead)")
	}
}

func TestParseOAuthCallback(t *testing.T) {
	vendor, code, state, ok := ParseOAuthCallback("http://localhost:46952/auth/callback-google?code=ac1&state=up1")
	if !ok || vendor != oauthVendorGoogle || code != "ac1" || state != "up1" {
		t.Fatalf("google callback = %v/%v/%v/%v", vendor, code, state, ok)
	}
	vendor, code, _, ok = ParseOAuthCallback("http://localhost:18432/auth/callback-zai?code=ac2")
	if !ok || vendor != oauthVendorZai || code != "ac2" {
		t.Fatalf("zai callback = %v/%v/%v", vendor, code, ok)
	}
	if _, _, _, ok := ParseOAuthCallback("http://evil.com/auth/callback-google?code=x"); ok {
		t.Fatal("foreign host should be rejected")
	}
	if _, _, _, ok := ParseOAuthCallback("http://localhost:46952/other?code=x"); ok {
		t.Fatal("wrong path should be rejected")
	}
}

func TestOAuthNavigateURI(t *testing.T) {
	if got := OAuthNavigateURI(oauthVendorGoogle, 0); got != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("OAuthNavigateURI = %q", got)
	}
	if got := OAuthNavigateURI(oauthVendorZai, 18432); got != "http://localhost:18432/auth/callback-zai" {
		t.Fatalf("OAuthNavigateURI zai = %q", got)
	}
}

func TestLoginStartRegionGuards(t *testing.T) {
	instances := Instances(core.Env{})
	cn := instances[0]
	_ = instances[1]
	// 登录能力随实例类型分派：cn 只有短信登录（SMSLoginProvider），
	// global 只有 OAuth（LoginProvider + CallbackSubmitter）。
	if _, okSMS := core.As[core.SMSLoginProvider](cn); !okSMS {
		t.Fatal("cn should implement SMSLoginProvider")
	}
	if _, okLogin := core.As[core.LoginProvider](cn); okLogin {
		t.Fatal("cn must NOT implement LoginProvider")
	}
	// 国际版 OAuth 第②步强制风控验证码（浏览器端 SDK），插件侧无法完成——
	// LoginStart 返回指引文案页，不声明 LoginProvider（控制台回退凭证填写）。
}

// testContext 返回测试用背景 context。
func testContext() context.Context { return context.Background() }

// TestOAuthDefaultCallbackPorts 锁定两个变体的回调端口：Zai 只能用官方客户端
// 登记的四个端口之一，Google 走 loopback 宽松匹配。
func TestOAuthDefaultCallbackPorts(t *testing.T) {
	if got := OAuthNavigateURI(oauthVendorZai, 0); got != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("zai navigate uri = %q", got)
	}
	if got := OAuthNavigateURI(oauthVendorGoogle, 0); got != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("google navigate uri = %q", got)
	}
}

func TestParseOAuthCallbackAcceptsLoopbackAliases(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:46952/auth/callback-google?code=c1&state=s1",
		"http://[::1]:46952/auth/callback-google?code=c1&state=s1",
	} {
		if _, code, _, ok := ParseOAuthCallback(raw); !ok || code != "c1" {
			t.Fatalf("ParseOAuthCallback(%q) = %q, %v", raw, code, ok)
		}
	}
}

func TestOAuthCaptchaConfigParsesEnvelope(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != OAuthCaptchaConfigPath {
			t.Errorf("path = %q, want %q", r.URL.Path, OAuthCaptchaConfigPath)
		}
		if r.Header.Get("X-Auth-Sign") == "" {
			t.Error("captcha-config 请求缺少签名头")
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"enabled":true,"region":"ga",
			"prefix":"sq51tr","scene_id":"18vhnjxl","captcha_supplier":"aliyun"}}`))
	}))
	defer server.Close()

	config, errConfig := OAuthCaptchaConfig(context.Background(), server.URL)
	if errConfig != nil {
		t.Fatalf("OAuthCaptchaConfig: %v", errConfig)
	}
	if !config.Enabled || config.SceneID != "18vhnjxl" || config.Region != "ga" || config.Prefix != "sq51tr" {
		t.Fatalf("config = %#v", config)
	}
}

func TestOAuthCaptchaConfigSurfacesUpstreamCode(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":630014,"msg":"Verification failed","data":null}`))
	}))
	defer server.Close()

	_, errConfig := OAuthCaptchaConfig(context.Background(), server.URL)
	if errConfig == nil || !strings.Contains(errConfig.Error(), "风控验证") {
		t.Fatalf("err = %v, want 风控验证提示", errConfig)
	}
}

func TestLoginStartRequiresCaptcha(t *testing.T) {
	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	// 请求地址不可达也没关系：缺验证码必须在发请求之前就被挡下。
	oauth.baseURL = "http://127.0.0.1:1"
	_, errStart := oauth.LoginStart(testContext(), map[string]any{"oauth_variant": "google"})
	if errStart == nil || !strings.Contains(errStart.Error(), "人机验证") {
		t.Fatalf("err = %v, want 人机验证提示", errStart)
	}
}

func TestLoginStartRequestsOAuthURLAndRegistersSession(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	var gotPath string
	var gotBody map[string]any
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeaders = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"oauth_url":"https://accounts.example/oauth?x=1"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	response, errStart := oauth.LoginStart(testContext(), map[string]any{
		"oauth_variant":            "zai",
		"ali_captcha_verify_param": "captcha-token-1",
	})
	if errStart != nil {
		t.Fatalf("LoginStart: %v", errStart)
	}
	if response.URL != "https://accounts.example/oauth?x=1" {
		t.Fatalf("url = %q", response.URL)
	}
	if !strings.HasPrefix(response.State, oauth.ID()+"_oauth_") {
		t.Fatalf("state = %q", response.State)
	}
	if response.Metadata["callback_hint"] != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("metadata = %#v", response.Metadata)
	}
	if gotPath != OAuthURLPathZai {
		t.Fatalf("path = %q, want %q", gotPath, OAuthURLPathZai)
	}
	if gotBody["ali_captcha_verify_param"] != "captcha-token-1" {
		t.Fatalf("body = %#v", gotBody)
	}
	if gotBody["navigate_uri"] != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("navigate_uri = %#v", gotBody["navigate_uri"])
	}
	if gotBody["source_id"] != protocolName {
		t.Fatalf("source_id = %#v", gotBody["source_id"])
	}
	// 阿里云 WAF 会以 405 HTML 拦掉 Go 默认 UA（实测只换 UA 就恢复）；
	// OAuth 还必须带官方桌面端指纹，否则浏览器里生成的验证码令牌不被接受。
	if ua := gotHeaders.Get("User-Agent"); !strings.HasPrefix(ua, "AutoClaw/") || strings.Contains(ua, "Go-http-client") {
		t.Fatalf("User-Agent = %q, want AutoClaw desktop UA", ua)
	}
	if gotHeaders.Get("X-Client-Type") != "pc" || gotHeaders.Get("X-Harness-Type") != "zcode" {
		t.Fatalf("desktop fingerprint headers missing: %#v", gotHeaders)
	}

	entry, pollState := oauth.Env.Logins.Poll(response.State)
	if pollState != core.PollReady || entry == nil {
		t.Fatalf("session not registered: %v", pollState)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		t.Fatal("session data type mismatch")
	}
	if pending.Vendor != oauthVendorZai || pending.NavigateURI != "http://localhost:53699/auth/callback-zai" {
		t.Fatalf("pending = %#v", pending)
	}
	if pending.DeviceID == "" || pending.DeviceID != gotBody["device_id"] {
		t.Fatalf("device id mismatch: pending=%q body=%#v", pending.DeviceID, gotBody["device_id"])
	}
}

func TestLoginStartRejectsUnknownVariant(t *testing.T) {
	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	_, errStart := oauth.LoginStart(testContext(), map[string]any{
		"oauth_variant": "github", "ali_captcha_verify_param": "x",
	})
	if errStart == nil || !strings.Contains(errStart.Error(), "未知的登录方式") {
		t.Fatalf("err = %v", errStart)
	}
}

func TestOAuthLoginCapabilitiesAndVariants(t *testing.T) {
	instances := Instances(envWithLogins())
	if _, okCaptcha := core.As[core.LoginCaptchaProvider](instances[1]); !okCaptcha {
		t.Fatal("global instance should implement LoginCaptchaProvider")
	}
	if _, okCaptcha := core.As[core.LoginCaptchaProvider](instances[0]); okCaptcha {
		t.Fatal("cn instance must NOT implement LoginCaptchaProvider")
	}
	variants, okVariants := core.As[core.LoginVariantProvider](instances[1])
	if !okVariants {
		t.Fatal("global instance should implement LoginVariantProvider")
	}
	list := variants.LoginVariants()
	if len(list) != 2 || list[0].ID != "zai" || list[1].ID != "google" {
		t.Fatalf("variants = %#v", list)
	}
	if list[1].CallbackHint != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("google hint = %q", list[1].CallbackHint)
	}
}

// TestSubmitCallbackExchangesCodeWithSessionNavigateURI 锁定「粘贴回调 → 换码」
// 这条链：第②跳与第③跳必须用同一个 navigate_uri / device_id，否则 Zai 会以
// 回调地址不匹配或状态失效拒掉。
func TestSubmitCallbackExchangesCodeWithSessionNavigateURI(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != OAuthLoginPathZai {
			t.Errorf("path = %q, want %q", r.URL.Path, OAuthLoginPathZai)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"access_token":"access-1",` +
			`"refresh_token":"refresh-1","user_id":42,"user_name":"tester"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	sessionID := oauth.ID() + "_oauth_test"
	navigateURI := "http://localhost:53699/auth/callback-zai"
	oauth.Env.Logins.Register(oauth.ID(), sessionID, time.Hour, 0, &autoclawPendingLogin{
		Region: RegionGlobal, Vendor: oauthVendorZai, DeviceID: "device-1", NavigateURI: navigateURI,
	})

	result, errSubmit := oauth.SubmitCallback(testContext(), sessionID,
		"http://localhost:53699/auth/callback-zai?code=code-1&state=upstream-state")
	if errSubmit != nil {
		t.Fatalf("SubmitCallback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, message = %q", result.Status, result.Message)
	}
	if result.Auth.FileName != "autoclawglobal-42.json" {
		t.Fatalf("file name = %q", result.Auth.FileName)
	}
	if gotBody["code"] != "code-1" || gotBody["state"] != "upstream-state" {
		t.Fatalf("body = %#v", gotBody)
	}
	if gotBody["navigate_uri"] != navigateURI || gotBody["device_id"] != "device-1" {
		t.Fatalf("navigate/device mismatch: %#v", gotBody)
	}
	var storage map[string]any
	if errUnmarshal := json.Unmarshal(result.Auth.StorageJSON, &storage); errUnmarshal != nil {
		t.Fatalf("decode storage: %v", errUnmarshal)
	}
	if storage["accessToken"] != "access-1" || storage["refreshToken"] != "refresh-1" {
		t.Fatalf("storage = %#v", storage)
	}
	if _, pollState := oauth.Env.Logins.Poll(sessionID); pollState != core.PollMissing {
		t.Fatalf("session should be consumed after success, state = %v", pollState)
	}
}

// TestLoginCredentialUIDChainCoversCamelCaseUserID 锁定 UID 提取链：国内版登录
// 响应只回驼峰 userId、token 又不是 JWT 时，UID 仍要落在凭证上——否则凭证文件名
// 只能靠兜底，第二个账号就会覆盖第一个（#55）。
func TestLoginCredentialUIDChainCoversCamelCaseUserID(t *testing.T) {
	cases := []struct {
		name       string
		data       loginResultData
		wantUID    string
		wantStored string
	}{
		{
			name:       "驼峰 userId 且 token 非 JWT",
			data:       loginResultData{AccessToken: "opaque-token", UserIDCamel: "8a22f603"},
			wantUID:    "8a22f603",
			wantStored: "8a22f603",
		},
		{
			name:       "蛇形 user_id 数字",
			data:       loginResultData{AccessToken: "opaque-token", UserID: float64(42)},
			wantUID:    "42",
			wantStored: "42",
		},
		{
			name:       "驼峰 userId 在蛇形 user_id 为空时补位",
			data:       loginResultData{AccessToken: "opaque-token", UserID: "", UserIDCamel: "u-camel"},
			wantUID:    "u-camel",
			wantStored: "u-camel",
		},
		{
			name:       "user_id 为 0 时不当作 UID",
			data:       loginResultData{AccessToken: "opaque-token", UserID: float64(0), UserIDCamel: "u-camel"},
			wantUID:    "u-camel",
			wantStored: "u-camel",
		},
		{
			name:       "两种写法同时出现时以蛇形为准",
			data:       loginResultData{AccessToken: "opaque-token", UserID: "u-snake", UserIDCamel: "u-camel"},
			wantUID:    "u-snake",
			wantStored: "u-snake",
		},
	}
	for _, testCase := range cases {
		cred, errCred := newLoginCredential(testCase.data, RegionCN, "device-1")
		if errCred != nil {
			t.Fatalf("%s: newLoginCredential: %v", testCase.name, errCred)
		}
		if got := cred.UIDValue(); got != testCase.wantUID {
			t.Fatalf("%s: UID = %q, want %q", testCase.name, got, testCase.wantUID)
		}
		// 落盘 JSON 的 userId 与 UID 同源：宿主重读凭证时身份不会丢。
		storage, errStorage := cred.StorageJSON()
		if errStorage != nil {
			t.Fatalf("%s: StorageJSON: %v", testCase.name, errStorage)
		}
		var payload map[string]any
		if errUnmarshal := json.Unmarshal(storage, &payload); errUnmarshal != nil {
			t.Fatalf("%s: decode storage: %v", testCase.name, errUnmarshal)
		}
		if payload["userId"] != testCase.wantStored {
			t.Fatalf("%s: storage userId = %#v, want %q", testCase.name, payload["userId"], testCase.wantStored)
		}
	}
}

// TestLoginFileNamesAreStableAndUniqueWithoutJWTToken 锁定登录落盘文件名：
// 同一账号重登同名（幂等）、不同账号不同名（不互相覆盖），且都不再是固定占位名。
func TestLoginFileNamesAreStableAndUniqueWithoutJWTToken(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	userIDs := map[string]string{"first": "8a22f603", "second": "9b33a704"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 刻意只回驼峰 userId、token 也不是 JWT：这正是国内版实测的响应形态。
		_, _ = w.Write([]byte(`{"code":0,"msg":"SUCCESS","data":{"access_token":"opaque-` + body.Code +
			`","refresh_token":"refresh-` + body.Code + `","userId":"` + userIDs[body.Code] +
			`","user_name":"同名用户"}}`))
	}))
	defer server.Close()

	instances := Instances(envWithLogins())
	oauth := instances[1].(*oauthLoginAdapter)
	oauth.baseURL = server.URL
	submit := func(sessionSuffix, code string) string {
		t.Helper()
		sessionID := oauth.ID() + "_oauth_" + sessionSuffix
		oauth.Env.Logins.Register(oauth.ID(), sessionID, time.Hour, 0, &autoclawPendingLogin{
			Region: RegionGlobal, Vendor: oauthVendorGoogle, DeviceID: "device-1",
			NavigateURI: "http://localhost:46952/auth/callback-google",
		})
		result, errSubmit := oauth.SubmitCallback(testContext(), sessionID,
			"http://localhost:46952/auth/callback-google?code="+code)
		if errSubmit != nil {
			t.Fatalf("SubmitCallback(%s): %v", code, errSubmit)
		}
		return result.Auth.FileName
	}

	firstLogin := submit("s1", "first")
	secondAccount := submit("s2", "second")
	sameAccountAgain := submit("s3", "first")

	if firstLogin != sameAccountAgain {
		t.Fatalf("同一账号重登换了文件名：%q / %q（会多出重复账号）", firstLogin, sameAccountAgain)
	}
	if firstLogin == secondAccount {
		t.Fatalf("不同账号落到同一文件名：%q（后登的覆盖前一个）", firstLogin)
	}
	if firstLogin != "autoclawglobal-8a22f603.json" {
		t.Fatalf("文件名 = %q, want autoclawglobal-8a22f603.json", firstLogin)
	}
	if secondAccount != "autoclawglobal-9b33a704.json" {
		t.Fatalf("文件名 = %q, want autoclawglobal-9b33a704.json", secondAccount)
	}
}

// ---------------------------------------------------------------------------
// 登录测试辅助：loopback 宿主与会话表
// ---------------------------------------------------------------------------

// 本文件的 HTTP 单测都经 hostbridge.Client 出站，由
// hostbridgetest.UseLoopbackHost 换成直连 httptest 的转发器。

// stubLoginSessions 是 core.LoginSessions 的内存实现（单测用）。
type stubLoginSessions struct {
	sessions map[string]*core.LoginSession
}

func newStubLoginSessions() *stubLoginSessions {
	return &stubLoginSessions{sessions: map[string]*core.LoginSession{}}
}

func (s *stubLoginSessions) Register(vendorID, sessionID string, ttl, interval time.Duration, data any) {
	s.sessions[sessionID] = &core.LoginSession{VendorID: vendorID, Data: data}
}

func (s *stubLoginSessions) Poll(sessionID string) (*core.LoginSession, core.PollState) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, core.PollMissing
	}
	return session, core.PollReady
}

func (s *stubLoginSessions) Owns(sessionID string) (string, bool) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return "", false
	}
	return session.VendorID, true
}

func (s *stubLoginSessions) Delete(sessionID string) { delete(s.sessions, sessionID) }

// envWithLogins 返回带会话表的 Env（登录链路单测用）。
func envWithLogins() core.Env { return core.Env{Logins: newStubLoginSessions()} }

var _ = testing.Short
