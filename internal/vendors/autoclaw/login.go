// 本文件实现 AutoClaw 的全量登录能力：国内短信验证码、国际版 OAuth 网页登录，
// 以及按区域实例分派。
//
// 登录渠道必须集中在唯一的 login.go 中维护，禁止拆成 login_*.go：控制台、
// 能力探测与测试都按「一家供应商一个登录入口」理解。

package autoclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// ---------------------------------------------------------------------------
// 区域分派
// ---------------------------------------------------------------------------

// 供应商实例 = 协议 × 区域，登录方式随地区不同（core 的接口隔离纪律：
// 只实现自己真实具备的能力，控制台据此渲染不同的添加表单）：
//   - smsLoginAdapter（国内版）：core.SMSLoginProvider（手机号 + 验证码）；
//   - oauthLoginAdapter（国际版）：core.LoginProvider + core.CallbackSubmitter
//     + core.LoginVariantProvider + core.LoginCaptchaProvider
//     （Zai / Google OAuth 网页登录：页面内人机验证 + 回调链接粘贴）。
// 共享的 adapter 自身不实现任何登录接口——把 SMS 方法挂在共享类型上会让
// 国际版在控制台也出现手机号表单（虽然运行时会被拒绝，但 UI 撒谎了）。

// SendLoginCode 发送短信验证码（国内版专用，core.SMSLoginProvider）。
func (sms *smsLoginAdapter) SendLoginCode(ctx context.Context, phone string) (string, error) {
	if sms.region.IsGlobal() {
		return "", core.NewPluginError("login_unsupported",
			"AutoClaw 国际版不支持手机号登录，请改用「网页登录（Google）」或「填写凭证」", http.StatusBadRequest)
	}
	return SendLoginCode(ctx, sms.resolveBaseURL(), phone)
}

// VerifyLoginCode 用验证码完成登录（国内版专用）。
func (sms *smsLoginAdapter) VerifyLoginCode(ctx context.Context, sessionKey, phone, code string) ([]byte, string, error) {
	if sms.region.IsGlobal() {
		return nil, "", core.NewPluginError("login_unsupported",
			"AutoClaw 国际版不支持手机号登录，请改用「网页登录（Google）」或「填写凭证」", http.StatusBadRequest)
	}
	cred, errLogin := VerifyLoginCode(ctx, sms.resolveBaseURL(), sessionKey, phone, code)
	if errLogin != nil {
		return nil, "", core.ToPluginError(errLogin)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, "", core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	label := core.FirstNonEmpty(cred.UserNameValue(), MaskPhone(cred.Phone), cred.UIDValue())
	return storageJSON, label, nil
}

// autoclawPendingLogin 是一次进行中的国际版 OAuth 登录会话。
//
// LoginStart 把 deviceID 与变体放进会话表；SubmitCallback 取出后用回调里
// 的 code/state 换票（state 用上游带回的值，官方客户端就是原样传回）。
type autoclawPendingLogin struct {
	Region      Region
	Vendor      oauthVendor
	DeviceID    string
	NavigateURI string
}

// LoginStart 用控制台完成人机验证后拿到的令牌换 OAuth 授权地址。
//
// 调用链（与 Agent2API providers/autoclaw/oauth.rs 同口径）：
//
//	控制台加载阿里云 SDK → 用户拖滑块 → POST {oauth-url}
//	  {source_id, device_id, navigate_uri, ali_captcha_verify_param}
//	→ data.oauth_url（就是用户要打开的那个登录页）
//
// 第 ② 步的验证码是**浏览器端**生成的，因此这里不接受空令牌：少了它上游
// 只会回一条误导性的 631002（「当前版本已停止服务」），不如在入口处说清楚。
// 会话登记进公共会话表，SubmitCallback 靠 state 找回这次的 device_id /
// navigate_uri / 变体去换码。
func (oauth *oauthLoginAdapter) LoginStart(ctx context.Context, meta map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	variant, errVariant := oauthVariantFromMetadata(meta)
	if errVariant != nil {
		return nil, errVariant
	}
	captcha := core.GetString(meta, "ali_captcha_verify_param")
	if captcha == "" {
		return nil, core.NewPluginError("captcha_required",
			"请先在控制台完成人机验证（阿里云滑块），再获取授权链接", http.StatusBadRequest)
	}
	if oauth.Env.Logins == nil {
		return nil, core.NewPluginError("login_unavailable",
			"登录会话表不可用，无法发起登录", http.StatusInternalServerError)
	}
	deviceID := core.RandomHex(32)
	navigateURI := OAuthNavigateURI(variant, 0)
	authURL, errURL := OAuthRequestURL(ctx, oauth.resolveBaseURL(), variant, navigateURI, captcha, deviceID)
	if errURL != nil {
		return nil, core.ToPluginError(errURL)
	}
	state := oauth.ID() + "_oauth_" + core.RandomHex(16)
	oauth.Env.Logins.Register(oauth.ID(), state, autoclawOAuthTTL, autoclawOAuthPollInterval,
		&autoclawPendingLogin{
			Region:      oauth.region,
			Vendor:      variant,
			DeviceID:    deviceID,
			NavigateURI: navigateURI,
		})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       authURL,
		State:     state,
		ExpiresAt: time.Now().Add(autoclawOAuthTTL),
		Metadata: map[string]any{
			core.VendorKey:  oauth.ID(),
			"region":        string(oauth.region),
			"oauth_variant": string(variant),
			"callback_hint": navigateURI,
		},
	}, nil
}

