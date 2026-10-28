package admin

// 本文件锁定「启用/停用账号」：它写的是凭证 JSON 上的 disabled 标记，
// 宿主扫盘后按该字段决定是否参与路由。
//
// 关键约束：
//   - 只增删 disabled 一个键，其余字段必须逐字节语义等价（上游 uid 可能超过
//     2^53，走 map[string]any 往返会丢精度）；
//   - 已处于目标状态的账号不重复写盘——宿主每次写盘都会触发一次扫盘重载。

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

func TestSetCredentialDisabledKeepsOtherFields(t *testing.T) {
	original := []byte(`{"vendor":"workbuddycn","uid":"9007199254740993","label":"工作号","api_key":"sk-x"}`)

	disabled, errDisable := setCredentialDisabled(original, true)
	if errDisable != nil {
		t.Fatalf("disable: %v", errDisable)
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(disabled, &fields); errUnmarshal != nil {
		t.Fatalf("disabled payload is not json: %v", errUnmarshal)
	}
	if string(fields["disabled"]) != "true" {
		t.Fatalf("disabled = %s, want true", fields["disabled"])
	}
	// uid 是超过 2^53 的整数：走 map[string]any 会被 float64 改写成 ...992。
	if got := string(fields["uid"]); got != `"9007199254740993"` {
		t.Fatalf("uid 被改写：%s", got)
	}
	if string(fields["api_key"]) != `"sk-x"` {
		t.Fatalf("其余字段必须原样保留：%s", fields["api_key"])
	}

	enabled, errEnable := setCredentialDisabled(disabled, false)
	if errEnable != nil {
		t.Fatalf("enable: %v", errEnable)
	}
	// 用新 map 解码：json.Unmarshal 只会覆盖出现的键，复用旧 map 会残留 disabled。
	var enabledFields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(enabled, &enabledFields); errUnmarshal != nil {
		t.Fatalf("enabled payload is not json: %v", errUnmarshal)
	}
	if _, exists := enabledFields["disabled"]; exists {
		t.Fatalf("启用必须删掉 disabled 键，实际 = %s", enabled)
	}
	if string(enabledFields["uid"]) != `"9007199254740993"` {
		t.Fatalf("启用往返后 uid 被改写：%s", enabledFields["uid"])
	}
}

func TestSetCredentialDisabledRejectsNonObject(t *testing.T) {
	if _, errSet := setCredentialDisabled([]byte(`"not-an-object"`), true); errSet == nil {
		t.Fatal("非对象凭证必须被拒绝，而不是写回一份坏文件")
	}
}

// TestAccountEnabledRequestRejectsEmptyTargets 锁定端点契约：没有目标时不猜「全部」，
// 直接 400——批量停用全部账号是破坏性操作，必须由页面显式列出目标。
func TestAccountEnabledRequestRejectsEmptyTargets(t *testing.T) {
	service := newService(t)
	body, errMarshal := json.Marshal(map[string]any{"account_ids": []string{}, "enabled": false})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	response := service.handleAccountEnabledRequest(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Body:   body,
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.StatusCode, http.StatusBadRequest, response.Body)
	}
}
