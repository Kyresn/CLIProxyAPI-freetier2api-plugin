package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// malformedJSON 是「非空但不合法」的请求体：不能当作空请求继续按默认值执行。
const malformedJSON = `{"name":`

func TestDecodeRequestRejectsMalformedJSON(t *testing.T) {
	type request struct {
		Name string `json:"name"`
	}
	if _, errDecode := decodeRequest[request]([]byte(malformedJSON)); errDecode == nil {
		t.Fatal("decodeRequest accepted malformed JSON")
	}
	if _, errEmpty := decodeRequest[request](nil); errEmpty != nil {
		t.Fatalf("empty body should be accepted: %v", errEmpty)
	}
}

// TestDecodeRequestIntoKeepsDefaults 锁定「请求体只覆盖它给出的字段」。
func TestDecodeRequestIntoKeepsDefaults(t *testing.T) {
	type request struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
		School      bool `json:"school"`
	}
	base := request{Concurrency: 1, Growth: true, School: true}
	got, errDecode := decodeRequestInto([]byte(`{"concurrency":3}`), base)
	if errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if got != (request{Concurrency: 3, Growth: true, School: true}) {
		t.Fatalf("decodeRequestInto = %+v, want defaults kept", got)
	}
}

func TestBadJSONRequestResponseIs400(t *testing.T) {
	response := BadJSONRequestResponse()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("body is not JSON: %v", errUnmarshal)
	}
	if payload["ok"] != false || payload["error"] != "请求体解析失败，请检查 JSON 格式" {
		t.Fatalf("body = %s", response.Body)
	}
}

// TestManagementHandlersRejectMalformedJSON 覆盖所有解析请求体的管理入口。
//
// 只查「不合法 JSON 一律 400」：正常路径由各自的功能用例负责。handler 在
// 解析失败后立即返回，因此不需要配置宿主依赖。
func TestManagementHandlersRejectMalformedJSON(t *testing.T) {
	service := newService(t)
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Body: []byte(malformedJSON)}
	handlers := map[string]func(pluginapi.ManagementRequest) pluginapi.ManagementResponse{
		"accounts/delete": service.handleAccountDeleteRequest,
		"accounts/add":    service.handleAccountAddRequest,
		"accounts/alias":  service.handleAccountAliasRequest,
		"checkin":         service.handleCheckinRequest,
		"quotas":          service.handleQuotasRequest,
		"settings":        service.handleSettingsRequest,
		"plans/preview":   service.handlePlansPreview,
		"plans/claim":     service.handlePlansClaim,
		"tasks/run":       service.handleTasksRun,
		"tasks/auto":      service.handleTasksAuto,
		"models/toggle":   service.modelToggleHandler,
		"models/alias":    service.modelAliasHandler,
		"login/callback":  service.handleLoginCallbackRequest,
	}
	for name, handler := range handlers {
		response := handler(request)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d (body=%s)",
				name, response.StatusCode, http.StatusBadRequest, response.Body)
		}
	}
}

// TestLoginCallbackKeepsAuthStatusContract 锁定 login 的错误形状。
//
// 页面（console.html）按 AuthLoginStatus 契约读 data.status / data.message，
// 通用的 {ok,error} 形状会让提示文案消失。
func TestLoginCallbackKeepsAuthStatusContract(t *testing.T) {
	response := newService(t).handleLoginCallbackRequest(pluginapi.ManagementRequest{
		Method: http.MethodPost, Body: []byte(malformedJSON),
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("body is not JSON: %v", errUnmarshal)
	}
	if payload["status"] != string(pluginapi.AuthLoginStatusError) {
		t.Fatalf("status = %v, want %q", payload["status"], pluginapi.AuthLoginStatusError)
	}
	if message, _ := payload["message"].(string); message == "" {
		t.Fatalf("message missing: %s", response.Body)
	}
}
