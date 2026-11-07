package tests

// 本文件提供「假宿主」：把宿主回调替换成内存实现，
// 使插件的完整链路（凭证解析、模型注册、出站 HTTP、任务上报、流式转发）
// 都能在没有 CPA 进程的情况下被测试。
//
// 关键设计：
//   - installFakeHost 用 t.Cleanup 做完整还原（含把重试退避压到毫秒级）；
//   - resetPluginGlobals 集中清包级全局——白盒测试最大的坑是缓存跨用例泄漏；
//   - 非流式响应刻意按宿主的「无 tag」形态返回，覆盖 rpcHostHTTPResponse 的兼容逻辑。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/tests/testhost"
)

// abiEnvelope 是插件响应信封（测试用；生产侧已统一到 core）。
type abiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// testCallbackID 是测试用的宿主回调 ID。
const testCallbackID = "test-callback"

// fakeHost 是 tests/testhost.Host 的薄包装：保留既有小写方法名，
// 避免大规模改写用例；新包/新用例直接使用 testhost 的导出 API。
type fakeHost struct{ *testhost.Host }

// fakeUpstreamResponse 是假宿主应答的别名（兼容既有用例）。
type fakeUpstreamResponse = testhost.UpstreamResponse

func newFakeHost() *fakeHost { return &fakeHost{testhost.New()} }

func (h *fakeHost) call(callbackID, method string, payload any) (json.RawMessage, error) {
	return h.Host.Call(callbackID, method, payload)
}

func (h *fakeHost) addAccount(authIndex, name, credentialJSON string) {
	h.AddAccount(authIndex, name, credentialJSON)
}

func (h *fakeHost) setUpstream(fn func(method, url, body string) fakeUpstreamResponse) {
	h.SetUpstream(fn)
}

func (h *fakeHost) emittedChunks() []string { return h.EmittedChunks() }

func (h *fakeHost) streamErrors() []string { return h.StreamErrors() }

func (h *fakeHost) closedStreams() []string { return h.ClosedStreams() }

var testApp *app.App

// installFakeHost 把假宿主接到插件的宿主调用入口上。
func installFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	host := newFakeHost()
	hostbridge.Configure(host.call)
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return host
}

// setupTestPlugin 准备一个使用临时状态目录的 App 实例。
func setupTestPlugin(t *testing.T, mutate ...func(*app.Config)) app.Config {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.LogToFile = false
	for _, apply := range mutate {
		apply(&cfg)
	}
	instance, errNew := app.New(app.Options{Version: "test", Factories: app.BuiltinFactories()})
	if errNew != nil {
		t.Fatalf("new app: %v", errNew)
	}
	testApp = instance
	t.Cleanup(func() {
		instance.StopBackgroundWork()
		instance.WaitPluginStreams(0)
		testApp = nil
	})
	if errApply := instance.ApplyConfig(cfg); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	if _, errState := instance.LoadState(cfg); errState != nil {
		t.Fatalf("load state: %v", errState)
	}
	return cfg
}

// resetPluginGlobals 保留为空：每个用例创建独立 App，不再需要清全局。
func resetPluginGlobals(*testing.T) {}

// buildConfigYAML 生成宿主会传给 plugin.register 的配置 YAML。
//
// 包含宿主追加的 enabled / priority 两键，验证插件对未知字段的容忍。
func buildConfigYAML(cfg app.Config) []byte {
	var builder strings.Builder
	builder.WriteString("enabled: true\npriority: 1\n")
	fmt.Fprintf(&builder, "state_dir: %q\n", cfg.StateDir)
	fmt.Fprintf(&builder, "log_level: %s\n", cfg.LogLevel)
	fmt.Fprintf(&builder, "enabled_realms: %s\n", strings.Join(cfg.EnabledRealms, ","))
	if len(cfg.DisabledModels) > 0 {
		fmt.Fprintf(&builder, "disabled_models: %s\n", strings.Join(cfg.DisabledModels, ","))
	}
	if mode := cfg.Raw["prompt_mode"]; mode != "" {
		fmt.Fprintf(&builder, "prompt_mode: %s\n", mode)
	}
	fmt.Fprintf(&builder, "auto_checkin_at: %q\n", cfg.AutoCheckinAt)
	return []byte(builder.String())
}

