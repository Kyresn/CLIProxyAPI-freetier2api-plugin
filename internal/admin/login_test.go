package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// postLoginCallback 调用管理路由并解出响应体。
func postLoginCallback(t *testing.T, service *Service, state, callbackURL string) map[string]any {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/freetier2api-fork/login/callback",
		Body:   []byte(`{"state":"` + state + `","url":"` + callbackURL + `"}`),
	}
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	raw, errHandle := service.Handle(raw)
	if errHandle != nil {
		t.Fatalf("handle login callback: %v", errHandle)
	}
	var envelope struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(envelope.Result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v (body=%s)", errUnmarshal, envelope.Result.Body)
	}
	return payload
}

// TestRegistrationDeclaresLoginCallbackRoute 锁定粘贴回调入口必须出现在路由声明里：
// 宿主按这张表放行 /plugins/<id>/... 子路径，漏登记就等于 404。
func TestRegistrationDeclaresLoginCallbackRoute(t *testing.T) {
	for _, route := range newService(t).Registration().Routes {
		if route.Method == http.MethodPost && route.Path == "/plugins/freetier2api-fork/login/callback" {
			return
		}
	}
	t.Fatalf("login/callback route missing: %#v", newService(t).Registration().Routes)
}

// TestHandleLoginCallbackReturnsVendorResult 验证成功路径：供应商给出的状态与提示
// 原样回给页面；CodeArts 的门户登录地址也必须带回去（页面据此渲染可点链接）。
func TestHandleLoginCallbackReturnsVendorResult(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status:      pluginapi.AuthLoginStatusPending,
		Message:     "等待用户完成华为云登录…",
		RedirectURL: "https://codearts.huaweicloud.com/portal/login?ticket_id=x",
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "codearts_x",
		"http://127.0.0.1/oauth/callback?secret=s")

	if payload["status"] != string(pluginapi.AuthLoginStatusPending) {
		t.Fatalf("status = %v, want pending", payload["status"])
	}
	if payload["ok"] != false {
		t.Fatalf("ok = %v, want false", payload["ok"])
	}
	if payload["redirect_url"] == "" {
		t.Fatal("redirect_url 未回传：CodeArts 票据没换到凭证时用户没有下一步")
	}
}

// TestHandleLoginCallbackSurfacesVendorError 验证失败路径：供应商给出的原因要能直接
// 显示给用户，而不是变成一个技术性响应。
func TestHandleLoginCallbackSurfacesVendorError(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackErr = core.NewPluginError("callback_mismatch",
		"回调链接与当前登录会话不匹配，请重新发起登录后重新授权", http.StatusBadRequest)
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	message, _ := payload["message"].(string)
	if !strings.Contains(message, "回调链接与当前登录会话不匹配") {
		t.Fatalf("message = %q, want 供应商给出的可读原因", message)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("status = %v, want error", payload["status"])
	}
}

// TestHandleLoginCallbackRejectsMissingState 守住入参校验：没有 state 时不能把请求
// 透传给供应商（否则就是拿一条链接去撞所有会话）。
func TestHandleLoginCallbackRejectsMissingState(t *testing.T) {
	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{Status: pluginapi.AuthLoginStatusSuccess}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "", "http://127.0.0.1/oauth/callback?secret=s")
	if payload["ok"] != false {
		t.Fatalf("缺 state 时不应成功: %v", payload)
	}
}

// TestHandleLoginCallbackPersistsCredential 锁定「登录成功 → 凭证真的落盘」。
//
// 粘贴路径的凭证只回到**页面**：正常轮询路径是宿主在 auth.login.poll 之后写
// 文件，粘贴路径没有那个宿主环节。少了这一步，页面显示「登录成功」而 auth
// 目录里什么都没有、账号概览也不会多出新账号。
func TestHandleLoginCallbackPersistsCredential(t *testing.T) {
	var savedName string
	var savedJSON []byte
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthSave {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var request pluginapi.HostAuthSaveRequest
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		savedName, savedJSON = request.Name, request.JSON
		return json.RawMessage("{}"), nil
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })

	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth: pluginapi.AuthData{
			Provider:    "freetier",
			FileName:    "traecn-user-1.json",
			Label:       "Trae-tester",
			StorageJSON: []byte("{\"type\":\"freetier\",\"vendor\":\"traecn\"}"),
		},
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	if payload["ok"] != true {
		t.Fatalf("落盘成功后应回报成功: %v", payload)
	}
	if savedName != "traecn-user-1.json" {
		t.Fatalf("host.auth.save 的文件名 = %q, want traecn-user-1.json", savedName)
	}
	if !strings.Contains(string(savedJSON), "traecn") {
		t.Fatalf("落盘内容缺少归属字段: %s", savedJSON)
	}
	if payload["auth_id"] != "traecn-user-1.json" {
		t.Fatalf("响应未回报 auth_id: %v", payload)
	}
}

