package tests

// 本文件覆盖 auth 能力：登录分发与跨供应商凭证归属判定。

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/tests/testhost"
)

// 登录会话绑定的回归测试。
//
// 守卫的问题：宿主靠 AuthLoginStartResponse.Metadata 里的 vendor 字段把登录
// 会话绑定回具体供应商；缺了它宿主拿不到授权链接，页面只显示「获取授权链接
// 失败」。这条约束曾经只靠各供应商自己记得设置，导致 codearts / trae 等
// 供应商长期无法添加账号——而插件侧 LoginStart 明明成功返回了
// URL，日志上只有一句 "started login"，排查方向被完全带偏。
//
// 现在标识由根层（auth_login.go 的分发点）统一注入，本用例即该契约的守卫。

// TestLoginStartInjectsVendorMetadata 验证根层为供应商补齐登录会话绑定。
//
// 用**真实的 codearts** 而不是桩：它的 LoginStart 纯本地构造（无网络调用），
// 且其包内实现本身不设置 Metadata——正是回归要覆盖的场景。
func TestLoginStartInjectsVendorMetadata(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	vendor, okVendor := testApp.Registry().ByID("codearts")
	if !okVendor {
		t.Fatal("codearts vendor not registered")
	}

	result := callMethod(t, pluginabi.MethodAuthLoginStart, map[string]any{
		"metadata": map[string]any{core.VendorKey: vendor.ID()},
	})

	var response pluginapi.AuthLoginStartResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode login response: %v (raw=%s)", errUnmarshal, result)
	}
	if response.URL == "" {
		t.Fatal("授权链接为空")
	}
	// 核心断言：供应商未设置 Metadata，根层也必须补上 vendor 绑定。
	if got := response.Metadata[core.VendorKey]; got != vendor.ID() {
		t.Fatalf("metadata.vendor = %v, want %q（宿主靠它绑定登录会话）", got, vendor.ID())
	}
	if got := response.Metadata["region"]; got != vendor.Region() {
		t.Fatalf("metadata.region = %v, want %q", got, vendor.Region())
	}
}

// 跨供应商归属判定的回归测试。
//
// 这是本插件最关键的守卫：宿主把所有非内建格式的凭证依次喂给每个插件，
// 归属判定过宽会让宿主用本插件的结构覆盖对方账号（且是静默的）。
// 新增供应商时，这些用例保证「各家的凭证只被自家认领」。

