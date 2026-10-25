package tests

// uid 大小写归一的跨层回归：宿主解析（auth.parse）回给宿主的 StorageJSON
// 必须与文件名同拼写，否则 Windows 上会多登记一条账号。

import (
	"encoding/json"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

func TestAuthParseAlignsUIDWithFileName(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	result := callMethod(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		FileName: "zcode-eyJh---1KC8.json",
		RawJSON:  []byte("{\"vendor\":\"zcode\",\"uid\":\"zcode-eyjh---1kc8\",\"jwt_token\":\"t\",\"api_key\":\"k\"}"),
	})
	var response pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
		t.Fatalf("decode parse response: %v", errUnmarshal)
	}
	if !response.Handled {
		t.Fatalf("zcode credential must be handled: %s", string(result))
	}

	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(response.Auth.StorageJSON, &fields); errUnmarshal != nil {
		t.Fatalf("storage json: %v", errUnmarshal)
	}
	var uid string
	if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
		t.Fatalf("uid: %v", errUnmarshal)
	}
	if uid != "zcode-eyJh---1KC8" {
		t.Fatalf("uid = %q，want 文件名拼写（否则宿主会多登记一条）", uid)
	}
	// 其余字段原样保留，且 ID 仍由文件名派生。
	var token string
	if errUnmarshal := json.Unmarshal(fields["jwt_token"], &token); errUnmarshal != nil || token != "t" {
		t.Fatalf("jwt_token = %q (%v)", token, errUnmarshal)
	}
	if response.Auth.ID != "zcode-eyJh---1KC8" {
		t.Fatalf("id = %q", response.Auth.ID)
	}
}