// LoginVariants 把 Zai / Google 两个入口交给控制台（页面不硬编码供应商细节）。
func (oauth *oauthLoginAdapter) LoginVariants() []core.LoginVariant { return OAuthVariants() }

// LoginCaptchaConfig 返回阿里云控件的初始化参数（scene_id / region / prefix）。
func (oauth *oauthLoginAdapter) LoginCaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	return OAuthCaptchaConfig(ctx, oauth.resolveBaseURL())
}

// LoginPoll 轮询一次国际版 OAuth 登录状态（等待用户粘贴回调链接）。
func (oauth *oauthLoginAdapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	entry, pollState := oauth.Env.Logins.Poll(state)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	label := oauthVendorGoogle.label()
	hint := OAuthNavigateURI(oauthVendorGoogle, 0)
	if pending, okPending := entry.Data.(*autoclawPendingLogin); okPending {
		label = pending.Vendor.label()
		hint = pending.NavigateURI
	}
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusPending,
		Message: "等待用户在授权页完成 " + label + " 登录…登录完成后浏览器会停在 " + hint +
			"，请把地址栏里的整条链接复制回控制台提交",
	}, nil
}

// LocalCallbackHint 返回控制台要展示的本机回调地址。
//
// 两个变体的端口不同（见 OAuthNavigateURI），页面拿 LoginVariants 里的
// CallbackHint 逐变体展示；这里给 Google 的地址作为兜底。
func (oauth *oauthLoginAdapter) LocalCallbackHint() string {
	return OAuthNavigateURI(oauthVendorGoogle, 0)
}

