package tests

// 本文件锁定管理页「刷新即取数」的两条韧性约束。
//
// 真实事故：Trae 国内版的「剩余 / 总额」与模型列表时有时无。根因不是上游
// 限流（直连上游 200/0.5s），而是两处生命周期绑错了对象：
//
//  1. 额度批量查询挂在管理 RPC 的 context 上。页面刷新会让宿主取消这次 RPC，
//     整轮查询半途而废，一个账号都没写进缓存，账号表于是停在「查询中…」。
//  2. 模型清单缓存只在 static 探测与手动刷新时写入。插件重载后若探测未跑完，
//     控制台就整家供应商缺模型；而前端只在「合计为 0」时才自动刷新。
//
// 对应修复：额度查询改到后台作用域（单轮 singleflight），模型列表在缓存为
// 空时按供应商补探测（singleflight + 冷却）。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// traeQuotaFixture 是 Trae 额度接口的最小响应（product_type=2 为积分包）。
const traeQuotaFixture = `{"user_entitlement_pack_list":[
 {"entitlement_base_info":{"product_type":2,"quota":{"credits_limit":4000}},"usage":{"credits_amount":1.5}}
]}`

// addTraeAccount 注册一个 Trae 国内版账号，额度接口由假宿主应答。
func addTraeAccount(t *testing.T, host *fakeHost) {
	t.Helper()
	host.addAccount("1", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})
}

// TestQuotaRunSurvivesManagementCancel 验证额度查询不随管理 RPC 取消而中断。
//
// 管理上下文被取消后，后台那一轮必须继续跑完并写缓存，下一次 /status 就能
// 拿到额度结果，而不是重新回到「查询中…」。
func TestQuotaRunSurvivesManagementCancel(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)

	// 被取消的 RPC：宿主取消时会直接关掉请求上下文。
	resp := managementCallWithCancel(t, "POST", "/quotas")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}

	// 后台轮次仍在跑：等缓存出现后，/status 必须带额度结果。
	if !waitFor(t, 5*time.Second, func() bool {
		accounts := statusAccounts(t)
		return len(accounts) == 1 && accounts[0].Quota != nil
	}) {
		t.Fatal("quota batch must finish in the background even when the management RPC is cancelled")
	}
	accounts := statusAccounts(t)
	if !accounts[0].Quota.OK {
		t.Fatalf("trae quota should succeed, got %+v", accounts[0].Quota)
	}
}