func TestAuthParseVendorOwnershipDiscrimination(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// 判据说明：这些用例**刻意不给 provider**（空串），模拟宿主还没把文件
	// 归属给任何插件时的判定。给 provider 会让 workbuddy 的
	// `provider == "freetier"` 兜底把一切都收下，测试就测不出判别力了。
	cases := []struct {
		name       string
		fileName   string
		provider   string
		body       string
		wantHandle bool
	}{
		// OpenCodeZEN：opencodezen 文件名 + api_key。
		{"opencodezen file name", "opencodezen-key1.json", "", `{"api_key":"sk-abc"}`, true},
		{"opencodezen zen_key field", "my-account.json", "", `{"zen_key":"sk-abc"}`, true},
		// Cline：cline 文件名 / workos 前缀 / 驼峰 refreshToken。
		{"cline file name", "cline-me@x.com.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		{"cline workos prefix", "acc.json", "", `{"access_token":"workos:at"}`, true},
		// 既有供应商：仍按各自约定认领。
		{"workbuddy nested", "workbuddycn-u1.json", "", `{"account":{"uid":"u1"},"auth":{"accessToken":"at","realm":"cn"}}`, true},
		{"qoder pt token", "qodercn-u1.json", "", `{"token":"pt-abc"}`, true},
		// 新增供应商：ZCode, Trae 国内版/国际版, CodeArts, MiniMax Code
		{"zcode filename", "zcode-key1.json", "", `{"api_key":"abc.123"}`, true},
		{"traecn filename", "traecn-user1.json", "", `{"accessToken":"at_123"}`, true},
		{"traeglobal filename", "traeglobal-user1.json", "", `{"accessToken":"at_123","region":"global"}`, true},
		{"codearts filename", "codearts-user1.json", "", `{"access_key_id":"ak","secret_access_key":"sk"}`, true},
		{"minimaxcodecn filename", "minimaxcodecn-user1.json", "", `{"token":"at_123"}`, true},
		{"minimaxcodeglobal filename", "minimaxcodeglobal-user1.json", "", `{"token":"at_123","region":"global"}`, true},

		// 新增供应商：AutoClaw 国内版/国际版、小浣熊。
		{"autoclawcn filename", "autoclawcn-user1.json", "", `{"accessToken":"at","refreshToken":"rt","region":"cn"}`, true},
		{"autoclawglobal filename", "autoclawglobal-user1.json", "", `{"accessToken":"at","refreshToken":"rt","region":"global"}`, true},
		{"autoclaw vendor field", "acc.json", "", `{"vendor":"autoclawcn","accessToken":"at"}`, true},
		{"raccoon filename", "raccoon-7591340.json", "", `{"access_token":"a","refresh_token":"r","uid":"7591340"}`, true},
		{"raccoon pinyin filename", "xiaohuanxiong-u1.json", "", `{"access_token":"a","refresh_token":"r","uid":"1"}`, true},
		// 跨供应商误吞防护：通用结构必须落在**正确**的供应商头上。
		{"qoder pt token is qoder", "acc.json", "", `{"access_token":"pt-abc"}`, true},
		{"cline camel is cline", "acc.json", "", `{"accessToken":"at","refreshToken":"rt"}`, true},
		// 真正谁都不该认的：只含通用字段、没有专属特征。
		{"flat device token is nothing", "acc.json", "", `{"device_token":"dt-abc","account":{"uid":"u1"}}`, false},
		{"generic snake pair is nothing", "acc.json", "", `{"access_token":"at","refresh_token":"rt"}`, false},
		{"generic token is nothing", "acc.json", "", `{"token":"sk-abc"}`, false},
		{"generic access_token is nothing", "acc.json", "", `{"access_token":"at"}`, false},
		{"workbuddy provider hint overrides", "other.json", app.ProviderKey, `{"accessToken":"at"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				Provider: tc.provider,
				FileName: tc.fileName,
				RawJSON:  []byte(tc.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if response.Handled != tc.wantHandle {
				t.Fatalf("handled = %v, want %v", response.Handled, tc.wantHandle)
			}
		})
	}
}

// TestLegacyWorkBuddyAccountNotClaimedByCline 验证安装 FreeTier 2API 前添加的 CodeBuddy/WorkBuddy 账号
// 不会被 Cline 抢先误吞，且国内版和国际版分别归属到 workbuddycn 和 workbuddyglobal。
func TestLegacyWorkBuddyAccountNotClaimedByCline(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	cases := []struct {
		name       string
		fileName   string
		body       string
		wantVendor string
	}{
		{
			name:       "workbuddy global flat legacy file",
			fileName:   "workbuddy-fb0e95d.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","domain":"www.workbuddy.ai","uid":"fb0e95d"}`,
			wantVendor: "workbuddyglobal",
		},
		{
			name:       "workbuddy global legacy with device token",
			fileName:   "workbuddy-user1.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","domain":"workbuddy.ai","device_token":"dt_789"}`,
			wantVendor: "workbuddyglobal",
		},
		{
			name:       "workbuddy cn flat legacy file",
			fileName:   "workbuddy-user2.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","domain":"www.codebuddy.cn","uid":"user2"}`,
			wantVendor: "workbuddycn",
		},
		{
			name:       "workbuddy cn default legacy file",
			fileName:   "workbuddy-user3.json",
			body:       `{"accessToken":"at_123","refreshToken":"rt_456","uid":"user3"}`,
			wantVendor: "workbuddycn",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
				FileName: tc.fileName,
				RawJSON:  []byte(tc.body),
			})
			var response pluginapi.AuthParseResponse
			if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
				t.Fatalf("decode parse response: %v", errUnmarshal)
			}
			if !response.Handled {
				t.Fatalf("expected account %s to be handled", tc.fileName)
			}
			gotVendor := response.Auth.Attributes[core.VendorKey]
			if gotVendor != tc.wantVendor {
				t.Fatalf("vendor = %q, want %q (must not be claimed by cline)", gotVendor, tc.wantVendor)
			}
		})
	}
}

// ---- localhost 回调粘贴兜底 ----
//
// 下面三条守卫覆盖用户实际遇到的两种卡死：
//
//  1. Trae 的会话表自持在 SessionStore，根层按公共登记表找不到它，轮询一律被判成
//     「登录会话不存在或已过期」——用户看到的正是「授权失败：登录会话不存在…」。
//  2. 两家的上游都只认 localhost 回调，插件与浏览器不在同一台机器时收不到，
//     用户把地址栏里的整条链接粘回来即可完成登录。