// SubmitCallback 解析用户粘贴的回调链接并完成国际版 OAuth 换票。
func (oauth *oauthLoginAdapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	sessionID := strings.TrimSpace(state)
	entry, pollState := oauth.Env.Logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return nil, core.NewPluginError("session_not_found",
			"登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
	}
	pending, okPending := entry.Data.(*autoclawPendingLogin)
	if !okPending {
		return nil, core.NewPluginError("login_session_broken", "登录会话数据已损坏，请重新发起登录", http.StatusBadRequest)
	}
	vendor, code, upstreamState, okCode := ParseOAuthCallback(rawURL)
	if !okCode {
		return nil, core.NewPluginError("callback_parse_failed",
			"回调链接解析失败，请确认粘贴的是登录完成后浏览器停在的整条链接（含 code=…）",
			http.StatusBadRequest)
	}
	if vendor != pending.Vendor {
		return nil, core.NewPluginError("callback_mismatch",
			"回调链接与本次登录的登录方式不一致，请重新发起登录", http.StatusBadRequest)
	}
	cred, errExchange := OAuthExchangeCode(ctx, oauth.resolveBaseURL(), vendor, code, upstreamState, pending.NavigateURI, pending.DeviceID)
	if errExchange != nil {
		return nil, core.ToPluginError(errExchange)
	}
	storageJSON, errStorage := cred.StorageJSON()
	if errStorage != nil {
		return nil, core.NewPluginError("login_failed", "构建凭证失败："+errStorage.Error(), http.StatusInternalServerError)
	}
	oauth.Env.Logins.Delete(sessionID)
	label := core.FirstNonEmpty(cred.UserNameValue(), cred.UIDValue())
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "AutoClaw 登录成功",
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(oauth.ID(), cred.UIDValue(), cred.AccessTokenValue()),
			Label:       label,
			StorageJSON: storageJSON,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// 国内版：手机号 + 短信验证码
// ---------------------------------------------------------------------------

// 协议依据 Agent2API providers/autoclaw/login.rs（桌面端 app.asar 逆向 + 实测）：
//
//	POST {userapi}/userapi/v1/agent-send-code  {"phone","source_id","device_id"}
//	  → code=0 且 data.result=true 才算发出；
//	POST {userapi}/userapi/v1/agent-login/     {"phone","code","platform":"web",
//	                                            "source_id","device_id"}
//	  → data {access_token, refresh_token, user_id, user_name, ...}
//
// 实测口径（别改回去）：
//   - body 的 code 必须是 JSON 数字，字符串会得到与验证码错误无关的
//     400001 参数错误；
//   - agent-login 路径结尾的斜杠是上游要求的（服务端 307 到无斜杠）；
//   - 两次调用必须用同一个 device_id（上游把验证码绑在设备上）；
//   - 国际版没有这条链路（手机验证码入口已被官方移除），在入口处拒绝。

// 短信登录链路的端点（挂在 userapi 域下）。
const (
	SendCodePath = "/userapi/v1/agent-send-code"
	// LoginPath 结尾的斜杠是上游要求的。
	LoginPath = "/userapi/v1/agent-login/"
)

// 登录请求超时：验证码接口偶发慢，给到 20 秒；OAuth 链路 15 秒。
const (
	smsLoginTimeout   = 20 * time.Second
	oauthLoginTimeout = 15 * time.Second
)

// loginFlow 区分同一条业务码在短信 / OAuth 链路下的不同处置建议。
type loginFlow uint8

const (
	loginFlowSMS loginFlow = iota
	loginFlowOAuth
)

// describeLoginError 把上游业务码翻译成用户能看懂的原因。
//
// 400001 是参数错误而不是「验证码不正确」——把两者混为一谈会把排查方向带偏
// （code 传字符串就会稳定得这个码）。631002 / 630014 在两条链路上的下一步
// 不同：短信只能改用填写凭证，OAuth 可以重新点登录再走一次人机验证。
func describeLoginError(flow loginFlow, code int64, upstreamMsg string) string {
	switch code {
	case 400002:
		return "请求签名校验失败（本机时钟可能有偏差），请校准系统时间后重试"
	case 631002:
		if flow == loginFlowOAuth {
			return "本次登录缺少风控验证（验证码未通过或已过期），请重新点击登录再试一次"
		}
		return "上游要求过风控验证，本次登录无法在此完成；请改用「填写凭证」（可从 AutoClaw 客户端登录态文件里取）"
	case 630014:
		if flow == loginFlowOAuth {
			return "风控验证未通过，请重新点击登录再试一次"
		}
		return "风控验证未通过，请稍后重试；若持续失败请改用「填写凭证」"
	}
	if flow == loginFlowOAuth {
		if code == 631001 {
			return "授权码无效或已过期，请重新点击登录"
		}
	} else {
		switch code {
		case 400001:
			return "请求参数有误，请检查手机号与验证码格式后重试"
		case 630202:
			return "验证码不正确，请检查后重试"
		case 630101:
			return "获取验证码过于频繁，请稍后再试"
		case 410000, 400000:
			return "登录态已失效，请重新登录"
		}
	}
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		return "登录失败：" + msg
	}
	return fmt.Sprintf("登录失败（上游 code=%s）", strconv.FormatInt(code, 10))
}

