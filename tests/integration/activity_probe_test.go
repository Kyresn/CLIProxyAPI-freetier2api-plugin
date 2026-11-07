package tests

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/tests/testhost"
)

// activitiesPreviewResponse 是 /activities/preview 的成功响应。
type activitiesPreviewResponse struct {
	OK         bool            `json:"ok"`
	Message    string          `json:"message"`
	Activities []core.PlanInfo `json:"activities"`
	Warning    string          `json:"warning"`
}

// TestZCodeActivityPreviewIsReadOnlyAndDegradesWithoutUserID 覆盖活动探测的两条边界：
//   - 后端只读：探测全程不碰 billing/claim；
//   - 纯 API Key 凭证（无 JWT）可探测；缺 user_id 时报不上活跃，但不整体失败，
//     而是回来一条 warning + 上游当前投放。
func TestZCodeActivityPreviewIsReadOnlyAndDegradesWithoutUserID(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.AddAccount("1", "zcodeglobal-with-uid.json", `{"vendor":"zcodeglobal","api_key":"sk-test","user_id":"u-test","profile":{"platform":"win32","arch":"x64","device_mid":"dev-test"}}`)
	host.AddAccount("2", "zcodeglobal-without-uid.json", `{"vendor":"zcodeglobal","api_key":"sk-test-2","profile":{"platform":"win32","arch":"x64","device_mid":"dev-test-2"}}`)

	var claimCalls, activationCalls int
	host.SetUpstream(func(method, rawURL, _ string) testhost.UpstreamResponse {
		switch {
		case method == http.MethodPost && rawURL == "https://zcode.z.ai/api/v1/event/report":
			activationCalls++
			return testhost.UpstreamResponse{Status: 200, Body: `{"code":0}`}
		case method == http.MethodGet && rawURL == "https://zcode.z.ai/api/v1/marketing/touch?seq=0":
			return testhost.UpstreamResponse{Status: 200, Body: `{"code":0,"data":{"deliveries":[{"campaign_id":"c1","banner":{"background":{"args":{"zcode_plan":{"plan_id":"dev-day-1024","name":"ZCode 1024 Dev Day","priority":9,"starts_at":1792756800,"ends_at":1792976400,"entitlements":[{"show_name":"GLM-5.3-Flash","grant_units":300000000,"period":"one_time"}]}}}}}]}}`}
		case rawURL == "https://zcode.z.ai/api/v1/zcode-plan/billing/claim":
			claimCalls++
		}
		return testhost.UpstreamResponse{Status: 404, Body: `{"code":404}`}
	})

	// 有 user_id：补报两条活跃事件，探测结果完整，无 warning。
	withUID := previewActivities(t, "zcodeglobal-with-uid.json")
	if !withUID.OK || withUID.Warning != "" {
		t.Fatalf("with uid: %+v", withUID)
	}
	if len(withUID.Activities) != 1 || withUID.Activities[0].PlanID != "dev-day-1024" {
		t.Fatalf("activities = %+v", withUID.Activities)
	}
	if withUID.Activities[0].Grants[0].Model != "GLM-5.3-Flash" {
		t.Fatalf("grants = %+v", withUID.Activities[0].Grants)
	}
	if activationCalls != 2 {
		t.Fatalf("activation calls = %d, want 2", activationCalls)
	}

	// 无 user_id：活跃上报不可用，但端点仍成功返回投放，并用 warning 说明原因。
	withoutUID := previewActivities(t, "zcodeglobal-without-uid.json")
	if !withoutUID.OK || len(withoutUID.Activities) != 1 {
		t.Fatalf("without uid: %+v", withoutUID)
	}
	if !strings.Contains(withoutUID.Warning, "user_id") {
		t.Fatalf("warning = %q", withoutUID.Warning)
	}

	// 参数与能力边界：缺账号标识、供应商没有该能力，都要给出可读原因。
	if missing := previewActivities(t, ""); missing.OK || missing.Message != "缺少 account_id" {
		t.Fatalf("missing account: %+v", missing)
	}
	host.AddAccount("3", "opencodezen-oc_test.json", `{"api_key":"sk-test-opencode"}`)
	if unsupported := previewActivities(t, "opencodezen-oc_test.json"); unsupported.OK || !strings.Contains(unsupported.Message, "活动探测") {
		t.Fatalf("unsupported vendor: %+v", unsupported)
	}

	if claimCalls != 0 {
		t.Fatalf("activity preview invoked claim %d times", claimCalls)
	}
}

// previewActivities 调一次管理端点并解出响应体。
func previewActivities(t *testing.T, accountID string) activitiesPreviewResponse {
	t.Helper()
	raw, err := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(pluginapi.ManagementRequest{
		Method: "POST", Path: "/v0/management/plugins/freetier2api/activities/preview",
		Headers: http.Header{"X-Host-Callback-Id": []string{testCallbackID}}, Body: []byte(`{"account_id":"` + accountID + `"}`),
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	var env abiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope: %v raw=%s", err, raw)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("response: %v raw=%s env=%+v", err, raw, env)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var body activitiesPreviewResponse
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("body: %v body=%s", err, resp.Body)
	}
	return body
}