// startVendorLogin 发起一次登录并返回带 state 的响应。
func startVendorLogin(t *testing.T, vendorID string) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw := callMethod(t, pluginabi.MethodAuthLoginStart, map[string]any{
		"metadata": map[string]any{core.VendorKey: vendorID},
	})
	var response pluginapi.AuthLoginStartResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		t.Fatalf("decode login start: %v (raw=%s)", errUnmarshal, raw)
	}
	if response.State == "" {
		t.Fatalf("%s 登录未返回 state：%s", vendorID, raw)
	}
	return response
}

// codeartsPastedCallback 是浏览器授权后停在 127.0.0.1 上的真实回调链接样本。
//
// 关键证据：ticket_id / secret 均为**华为云签发**，不是 LoginStart 送进
// /authorize 的那一对；插件此前拿自造值轮询，控制台因此永远停在「等待授权中…」。
const codeartsPastedCallback = "http://127.0.0.1/oauth/callback?secret=d55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280" +
	"&redirect=https%3A%2F%2Fcodearts.huaweicloud.com%2Fportal%2Flogin%3Ftheme%3Dlight%26locale%3Dzh-cn" +
	"%26ticket_id%3D657abbb247eab363d1a461285fc50145%26authorizationAvailable%3Dfalse" +
	"%26secret%3Dd55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280"

const codeartsUpstreamSecret = "0628ec6bf686e409a532e813dc1a0140e49d1dcef1f96e2252f5a67e01d715d9"

// TestTraeLoginPollReachesVendorSession 锁定 Trae 的登录轮询能找回自己的会话。
//
// 回归的是「会话表自持 + 根层只查公共登记表」的组合：两者凑在一起，Trae 的每一次
// 轮询都会被判成「登录会话不存在或已过期」，登录根本走不到浏览器那一步。
func TestTraeLoginPollReachesVendorSession(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	started := startVendorLogin(t, "traecn")
	raw := callMethod(t, pluginabi.MethodAuthLoginPoll, map[string]any{"state": started.State})
	var polled pluginapi.AuthLoginPollResponse
	if errUnmarshal := json.Unmarshal(raw, &polled); errUnmarshal != nil {
		t.Fatalf("decode poll: %v (raw=%s)", errUnmarshal, raw)
	}
	if polled.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending（message=%q）", polled.Status, polled.Message)
	}
}

// TestSubmitLoginCallbackCompletesTraeLogin 端到端验证 Trae 的粘贴兜底：从登录链接里
// 取出 trace/machine 构造浏览器回调，提交后应当拿到凭证。
func TestSubmitLoginCallbackCompletesTraeLogin(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	started := startVendorLogin(t, "traecn")
	parsed, errParse := url.Parse(started.URL)
	if errParse != nil {
		t.Fatalf("parse login url: %v", errParse)
	}
	query := parsed.Query()
	trace, machine := query.Get("login_trace_id"), query.Get("machine_id")
	if trace == "" || machine == "" {
		t.Fatalf("登录链接缺少 trace/machine: %s", started.URL)
	}

	// 回调里刻意不带 refreshToken：带的话插件会去上游换 token，用例就得联网。
	callback := "http://127.0.0.1:18080/authorize?loginTraceID=" + trace + "&machine_id=" + machine +
		"&userJwt=" + url.QueryEscape(`{"Token":"jwt-access","TokenExpireAt":1793607527194}`) +
		"&userInfo=" + url.QueryEscape(`{"UserID":"user-1","ScreenName":"tester","TenantID":"tenant-1"}`)

	result, errSubmit := testApp.SubmitLoginCallback(context.Background(), started.State, callback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success（message=%q）", result.Status, result.Message)
	}
	if result.Auth.Label != "Trae-tester" || result.Auth.FileName != "traecn-user-1.json" {
		t.Fatalf("凭证标识不对: label=%q file=%q", result.Auth.Label, result.Auth.FileName)
	}
}