// normalizePhone 归一化中国大陆手机号：容忍 +86 / 86 前缀与空格、连字符，
// 要求 1[2-9] 开头的 11 位。
func normalizePhone(raw string) (string, error) {
	var builder strings.Builder
	for _, ch := range raw {
		if ch != ' ' && ch != '-' {
			builder.WriteRune(ch)
		}
	}
	trimmed := builder.String()
	trimmed = strings.TrimPrefix(trimmed, "+86")
	if rest, okRest := strings.CutPrefix(trimmed, "86"); okRest && len(rest) == 11 {
		trimmed = rest
	}
	if len(trimmed) == 11 && strings.HasPrefix(trimmed, "1") && trimmed[1] >= '2' && trimmed[1] <= '9' {
		valid := true
		for _, ch := range trimmed {
			if ch < '0' || ch > '9' {
				valid = false
				break
			}
		}
		if valid {
			return trimmed, nil
		}
	}
	return "", core.NewPluginError("invalid_request", "请填写 11 位中国大陆手机号", http.StatusBadRequest)
}

// normalizeSMSCode 校验 6 位数字验证码并转成数字。
//
// 上游要求 body 里的 code 是 JSON 数字（传字符串得到 400001 参数错误），
// 前导零无所谓——服务端只看数值。
func normalizeSMSCode(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) != 6 {
		return 0, core.NewPluginError("invalid_request", "请填写 6 位数字验证码", http.StatusBadRequest)
	}
	value, errParse := strconv.ParseInt(trimmed, 10, 64)
	if errParse != nil {
		return 0, core.NewPluginError("invalid_request", "请填写 6 位数字验证码", http.StatusBadRequest)
	}
	return value, nil
}

// postLoginJSON 发一次匿名登录 POST（登录时还没有 token）。
//
// SMS 与 OAuth 只差请求头与超时；marshal、发送、读响应、错误分类完全一致，
// 集中在这里，避免两条链路各维护一份会漂移的实现。
func postLoginJSON(ctx context.Context, baseURL, path string, body any, what string, timeout time.Duration, applyHeaders func(*http.Request, string)) (json.RawMessage, error) {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal %s request: %w", what, errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", what, errReq)
	}
	applyHeaders(req, "")

	resp, errDo := hostbridge.Client(ctx, timeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: what + ": " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read " + what + " response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	return json.RawMessage(raw), nil
}

// loginEnvelope 拆业务信封 {code, msg, data}；data 缺失时返回 null。
func loginEnvelope(raw json.RawMessage) (code int64, msg string, data json.RawMessage) {
	var envelope struct {
		Code *int64          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return 0, "", json.RawMessage("null")
	}
	if envelope.Code != nil {
		code = *envelope.Code
	}
	if len(envelope.Data) == 0 {
		envelope.Data = json.RawMessage("null")
	}
	return code, envelope.Msg, envelope.Data
}

// SendLoginCode 发送短信验证码（国内版专用）。
//
// device_id 由本函数生成并随 sessionKey 返回——上游把验证码绑在设备上，
// VerifyLoginCode 必须沿用同一个值。
func SendLoginCode(ctx context.Context, baseURL, phone string) (sessionKey string, err error) {
	phone, errPhone := normalizePhone(phone)
	if errPhone != nil {
		return "", errPhone
	}
	deviceID := core.RandomHex(32)
	raw, errPost := postLoginJSON(ctx, baseURL, SendCodePath, map[string]any{
		"phone":     phone,
		"source_id": protocolName,
		"device_id": deviceID,
	}, "发送验证码", smsLoginTimeout, ApplyUserAPIHeaders)
	if errPost != nil {
		return "", core.ToPluginError(errPost)
	}
	code, msg, payload := loginEnvelope(raw)
	if code != 0 {
		return "", core.NewPluginError("login_failed", describeLoginError(loginFlowSMS, code, msg), http.StatusBadRequest)
	}
	// 源实现还要求 data.result 为真（code=0 但 result 假也算失败）。
	var data struct {
		Result bool `json:"result"`
	}
	_ = json.Unmarshal(payload, &data)
	if !data.Result {
		return "", core.NewPluginError("login_failed", "验证码发送失败，请稍后重试", http.StatusBadGateway)
	}
	return deviceID, nil
}