// TestConcurrentQuotaRequestsShareOneRun 验证 singleflight：在本轮尚未结束时
// 连续刷新页面（宿主收到多个 /quotas）不会把同一批账号重复打向上游。
func TestConcurrentQuotaRequestsShareOneRun(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	host.addAccount("1", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return fakeUpstreamResponse{Status: http.StatusOK, Body: traeQuotaFixture}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	done := make(chan struct{}, 4)
	go func() { managementCall(t, "POST", "/quotas"); done <- struct{}{} }()
	<-started // 第一轮已进入上游，后续请求应等待同一轮
	for i := 0; i < 3; i++ {
		go func() { managementCall(t, "POST", "/quotas"); done <- struct{}{} }()
	}
	time.Sleep(50 * time.Millisecond) // 让后三个请求拿到同一个进行中的轮次
	close(release)
	for i := 0; i < 4; i++ {
		<-done
	}
	if calls := host.UpstreamCalls(); calls != 1 {
		t.Fatalf("quota upstream calls = %d, want 1 (singleflight)", calls)
	}
}

// TestConsoleModelsProbeWhenCacheEmpty 验证控制台在缓存为空时补探测。
//
// 场景：插件刚重载，宿主还没跑 static 探测，但凭证已加载（active）。此时
// /models 必须主动探测一次把模型列出来，而不是整家供应商缺模型。
func TestConsoleModelsProbeWhenCacheEmpty(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	resp := managementCall(t, "GET", "/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/models = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Models []struct {
			ID       string `json:"id"`
			VendorID string `json:"vendor_id"`
		} `json:"models"`
		PendingProbes int `json:"pending_probes"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode models: %v (%s)", errUnmarshal, resp.Body)
	}
	var found bool
	for _, model := range payload.Models {
		if model.ID == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("console models must include the probed vendor models, got %+v (pending=%d)",
			payload.Models, payload.PendingProbes)
	}
}

// TestModelsForAuthFillsConsoleCache 验证 model.for_auth 顺手写入控制台缓存。
//
// 宿主加载凭证走的就是这条 RPC；写入后控制台即便不做补探测也能读到清单。
func TestModelsForAuthFillsConsoleCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"for-auth-only","name":"For Auth"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})

	models := testApp.RegisteredModels("workbuddycn")
	var found bool
	for _, model := range models {
		if model.ID == "for-auth-only" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.for_auth must fill the console cache, got %+v", models)
	}
}

// managementCallWithCancel 发起一次管理调用，并模拟宿主在请求发出后立即取消。
func managementCallWithCancel(t *testing.T, method, path string) pluginapi.ManagementResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: method,
		Path:   "/v0/management/plugins/freetier2api" + path,
		Headers: http.Header{
			"X-Host-Callback-Id": []string{testCallbackID},
		},
		Context: ctx,
	}))
	if errHandle != nil {
		t.Fatalf("%s %s: %v", method, path, errHandle)
	}
	var env abiEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode envelope: %v", errUnmarshal)
	}
	var resp pluginapi.ManagementResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode management response: %v (raw=%s)", errUnmarshal, env.Result)
	}
	return resp
}

// TestForAuthFailureDoesNotWipeModelCache 复现 OpenCode 模型偶发消失的事故。
//
// 供应商在探测失败时按契约返回空清单（让宿主淘汰坏凭证）。若这条路径把空
// 清单写进控制台缓存，一次瞬时 401 就会让整家供应商的模型消失，且要等下一轮
// 探测成功才回来。期望：宿主拿到的仍是空清单，但控制台缓存保持原样。
//
// 用 OpenCode ZEN 而非 WorkBuddy 复现：前者每次 model.for_auth 都真打上游
// （WorkBuddy 有 1 小时目录缓存，第二次不会失败）。
func TestForAuthFailureDoesNotWipeModelCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)

	failing := false
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if !strings.Contains(url, "/inference/v1/models") {
			return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
		}
		if failing {
			return fakeUpstreamResponse{Status: http.StatusUnauthorized, Body: `{"error":{"message":"Invalid credential"}}`}
		}
		return fakeUpstreamResponse{
			Status: http.StatusOK,
			Body:   `{"data":[{"id":"kept-model-free"},{"id":"other-model"}]}`,
		}
	})

	// 先成功一次，写入控制台缓存。
	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	if len(testApp.RegisteredModels("opencodezen")) == 0 {
		t.Fatal("first for_auth call must populate the console cache")
	}

	// 再让上游失败：宿主响应仍为空（契约不变），但缓存不能被清空。
	failing = true
	raw := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var resp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		t.Fatalf("decode model response: %v", errUnmarshal)
	}
	if len(resp.Models) != 0 {
		t.Fatalf("host must still receive an empty list for a failing credential, got %d", len(resp.Models))
	}
	models := testApp.RegisteredModels("opencodezen")
	if len(models) == 0 || models[0].ID != "kept-model-free" {
		t.Fatalf("console cache must survive a failed probe, got %+v", models)
	}
}

// TestRefreshZeroDoesNotWipeCache 验证上游返回空清单时不清空既有缓存。
func TestRefreshZeroDoesNotWipeCache(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))

	returnEmpty := false
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if !strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
		}
		if returnEmpty {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: `{"code":0,"data":{"models":[]}}`}
		}
		return fakeUpstreamResponse{
			Status: http.StatusOK,
			Body:   `{"code":0,"data":{"models":[{"id":"kept-model","name":"Kept"}]}}`,
		}
	})

	managementCall(t, "POST", "/models/refresh")
	if len(testApp.RegisteredModels("workbuddycn")) == 0 {
		t.Fatal("refresh must populate the cache")
	}

	returnEmpty = true
	managementCall(t, "POST", "/models/refresh")
	if models := testApp.RegisteredModels("workbuddycn"); len(models) == 0 || models[0].ID != "kept-model" {
		t.Fatalf("an empty upstream list must not wipe the cache, got %+v", models)
	}
}

// TestModelCachePersistsToState 验证探测成功后清单落盘，插件重载可回灌。
func TestModelCachePersistsToState(t *testing.T) {
	host := installFakeHost(t)
	cfg := setupTestPlugin(t)
	host.addAccount("1", "cn-account", sampleCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/v3/config") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"models":[{"id":"persisted-model","name":"Persisted"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})

	statePath := filepath.Join(cfg.StateDir, "state.json")
	persisted := waitFor(t, 5*time.Second, func() bool {
		raw, errRead := os.ReadFile(statePath)
		return errRead == nil && strings.Contains(string(raw), "persisted-model")
	})
	if !persisted {
		t.Fatalf("model list must be persisted to %s after a successful probe", statePath)
	}
}

// TestUnavailableCredentialStillServesConsole 复现「模型偶发消失 + 额度永远查询中」。
//
// 事故链：上游限流（WorkBuddy code:6004）或超时后，宿主路由层把凭证标成
// unavailable（临时冷却，稍后自动恢复）。插件若把它当成「没有凭证」：
//   - 模型列表整家消失（ActiveVendorSet 不再包含该供应商）；
//   - 额度批量查询跳过该账号 → 没有结果 → 页面永远停在「查询中…」。
//
// 用 OpenCode ZEN 而非 Trae 验证模型侧：它没有 ConsoleModelProvider，
// 控制台列表直接来自 Catalog 缓存，能干净地反映「这家有没有被跳过」。
func TestUnavailableCredentialStillServesConsole(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/inference/v1/models") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: `{"data":[{"id":"kept-model-free"}]}`}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})
	// 宿主因上游限流给它打上临时冷却。
	host.MarkUnavailable("opencodezen-oc_test.json", true)

	models := managementCall(t, "GET", "/models")
	var payload struct {
		Models []struct {
			VendorID string `json:"vendor_id"`
		} `json:"models"`
	}
	if errUnmarshal := json.Unmarshal(models.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode models: %v (%s)", errUnmarshal, models.Body)
	}
	var listed bool
	for _, model := range payload.Models {
		if model.VendorID == "opencodezen" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("a temporarily unavailable credential must not drop its vendor from the model list: %s", models.Body)
	}
}

// TestUnavailableCredentialStillQueried 锁定账号概览的额度列。
//
// 与模型列表同一个根因：额度批量查询复用 pool.Load 的凭证过滤，把宿主的
// 临时冷却当成「没有凭证」后整批跳过，账号行拿不到任何结果，页面于是永远
// 停在「查询中…」。凭证只是被限流，额度接口本身是能查的。
func TestUnavailableCredentialStillQueried(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)
	host.MarkUnavailable("traecn-test.json", true)

	if resp := managementCall(t, "POST", "/quotas"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/quotas = %d: %s", resp.StatusCode, resp.Body)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, account := range statusAccounts(t) {
			if account.Quota != nil && account.Quota.OK {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("a temporarily unavailable credential must still yield quota, got %+v", statusAccounts(t))
	}
}

// TestCooldownWithFailingUpstreamKeepsConsole 覆盖冷却的真正形态。
//
// 凭证被标成 unavailable 的原因通常就是上游正在失败（限流、超时）。此时：
//   - 额度查询会失败，但**必须缓存成 ok=false**，页面显示「-- / --」而不是
//     永远停在「查询中…」——「查过但失败」与「还没查」是两种状态；
//   - 模型探测同样失败，但**已缓存的清单必须保留**，不能让整家供应商消失。
//
// 两个断言分别用两家供应商：OpenCode ZEN 有模型接口但没有额度接口，用它
// 验证缓存保留；Trae 有额度接口，用它验证失败结果被缓存（用 OpenCode 测
// 额度会走「不支持」分支，等于没测到冷却）。
func TestCooldownWithFailingUpstreamKeepsConsole(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode","label":"zen-test"}`)
	host.addAccount("2", "traecn-test.json",
		`{"vendor":"traecn","region":"cn","access_token":"test-access-token","uid":"2487573687317868"}`)

	healthy := true
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if !healthy {
			return fakeUpstreamResponse{Status: http.StatusTooManyRequests, Body: `{"error":{"message":"rate limited"}}`}
		}
		if strings.Contains(url, "/inference/v1/models") {
			return fakeUpstreamResponse{Status: http.StatusOK, Body: `{"data":[{"id":"kept-model-free"}]}`}
		}
		if strings.Contains(url, "pay/ide_user_ent_usage") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Header: map[string][]string{"Content-Type": {"application/json"}},
				Body:   traeQuotaFixture,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	// 先成功一次，把模型清单写进缓存。
	managementCall(t, "GET", "/models")

	// 上游开始失败，宿主同时打上临时冷却。
	healthy = false
	host.MarkUnavailable("opencodezen-oc_test.json", true)
	host.MarkUnavailable("traecn-test.json", true)

	// 模型：已缓存的清单必须保留。
	models := managementCall(t, "GET", "/models")
	if !strings.Contains(string(models.Body), "kept-model-free") {
		t.Fatalf("cached models must survive a failing probe during cooldown: %s", models.Body)
	}

	// 额度：失败也必须留下一条「已确定」的结果，而不是继续「查询中」。
	managementCall(t, "POST", "/quotas")
	var traeQuota *struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if !waitFor(t, 5*time.Second, func() bool {
		for _, account := range statusAccounts(t) {
			if account.VendorID == "traecn" && account.Quota != nil {
				traeQuota = account.Quota
				return true
			}
		}
		return false
	}) {
		t.Fatal("a failed quota query must still be cached; otherwise the console shows「查询中…」forever")
	}
	if traeQuota.OK {
		t.Fatalf("quota must report failure during cooldown, got %+v", traeQuota)
	}
	if traeQuota.Message == "" {
		t.Fatal("a failed quota must carry the upstream reason so the console can explain it")
	}
}

// TestDisabledCredentialSkipsQuota 锁住另一半边界：用户显式禁用仍然生效。
//
// /status 会列出禁用账号（页面要能显示并允许重新启用），所以断言落在
// 「额度没有被查询」上，而不是「账号不在列表里」。
func TestDisabledCredentialSkipsQuota(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	addTraeAccount(t, host)
	host.MarkDisabled("traecn-test.json", true)

	managementCall(t, "POST", "/quotas")
	time.Sleep(200 * time.Millisecond) // 给后台轮次一个窗口，确认它确实没有查询
	for _, account := range statusAccounts(t) {
		if account.Quota != nil {
			t.Fatalf("a disabled credential must not be queried for quota, got %+v", account.Quota)
		}
	}
}
