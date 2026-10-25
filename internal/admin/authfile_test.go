package admin

import (
	"encoding/json"
	"fmt"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/hostbridge"
)

// TestSaveAuthFileAlignsUID 锁定「添加账号 / 粘贴回调」这条写入口也会归一 uid。
//
// 宿主在 Windows 上按文件名与 uid 各登记一次，两条只差大小写就会变成两个账号。
// 归一收口在 hostbridge.SaveAuth，admin 不应自己拼 host.auth.save 请求。
func TestSaveAuthFileAlignsUID(t *testing.T) {
	var handed pluginapi.HostAuthSaveRequest
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthSave {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		if errUnmarshal := json.Unmarshal(raw, &handed); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return json.RawMessage("{}"), nil
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })

	raw := []byte("{\"vendor\":\"zcode\",\"uid\":\"zcode-eyjh---1kc8\",\"jwt_token\":\"t\"}")
	if errSave := saveAuthFile("cb-1", "zcode-eyJh---1KC8.json", raw); errSave != nil {
		t.Fatalf("saveAuthFile: %v", errSave)
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(handed.JSON, &fields); errUnmarshal != nil {
		t.Fatalf("saved payload is not json: %v", errUnmarshal)
	}
	var uid string
	if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
		t.Fatalf("uid: %v", errUnmarshal)
	}
	if uid != "zcode-eyJh---1KC8" {
		t.Fatalf("uid = %q，want 与文件名一致", uid)
	}
	var vendor string
	if errUnmarshal := json.Unmarshal(fields["vendor"], &vendor); errUnmarshal != nil || vendor != "zcode" {
		t.Fatalf("vendor = %q (%v)，其余字段必须原样保留", vendor, errUnmarshal)
	}
}