// VerifyLoginCode 用手机号 + 验证码完成登录，返回落盘凭证。
//
// sessionKey 是 SendLoginCode 返回的 device_id；没带上时现生成一个
// （上游接受「新设备直接登录」的形态）。
func VerifyLoginCode(ctx context.Context, baseURL, sessionKey, phone, code string) (*Credential, error) {
	phone, errPhone := normalizePhone(phone)
	if errPhone != nil {
		return nil, errPhone
	}
	codeNum, errCode := normalizeSMSCode(code)
	if errCode != nil {
		return nil, errCode
	}
	deviceID := strings.TrimSpace(sessionKey)
	if deviceID == "" {
		deviceID = core.RandomHex(32)
	}
	raw, errPost := postLoginJSON(ctx, baseURL, LoginPath, map[string]any{
		"phone":     phone,
		"code":      codeNum,
		"platform":  "web",
		"source_id": protocolName,
		"device_id": deviceID,
	}, "登录", smsLoginTimeout, ApplyUserAPIHeaders)
	if errPost != nil {
		return nil, core.ToPluginError(errPost)
	}
	envCode, msg, payload := loginEnvelope(raw)
	if envCode != 0 {
		return nil, core.NewPluginError("login_failed", describeLoginError(loginFlowSMS, envCode, msg), http.StatusBadRequest)
	}
	var data loginResultData
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return nil, &core.Failure{Msg: "parse login response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	cred, errCred := newLoginCredential(data, RegionCN, deviceID)
	if errCred != nil {
		return nil, errCred
	}
	cred.Phone = phone
	return cred, nil
}

// loginResultData 是短信 / OAuth 两条链路共有的登录响应字段。
//
// 收成具名类型是因为两条链路原先各抄了一份匿名结构，加字段必然漏一处；
// user_id 同时收蛇形与驼峰：国内版实测回的是 userId，只读 user_id 会让 UID
// 恒为空，账号文件名随之撞车（#55）。
type loginResultData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       any    `json:"user_id"`
	UserIDCamel  any    `json:"userId"`
	UserName     string `json:"user_name"`
}

// newLoginCredential 把两条登录链路共有的响应字段归一成凭证。
//
// 上游登录响应没有 expires_in 时按 28 天保守记录；UID 取响应里的
// user_id / userId，缺失再从 access token 的 JWT payload 里读——国内版 token
// 不是 JWT，响应字段才是主路径。
func newLoginCredential(data loginResultData, region Region, deviceID string) (*Credential, error) {
	access := strings.TrimSpace(data.AccessToken)
	if access == "" {
		return nil, core.NewPluginError("login_failed", "登录成功但上游没有返回 access_token", http.StatusBadGateway)
	}
	cred := &Credential{
		AccessToken:  access,
		RefreshToken: strings.TrimSpace(data.RefreshToken),
		UserName:     strings.TrimSpace(data.UserName),
		DeviceID:     deviceID,
		Region:       region,
		ExpiresAt:    core.JWTExpiry(access),
	}
	if cred.ExpiresAt == 0 {
		cred.ExpiresAt = time.Now().Add(28 * 24 * time.Hour).Unix()
	}
	cred.UID = core.FirstNonEmpty(uidFromAny(data.UserID, data.UserIDCamel), core.JWTUserID(access))
	return cred, nil
}

// uidFromAny 把上游的 user_id / userId（数字或字符串）归一成第一个非空字符串。
func uidFromAny(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case float64:
			// 0 是「字段存在但没填」的形态，不能当成 UID——否则所有这种账号
			// 会一起落到 <vendor>-0.json 上。
			if typed > 0 {
				return strconv.FormatInt(int64(typed), 10)
			}
		}
	}
	return ""
}

// MaskPhone 手机号脱敏（保留前 3 后 4）。
func MaskPhone(phone string) string {
	chars := []rune(strings.TrimSpace(phone))
	if len(chars) < 7 {
		return phone
	}
	return string(chars[:3]) + "****" + string(chars[len(chars)-4:])
}