// TestSubmitLoginCallbackCompletesCodeArtsLogin 端到端验证 CodeArts 的粘贴兜底。
//
// 关键点：票据号必须仍是**本会话自己那个**（从授权链接里取），只有 secret 才换成
// 门户下发的。拿回调里那个门户中间票去查票据接口，上游只会回「无效ticketId」。
func TestSubmitLoginCallbackCompletesCodeArtsLogin(t *testing.T) {
	var gotTicket string
	host := testhost.New()
	host.SetUpstream(func(method, rawURL, body string) testhost.UpstreamResponse {
		if !strings.Contains(rawURL, "/v1/login/ticket") {
			return testhost.UpstreamResponse{Status: 404, Body: `{"code":404,"msg":"not stubbed"}`}
		}
		// 入参 url 遮蔽了 net/url 包，这里按字符串解析回查询参数。
		query, errQuery := url.Parse(rawURL)
		if errQuery != nil {
			return testhost.UpstreamResponse{Status: 400, Body: `{"code":400,"msg":"bad url"}`}
		}
		gotTicket = query.Query().Get("ticket_id")
		if query.Query().Get("secret") != codeartsUpstreamSecret {
			t.Errorf("票据查询没用门户下发的 secret: %s", rawURL)
		}
		return testhost.UpstreamResponse{
			Status: 200,
			Header: map[string][]string{"Content-Type": {"application/json"}},
			Body: `{"user_id":"user-1","user_name":"tester","refresh_token":"refresh-1",` +
				`"credentials":{"access_key_id":"AK-test","secret_access_key":"SK-test",` +
				`"security_token":"ST-test","expiration":"2026-10-19T18:00:00Z"}}`,
		}
	})
	hostbridge.Configure(host.Call)
	t.Cleanup(func() { hostbridge.Configure(nil) })
	setupTestPlugin(t)

	started := startVendorLogin(t, "codearts")
	parsed, errParse := url.Parse(started.URL)
	if errParse != nil {
		t.Fatalf("parse login url: %v", errParse)
	}
	sessionTicket := parsed.Query().Get("ticket_id")
	if sessionTicket == "" {
		t.Fatalf("授权链接缺少 ticket_id: %s", started.URL)
	}

	// 按本会话票据号拼一条真实形状的回调：顶层 secret + redirect 里的门户登录页。
	portal := "https://codearts.huaweicloud.com/portal/login?theme=light&locale=zh-cn&ticket_id=" + sessionTicket
	callback := "http://127.0.0.1:7866/oauth/callback?secret=" + codeartsUpstreamSecret +
		"&redirect=" + url.QueryEscape(portal)

	result, errSubmit := testApp.SubmitLoginCallback(context.Background(), started.State, callback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success（message=%q）", result.Status, result.Message)
	}
	if gotTicket != sessionTicket {
		t.Fatalf("票据查询用了 %q，want 本会话的 %q", gotTicket, sessionTicket)
	}
	if result.Auth.FileName != "codearts-user-1.json" {
		t.Fatalf("file name = %q, want codearts-user-1.json", result.Auth.FileName)
	}
	if !strings.Contains(string(result.Auth.StorageJSON), "AK-test") {
		t.Fatalf("凭证未落盘: %s", result.Auth.StorageJSON)
	}
}

// TestSubmitLoginCallbackRejectsSessionlessVendor 守住边界：设备码类供应商没有本地
// 回调，粘贴入口必须明确拒绝，而不是给一个让人摸不着头脑的失败。
func TestSubmitLoginCallbackRejectsSessionlessVendor(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	// Qoder 国内版是设备码登录：LoginStart 纯本地构造，且没有本地回调可粘。
	started := startVendorLogin(t, "qodercn")
	if _, errSubmit := testApp.SubmitLoginCallback(
		context.Background(), started.State, "http://127.0.0.1/authorize?a=1"); errSubmit == nil {
		t.Fatal("没有本地回调的供应商必须拒绝粘贴")
	}
}

// TestVendorAccountLimitEnforced 验证单个供应商最多允许添加 10 个账号（core.MaxAccountsPerVendor）。
func TestVendorAccountLimitEnforced(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	for i := 1; i <= core.MaxAccountsPerVendor; i++ {
		id := string(rune(96 + i))
		host.AddAccount(id, "codearts-user-"+id+".json", `{"vendor":"codearts","access_key_id":"ak","secret_access_key":"sk"}`)
	}

	// 同供应商已满 10 个账号时，发起第 11 个登录必须被拒绝。
	_, errStart := testApp.HandleMethod(pluginabi.MethodAuthLoginStart, mustMarshal(map[string]any{
		"metadata": map[string]any{core.VendorKey: "codearts"},
	}))
	if errStart == nil || !strings.Contains(errStart.Error(), "最多添加 10 个账号") {
		t.Fatalf("expected account limit error for codearts, got %v", errStart)
	}

	// 其它未满 10 个账号的供应商不受影响。
	if _, errOther := testApp.HandleMethod(pluginabi.MethodAuthLoginStart, mustMarshal(map[string]any{
		"metadata": map[string]any{core.VendorKey: "qodercn"},
	})); errOther != nil {
		t.Fatalf("other vendor should still be allowed, got %v", errOther)
	}
}