// TestHandleLoginCallbackReportsPersistFailure 锁定保存失败要说实话：宿主回调与
// 落盘都失败时，不能页面显示「登录成功」而磁盘上什么都没有。
func TestHandleLoginCallbackReportsPersistFailure(t *testing.T) {
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		return nil, fmt.Errorf("host unavailable")
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })

	// 兜底目录指向不存在的路径，让落盘必然失败（不写进测试包目录）。
	original := authFileDirs
	authFileDirs = []string{filepath.Join(t.TempDir(), "missing")}
	t.Cleanup(func() { authFileDirs = original })

	deps := newStubDeps(t)
	deps.callbackResult = &core.CallbackSubmitResult{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   pluginapi.AuthData{FileName: "traecn-x.json", StorageJSON: []byte("{}")},
	}
	payload := postLoginCallback(t, newServiceWithDeps(t, deps), "traecn_x",
		"http://127.0.0.1:18080/authorize?loginTraceID=t")

	if payload["ok"] != false {
		t.Fatalf("保存失败时不应回报成功: %v", payload)
	}
	message, _ := payload["message"].(string)
	if !strings.Contains(message, "保存凭证失败") {
		t.Fatalf("message = %q, want 说明保存失败", message)
	}
}

// loginRoutePayload 调用一个登录相关管理路由并解出响应体。
func loginRoutePayload(t *testing.T, service *Service, request pluginapi.ManagementRequest) map[string]any {
	t.Helper()
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	raw, errHandle := service.Handle(raw)
	if errHandle != nil {
		t.Fatalf("handle %s: %v", request.Path, errHandle)
	}
	var envelope struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(envelope.Result.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v (body=%s)", errUnmarshal, envelope.Result.Body)
	}
	return payload
}

// TestRegistrationDeclaresLoginStartRoutes 锁定页面内登录的两个入口：宿主按
// 路由声明放行插件子路径，漏登记就是 404（页面只能看到「获取授权链接失败」）。
func TestRegistrationDeclaresLoginStartRoutes(t *testing.T) {
	found := map[string]bool{}
	for _, route := range newService(t).Registration().Routes {
		found[route.Method+" "+route.Path] = true
	}
	for _, want := range []string{
		"POST /plugins/freetier2api-fork/login/start",
		"GET /plugins/freetier2api-fork/login/captcha-config",
	} {
		if !found[want] {
			t.Fatalf("route %q missing: %#v", want, found)
		}
	}
}

func TestHandleLoginStartReturnsAuthorizationURL(t *testing.T) {
	deps := newStubDeps(t)
	deps.loginStart = &pluginapi.AuthLoginStartResponse{
		URL:      "https://accounts.example/oauth",
		State:    "autoclawglobal_oauth_abc",
		Metadata: map[string]any{"callback_hint": "http://localhost:46952/auth/callback-google"},
	}
	payload := loginRoutePayload(t, newServiceWithDeps(t, deps), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/freetier2api-fork/login/start",
		Body:   []byte(`{"vendor_id":"autoclawglobal","metadata":{"oauth_variant":"google","ali_captcha_verify_param":"p"}}`),
	})
	if payload["ok"] != true || payload["url"] != "https://accounts.example/oauth" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["callback_hint"] != "http://localhost:46952/auth/callback-google" {
		t.Fatalf("callback_hint = %#v", payload["callback_hint"])
	}
}