// ---------------------------------------------------------------------------
// 国际版：Zai / Google OAuth 网页登录
// ---------------------------------------------------------------------------

// 国际版协议（Agent2API providers/autoclaw/oauth.rs，桌面端逆向 + 实测）：
//
//	① POST {intl}/userapi/overseasv1/oauth-captcha-config {}
//	     → data.enabled 是「这个构建要不要走 OAuth」的开关（国内 false）；
//	② POST {intl}/userapi/overseasv1/{zai|google}-oauth-url
//	     {source_id, device_id, navigate_uri, ali_captcha_verify_param}
//	     → data.oauth_url；
//	③ 浏览器登录 → 302 到 {navigate_uri}?code=…&state=…
//	   POST {intl}/userapi/overseasv1/{zai|google}-oauth-login
//	     {source_id, device_id, code, state, navigate_uri} → token 对。
//
// 卡点与解法（2026-10 修正）：
//   - 第 ② 步强制要求阿里云风控验证码（浏览器端 JS SDK，缺它得误导性的
//     631002）。插件控制台**本身就是我们自己的页面**，可以直接加载
//     o.alicdn.com 的官方 SDK（与 ZCode「领套餐」同一套控件），把
//     captchaVerifyParam 交给 LoginStart，再由这里原样转交上游。
//   - Google 的回调白名单是 RFC 8252 loopback 宽松匹配（端口任意），浏览器
//     登录完成后会停在 http://localhost:46952/auth/callback-google?code=…；
//     插件跑在远程主机/Docker 时这个地址到不了服务端，用户把地址栏整条链接
//     粘回来即可换码（第 ③ 步不需要验证码，实测）。
//   - Zai 的白名单只认官方客户端那四个登记端口
//     （18432/19654/19723/53699）。这里固定用 53699：官方客户端启动时会先占
//     第一个空闲端口（通常是 18432），挑末尾那个能避开「回调被桌面端吃掉」。
//
// 因此本实现：Zai / Google 都走完整链路（人机验证 → 授权地址 → 粘贴回调 → 换码）。

// 国际版 OAuth 端点（挂在 userapi 域下）。
const (
	OAuthCaptchaConfigPath = "/userapi/overseasv1/oauth-captcha-config"
	OAuthURLPathZai        = "/userapi/overseasv1/zai-oauth-url"
	OAuthURLPathGoogle     = "/userapi/overseasv1/google-oauth-url"
	OAuthLoginPathZai      = "/userapi/overseasv1/zai-oauth-login"
	OAuthLoginPathGoogle   = "/userapi/overseasv1/google-oauth-login"
)

// oauthCallbackPrefix 是回调路径前缀（与官方客户端逐字同款）。
const oauthCallbackPrefix = "/auth/callback-"

// oauthVendor 标识国际版 OAuth 的两个变体。
type oauthVendor string

const (
	oauthVendorZai    oauthVendor = "zai"
	oauthVendorGoogle oauthVendor = "google"
)

// urlPath / loginPath 返回该变体的两个端点。
func (v oauthVendor) urlPath() string {
	if v == oauthVendorZai {
		return OAuthURLPathZai
	}
	return OAuthURLPathGoogle
}

func (v oauthVendor) loginPath() string {
	if v == oauthVendorZai {
		return OAuthLoginPathZai
	}
	return OAuthLoginPathGoogle
}

// label 返回界面上那个按钮的名字。
func (v oauthVendor) label() string {
	if v == oauthVendorZai {
		return "Zai"
	}
	return "Google"
}