// registerRequest 构造 plugin.register 的请求体。
func registerRequest(t *testing.T, cfg app.Config) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(app.LifecycleRequest{ConfigYAML: buildConfigYAML(cfg)})
	if errMarshal != nil {
		t.Fatalf("marshal register request: %v", errMarshal)
	}
	return raw
}

// sampleCredentialJSON 构造一份凭证 JSON（嵌套形态）。
func sampleCredentialJSON(realm, uid string) string {
	return credentialJSON(realm, uid, "ent-1")
}

// personalCredentialJSON 构造一份不带企业租户的普通账号凭证。
//
// 企业账号与个人账号的关键差别就在 enterpriseId 上：企业账号没有签到活动、
// 额度走企业用量接口，所以「个人账号」用例必须显式不带这个字段。
func personalCredentialJSON(realm, uid string) string {
	return credentialJSON(realm, uid, "")
}

func credentialJSON(realm, uid, enterpriseID string) string {
	region := workbuddy.NormalizeRegion(realm)
	enterprise := ""
	if enterpriseID != "" {
		enterprise = fmt.Sprintf(`"enterpriseId": %q, `, enterpriseID)
	}
	return fmt.Sprintf(`{
  "auth": {"accessToken": "at-%s", "refreshToken": "rt-%s", "expiresAt": %d, "domain": "%s", "realm": "%s"},
  "account": {%s"uid": "%s", "nickname": "账号%s"},
  "device_token": "dt-%s"
}`, uid, uid, time.Now().Add(24*time.Hour).Unix(), workbuddy.DefaultDomainFor(region), region, enterprise, uid, uid, uid)
}

// 小工具。

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

// decodeEnvelopeResultForTest 解开插件响应信封（测试辅助）。
func decodeEnvelopeResultForTest(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", errUnmarshal, raw)
	}
	if !env.OK {
		message := "unknown"
		if env.Error != nil {
			message = env.Error.Code + ": " + env.Error.Message
		}
		t.Fatalf("envelope is not ok: %s", message)
	}
	return env.Result
}

// callMethod 调用一个 ABI 方法并解开结果。
func callMethod(t *testing.T, method string, payload any) json.RawMessage {
	t.Helper()
	var request []byte
	if payload != nil {
		request = mustMarshal(payload)
	}
	raw, errHandle := testApp.HandleMethod(method, request)
	if errHandle != nil {
		t.Fatalf("%s: %v", method, errHandle)
	}
	return decodeEnvelopeResultForTest(t, raw)
}

// waitFor 轮询等待条件成立（避免用固定 sleep 造成偶发失败）。
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

// managementCall 调一次插件管理接口并返回响应体（body 可为 nil）。
func managementCall(t *testing.T, method, path string) pluginapi.ManagementResponse {
	t.Helper()
	return managementCallWithBody(t, method, path, nil)
}

// managementCallWithBody 调一次带 JSON body 的插件管理接口。
func managementCallWithBody(t *testing.T, method, path string, body any) pluginapi.ManagementResponse {
	t.Helper()
	request := pluginapi.ManagementRequest{
		Method:  method,
		Path:    "/v0/management/plugins/freetier2api-fork" + path,
		Headers: http.Header{"X-Host-Callback-Id": []string{testCallbackID}},
	}
	if body != nil {
		request.Body = mustMarshal(body)
	}
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(request))
	if errHandle != nil {
		t.Fatalf("%s %s: %v", method, path, errHandle)
	}
	var response pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(decodeEnvelopeResultForTest(t, raw), &response); errUnmarshal != nil {
		t.Fatalf("decode management response: %v", errUnmarshal)
	}
	return response
}
