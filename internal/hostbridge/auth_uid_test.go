package hostbridge

// uid 大小写归一（Windows 上宿主按文件名与 uid 各登记一次）的单元测试。

import (
	"encoding/json"
	"fmt"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// TestAlignCredentialUIDRewritesCaseVariant 验证「大小写不敏感相等」的 uid
// 被改写成文件名拼写，且其余字段一个字节都不动。
func TestAlignCredentialUIDRewritesCaseVariant(t *testing.T) {
	raw := []byte("{\n  \"uid\": \"zcode-eyjh---1kc8\",\n  \"jwt_token\": \"t\",\n  \"expires_at\": 9007199254740993,\n  \"note\": \"a<b&c\"\n}")
	aligned := AlignCredentialUID(raw, "zcode-eyJh---1KC8.json")

	var fields map[string]json.RawMessage
	if errParse := json.Unmarshal(aligned, &fields); errParse != nil {
		t.Fatalf("aligned payload is not json: %v", errParse)
	}
	var uid string
	if errParse := json.Unmarshal(fields["uid"], &uid); errParse != nil {
		t.Fatalf("decode uid: %v", errParse)
	}
	if uid != "zcode-eyJh---1KC8" {
		t.Fatalf("uid = %q，wanted 文件名拼写", uid)
	}
	// 只替换 uid：数值不因 float64 往返丢精度，转义也不被重写。
	if got := string(fields["expires_at"]); got != "9007199254740993" {
		t.Fatalf("expires_at = %s，数值被改写", got)
	}
	if got := string(fields["note"]); got != "\"a<b&c\"" {
		t.Fatalf("note = %s，转义被改写", got)
	}
	var token string
	if errParse := json.Unmarshal(fields["jwt_token"], &token); errParse != nil || token != "t" {
		t.Fatalf("jwt_token = %v (%v)", token, errParse)
	}
}

// TestAlignCredentialUIDLeavesOthersAlone 验证不该改的输入原样返回。
func TestAlignCredentialUIDLeavesOthersAlone(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		file    string
	}{
		{"已一致", "{\"uid\":\"zcode-AbC\"}", "zcode-AbC.json"},
		{"供应商自有 uid", "{\"uid\":\"a707ecea-6211-4e4e\"}", "workbuddycn-A707ECEA.json"},
		{"无 uid 字段", "{\"jwt_token\":\"t\"}", "zcode-AbC.json"},
		{"uid 非字符串", "{\"uid\":123}", "zcode-123.json"},
		{"非 JSON", "not json", "x.json"},
		{"JSON 数组", "[\"uid\"]", "x.json"},
		{"文件名为空", "{\"uid\":\"ZCODE-abc\"}", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := AlignCredentialUID([]byte(testCase.payload), testCase.file)
			if string(got) != testCase.payload {
				t.Fatalf("payload must pass through, got %s", got)
			}
		})
	}
}

// TestAlignCredentialUIDAcceptsUppercaseExtension 锁定 .JSON 扩展名同样剥离。
func TestAlignCredentialUIDAcceptsUppercaseExtension(t *testing.T) {
	aligned := AlignCredentialUID([]byte("{\"uid\":\"qodercn-abcd\"}"), "qodercn-ABCD.JSON")
	var fields map[string]json.RawMessage
	if errParse := json.Unmarshal(aligned, &fields); errParse != nil {
		t.Fatalf("aligned payload is not json: %v", errParse)
	}
	var uid string
	if errParse := json.Unmarshal(fields["uid"], &uid); errParse != nil {
		t.Fatalf("decode uid: %v", errParse)
	}
	if uid != "qodercn-ABCD" {
		t.Fatalf("uid = %q", uid)
	}
}

// TestSaveAuthAlignsBeforeHandoff 锁定「交给宿主的 uid 已归一」：写回只有
// hostbridge.SaveAuth 一条路径，宿主拿到的 JSON 与调用方落盘的是同一份。
func TestSaveAuthAlignsBeforeHandoff(t *testing.T) {
	var handed pluginapi.HostAuthSaveRequest
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
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
	t.Cleanup(func() { Configure(nil) })

	payload, errSave := SaveAuth("cb-1", "zcode-eyJh---1KC8.json", []byte("{\"uid\":\"zcode-eyjh---1kc8\"}"))
	if errSave != nil {
		t.Fatalf("SaveAuth: %v", errSave)
	}
	if handed.Name != "zcode-eyJh---1KC8.json" {
		t.Fatalf("name = %q", handed.Name)
	}
	for name, raw := range map[string][]byte{"host": handed.JSON, "returned": payload} {
		var fields map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(raw, &fields); errUnmarshal != nil {
			t.Fatalf("%s payload is not json: %v", name, errUnmarshal)
		}
		var uid string
		if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
			t.Fatalf("%s uid: %v", name, errUnmarshal)
		}
		if uid != "zcode-eyJh---1KC8" {
			t.Fatalf("%s uid = %q，want 文件名拼写", name, uid)
		}
	}
}