// OAuthCaptchaConfig 取风控验证配置（控制台据此初始化阿里云 SDK）。
//
// 返回 enabled=false 不是错误：上游国际化构建会关掉这条登录方式，入口应当
// 如实告诉用户「这家没有网页登录」，而不是把所有人都引到一次必然失败的滑块。
func OAuthCaptchaConfig(ctx context.Context, baseURL string) (core.CaptchaConfig, error) {
	raw, errPost := postLoginJSON(ctx, baseURL, OAuthCaptchaConfigPath, struct{}{}, "获取风控验证配置", oauthLoginTimeout, ApplyOAuthHeaders)
	if errPost != nil {
		return core.CaptchaConfig{}, core.ToPluginError(errPost)
	}
	code, msg, payload := loginEnvelope(raw)
	if code != 0 {
		return core.CaptchaConfig{}, core.NewPluginError("login_failed", describeLoginError(loginFlowOAuth, code, msg), http.StatusBadGateway)
	}
	var data struct {
		Enabled bool   `json:"enabled"`
		Region  string `json:"region"`
		Prefix  string `json:"prefix"`
		SceneID string `json:"scene_id"`
	}
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return core.CaptchaConfig{}, &core.Failure{Msg: "parse oauth captcha config: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	return core.CaptchaConfig{
		Enabled: data.Enabled,
		SceneID: strings.TrimSpace(data.SceneID),
		Region:  strings.TrimSpace(data.Region),
		Prefix:  strings.TrimSpace(data.Prefix),
	}, nil
}

// OAuthRequestURL 取 OAuth 授权地址并返回官方登录页 URL。
//
// captchaVerifyParam 是控制台页面跑完阿里云 SDK 得到的不透明字符串，网关
// 不解析、只转交。Google 在没有验证码组件的部署形态下可以传空——上游对
// google-oauth-url 的风控要求与 Zai 相同，但 Google 的回调白名单宽松，
// 部分部署实测可跳过；失败时按 631002 文案回退到「填写凭证」。
func OAuthRequestURL(ctx context.Context, baseURL string, vendor oauthVendor, navigateURI, captchaVerifyParam, deviceID string) (string, error) {
	captcha := strings.TrimSpace(captchaVerifyParam)
	if captcha == "" {
		// 本地就挡掉：不带它打上游必然得到那条误导性的 631002，
		// 不如在这里说清「验证码这一步没完成」。
		return "", core.NewPluginError("captcha_required", "缺少风控验证参数，请先完成人机验证", http.StatusBadRequest)
	}
	body := map[string]any{
		"source_id":                protocolName,
		"device_id":                deviceID,
		"navigate_uri":             navigateURI,
		"ali_captcha_verify_param": captcha,
	}
	raw, errPost := postLoginJSON(ctx, baseURL, vendor.urlPath(), body, "获取 "+vendor.label()+" 授权地址", oauthLoginTimeout, ApplyOAuthHeaders)
	if errPost != nil {
		return "", core.ToPluginError(errPost)
	}
	code, msg, payload := loginEnvelope(raw)
	if code != 0 {
		return "", core.NewPluginError("login_failed", describeLoginError(loginFlowOAuth, code, msg), http.StatusBadRequest)
	}
	var data struct {
		OAuthURL string `json:"oauth_url"`
	}
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return "", &core.Failure{Msg: "parse oauth url response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	if strings.TrimSpace(data.OAuthURL) == "" {
		return "", core.NewPluginError("login_failed", "上游没有返回授权地址，请重试", http.StatusBadGateway)
	}
	return strings.TrimSpace(data.OAuthURL), nil
}

// OAuthExchangeCode 用回调里的一次性授权码换凭证。
//
// state 是**上游**回调带回来的那个值（不是插件生成的）——官方客户端就是
// 原样传回；它为空时传空串（上游接受这种形态）。
func OAuthExchangeCode(ctx context.Context, baseURL string, vendor oauthVendor, code, state, navigateURI, deviceID string) (*Credential, error) {
	body := map[string]any{
		"source_id":    protocolName,
		"device_id":    deviceID,
		"code":         strings.TrimSpace(code),
		"state":        strings.TrimSpace(state),
		"navigate_uri": navigateURI,
	}
	raw, errPost := postLoginJSON(ctx, baseURL, vendor.loginPath(), body, vendor.label()+" 登录换取凭证", oauthLoginTimeout, ApplyOAuthHeaders)
	if errPost != nil {
		return nil, core.ToPluginError(errPost)
	}
	status, msg, payload := loginEnvelope(raw)
	if status != 0 {
		return nil, core.NewPluginError("login_failed", describeLoginError(loginFlowOAuth, status, msg), http.StatusBadRequest)
	}
	var data loginResultData
	if errUnmarshal := json.Unmarshal(payload, &data); errUnmarshal != nil {
		return nil, &core.Failure{Msg: "parse oauth login response: " + errUnmarshal.Error(), Kind: core.FailureKindServer}
	}
	return newLoginCredential(data, RegionGlobal, deviceID)
}