// TestHandleLoginStartSurfacesVendorError 锁定上游错误必须原样回到页面上：
// 滑块过期（630014）时用户需要看到「重拖一次」，而不是一句 HTTP 500。
func TestHandleLoginStartSurfacesVendorError(t *testing.T) {
	deps := newStubDeps(t)
	deps.loginStartErr = core.NewPluginError("login_failed", "风控验证未通过，请重新点击登录再试一次", http.StatusBadRequest)
	payload := loginRoutePayload(t, newServiceWithDeps(t, deps), pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/freetier2api-fork/login/start",
		Body:   []byte(`{"vendor_id":"autoclawglobal"}`),
	})
	if payload["ok"] != false {
		t.Fatalf("ok = %v", payload["ok"])
	}
	message, _ := payload["message"].(string)
	if !strings.Contains(message, "风控验证未通过") {
		t.Fatalf("message = %q", message)
	}
}

func TestHandleLoginCaptchaConfigRequest(t *testing.T) {
	deps := newStubDeps(t)
	deps.captcha = core.CaptchaConfig{Enabled: true, SceneID: "scene-1", Region: "ga", Prefix: "sq51tr"}
	payload := loginRoutePayload(t, newServiceWithDeps(t, deps), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/freetier2api-fork/login/captcha-config",
		Query:  map[string][]string{"vendor": {"autoclawglobal"}},
	})
	if payload["ok"] != true {
		t.Fatalf("payload = %#v", payload)
	}
	captcha, _ := payload["captcha"].(map[string]any)
	if captcha["enabled"] != true || captcha["scene_id"] != "scene-1" {
		t.Fatalf("captcha = %#v", captcha)
	}
}

func TestHandleLoginCaptchaConfigRejectsMissingVendor(t *testing.T) {
	payload := loginRoutePayload(t, newService(t), pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/freetier2api-fork/login/captcha-config",
	})
	if payload["ok"] != false {
		t.Fatalf("payload = %#v", payload)
	}
}

// captchaLoginVendor 在 fakeVendor 之上补「页面内登录」的两项能力。
type captchaLoginVendor struct {
	fakeVendor
}

func (v *captchaLoginVendor) LoginVariants() []core.LoginVariant {
	return []core.LoginVariant{
		{ID: "zai", Label: "Zai", CallbackHint: "http://localhost:53699/auth/callback-zai"},
		{ID: "google", Label: "Google", CallbackHint: "http://localhost:46952/auth/callback-google"},
	}
}
func (v *captchaLoginVendor) LoginCaptchaConfig(context.Context) (core.CaptchaConfig, error) {
	return core.CaptchaConfig{Enabled: true, SceneID: "scene"}, nil
}
func (v *captchaLoginVendor) LoginStart(context.Context, map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return &pluginapi.AuthLoginStartResponse{}, nil
}
func (v *captchaLoginVendor) LoginPoll(context.Context, string) (*pluginapi.AuthLoginPollResponse, error) {
	return &pluginapi.AuthLoginPollResponse{}, nil
}
func (v *captchaLoginVendor) LocalCallbackHint() string {
	return "http://localhost:46952/auth/callback-google"
}
func (v *captchaLoginVendor) SubmitCallback(context.Context, string, string) (*core.CallbackSubmitResult, error) {
	return &core.CallbackSubmitResult{}, nil
}

// TestVendorsPayloadExposesCaptchaLogin 锁定页面渲染所需的两项描述：只有声明
// 了能力的供应商才会出现「先验证码再登录」的面板，而变体与回调地址来自后端
// （页面不硬编码 AutoClaw）。
func TestVendorsPayloadExposesCaptchaLogin(t *testing.T) {
	deps := newStubDeps(t)
	vendor := &captchaLoginVendor{fakeVendor{Meta: core.Meta{
		VendorID: "autoclawglobal", VendorName: "AutoClaw 国际版", VendorRegion: "global",
	}}}
	if errRegister := deps.registry.Register(vendor); errRegister != nil {
		t.Fatal(errRegister)
	}
	payload := newServiceWithDeps(t, deps).buildVendorsPayload()
	vendors, _ := payload["vendors"].([]vendorDescriptor)
	if len(vendors) != 1 {
		t.Fatalf("vendors = %#v", payload["vendors"])
	}
	got := vendors[0]
	if !got.LoginCaptcha || !got.SupportsLogin || !got.PasteCallback {
		t.Fatalf("descriptor = %#v", got)
	}
	if len(got.LoginVariants) != 2 || got.LoginVariants[0].ID != "zai" {
		t.Fatalf("variants = %#v", got.LoginVariants)
	}
}