// 回调端口：Zai 只能用官方客户端登记过的那四个端口（见文件头），
// Google 走 RFC 8252 loopback 匹配，端口任意。
const (
	oauthCallbackPortZai    = 53699
	oauthCallbackPortGoogle = 46952
)

// OAuthNavigateURI 拼这次登录的回调地址（同时是交给上游的 navigate_uri）。
//
// host 必须是 localhost（Zai 白名单逐字校验 host）；port<=0 时按变体取默认
// 端口。插件不监听这个端口：远程/Docker 部署下浏览器停在「打不开」的页面上，
// 用户把地址栏整条链接粘回来即可。
func OAuthNavigateURI(vendor oauthVendor, port int) string {
	if port <= 0 {
		if vendor == oauthVendorZai {
			port = oauthCallbackPortZai
		} else {
			port = oauthCallbackPortGoogle
		}
	}
	return fmt.Sprintf("http://localhost:%d%s%s", port, oauthCallbackPrefix, vendor)
}

// OAuthVariants 返回控制台要渲染的两个登录入口（顺序与官方客户端一致：Zai 在前）。
func OAuthVariants() []core.LoginVariant {
	return []core.LoginVariant{
		{ID: string(oauthVendorZai), Label: "Zai", CallbackHint: OAuthNavigateURI(oauthVendorZai, 0)},
		{ID: string(oauthVendorGoogle), Label: "Google", CallbackHint: OAuthNavigateURI(oauthVendorGoogle, 0)},
	}
}

// oauthVariantFromMetadata 解析控制台传来的变体标识。
//
// 缺省按 Google：宿主旧路径（直接点「添加认证」）不会带这个字段，而 Google
// 是唯一在任意端口都能完成回调的变体。
func oauthVariantFromMetadata(meta map[string]any) (oauthVendor, error) {
	raw := strings.ToLower(core.GetString(meta, "oauth_variant"))
	switch raw {
	case "", string(oauthVendorGoogle):
		return oauthVendorGoogle, nil
	case string(oauthVendorZai):
		return oauthVendorZai, nil
	}
	return "", core.NewPluginError("invalid_request", "未知的登录方式："+raw, http.StatusBadRequest)
}

// ParseOAuthCallback 从回调链接里提取 code / state / 变体。
//
// 接受两条形态：/auth/callback-zai|google（本插件拼的 navigate_uri）与
// 官方客户端的 query 形态。code 一次性、state 原样带回（换码要用上游的）。
func ParseOAuthCallback(raw string) (vendor oauthVendor, code, state string, ok bool) {
	raw = strings.TrimSpace(raw)
	if index := strings.IndexAny(raw, "\r\n"); index >= 0 {
		raw = raw[:index]
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil {
		return "", "", "", false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
	default:
		// navigate_uri 的 host 必须是回环地址（Zai 白名单逐字校验 localhost；
		// 别的 host 即便 path 相同也是伪造回调）。用户从地址栏复制时，
		// 部分浏览器/代理会把 localhost 规范化成 127.0.0.1 或 [::1]，一并接受。
		return "", "", "", false
	}
	if !strings.HasPrefix(parsed.Path, oauthCallbackPrefix) {
		return "", "", "", false
	}
	switch strings.TrimPrefix(parsed.Path, oauthCallbackPrefix) {
	case string(oauthVendorZai):
		vendor = oauthVendorZai
	case string(oauthVendorGoogle):
		vendor = oauthVendorGoogle
	default:
		return "", "", "", false
	}
	query := parsed.Query()
	code = strings.TrimSpace(query.Get("code"))
	state = strings.TrimSpace(query.Get("state"))
	return vendor, code, state, code != ""
}
