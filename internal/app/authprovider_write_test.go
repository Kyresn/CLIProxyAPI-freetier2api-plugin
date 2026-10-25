package app

// 续期凭证写回的回归：宿主热更新与落盘必须是同一份内容，且 uid 与文件名同拼写
// （Windows 上宿主会按文件名与 uid 各登记一次，大小写不同就是两条账号）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// writeStubVendor 是只满足 core.Vendor 的最小实现；mergeStorageJSON 非 nil 时
// 额外实现 core.StorageMerger。
type writeStubVendor struct {
	mergeStorageJSON func(original []byte) ([]byte, error)
}

func (v *writeStubVendor) ID() string          { return "zcode" }
func (v *writeStubVendor) Name() string        { return "ZCode" }
func (v *writeStubVendor) Region() string      { return "global" }
func (v *writeStubVendor) ModelPrefix() string { return "" }

func (v *writeStubVendor) Match(string, string, map[string]any) bool { return true }

func (v *writeStubVendor) Parse([]byte, string) (*core.Credential, error) {
	return nil, fmt.Errorf("not used in this test")
}

func (v *writeStubVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}

func (v *writeStubVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, fmt.Errorf("not used in this test")
}

func (v *writeStubVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return fmt.Errorf("not used in this test")
}

// writeTestVendor 只在 mergeStorageJSON 非 nil 时才满足 core.StorageMerger。
type writeTestVendor struct{ *writeStubVendor }

func (v *writeTestVendor) MergeStorageJSON(original []byte, _ *core.Credential) ([]byte, error) {
	return v.mergeStorageJSON(original)
}

const (
	writeTestFile = "zcode-eyJh---1KC8.json"
	writeTestRaw  = "{\"vendor\":\"zcode\",\"uid\":\"zcode-eyjh---1kc8\",\"jwt_token\":\"t\"}"
)

// TestSaveRefreshedCredentialAlignsHostAndDisk 覆盖写回的三条路径：
// 合并成功、合并失败（非 StorageMerger）、宿主保存失败。
func TestSaveRefreshedCredentialAlignsHostAndDisk(t *testing.T) {
	merging := func() *writeTestVendor {
		return &writeTestVendor{writeStubVendor: &writeStubVendor{
			mergeStorageJSON: func(original []byte) ([]byte, error) { return original, nil },
		}}
	}
	cases := []struct {
		name         string
		vendor       core.Vendor
		hostFails    bool
		wantHostSave bool
	}{
		{name: "合并成功", vendor: merging(), wantHostSave: true},
		// 没有 StorageMerger 时凭证没有变化，不该惊动宿主；但落盘内容仍要归一。
		{name: "供应商不支持合并", vendor: &writeStubVendor{}},
		{name: "宿主保存失败", vendor: merging(), hostFails: true, wantHostSave: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, writeTestFile)
			if errWrite := os.WriteFile(path, []byte(writeTestRaw), 0o600); errWrite != nil {
				t.Fatalf("seed credential: %v", errWrite)
			}

			var handed pluginapi.HostAuthSaveRequest
			saveCalls := 0
			hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
				if method != pluginabi.MethodHostAuthSave {
					return nil, fmt.Errorf("unexpected host method %s", method)
				}
				saveCalls++
				raw, errMarshal := json.Marshal(payload)
				if errMarshal != nil {
					return nil, errMarshal
				}
				if errUnmarshal := json.Unmarshal(raw, &handed); errUnmarshal != nil {
					return nil, errUnmarshal
				}
				if testCase.hostFails {
					return nil, fmt.Errorf("host rejected the save")
				}
				return json.RawMessage("{}"), nil
			})
			t.Cleanup(func() { hostbridge.Configure(nil) })

			app := &App{}
			app.saveRefreshedCredential(context.Background(), "cb-1", testCase.vendor,
				core.HostAuth{Name: writeTestFile, Path: path}, []byte(writeTestRaw), nil, path)

			disk, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatalf("read credential: %v", errRead)
			}
			var fields map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(disk, &fields); errUnmarshal != nil {
				t.Fatalf("disk payload is not json: %v", errUnmarshal)
			}
			var uid string
			if errUnmarshal := json.Unmarshal(fields["uid"], &uid); errUnmarshal != nil {
				t.Fatalf("uid: %v", errUnmarshal)
			}
			if uid != "zcode-eyJh---1KC8" {
				t.Fatalf("disk uid = %q，want 与文件名一致", uid)
			}
			if _, errStat := os.Stat(path + ".tmp"); !os.IsNotExist(errStat) {
				t.Fatalf("临时文件未清理：%v", errStat)
			}
			wantSaveCalls := 0
			if testCase.wantHostSave {
				wantSaveCalls = 1
			}
			if saveCalls != wantSaveCalls {
				t.Fatalf("host.auth.save 调用次数 = %d，want %d", saveCalls, wantSaveCalls)
			}
			if !testCase.wantHostSave {
				return
			}
			// 宿主与磁盘必须是同一份内容（ABI 编码会把 RawMessage 压成单行，
			// 所以比语义而不是比字节）。
			if !sameJSON(t, handed.JSON, disk) {
				t.Fatalf("host = %s\ndisk = %s", handed.JSON, disk)
			}
			if handed.Name != writeTestFile {
				t.Fatalf("host name = %q", handed.Name)
			}
		})
	}
}

// sameJSON 比较两份 JSON 的语义（宿主编码会改写空白与键序）。
func sameJSON(t *testing.T, left, right []byte) bool {
	t.Helper()
	var leftValue, rightValue any
	if errUnmarshal := json.Unmarshal(left, &leftValue); errUnmarshal != nil {
		t.Fatalf("left is not json: %v", errUnmarshal)
	}
	if errUnmarshal := json.Unmarshal(right, &rightValue); errUnmarshal != nil {
		t.Fatalf("right is not json: %v", errUnmarshal)
	}
	leftJSON, _ := json.Marshal(leftValue)
	rightJSON, _ := json.Marshal(rightValue)
	return string(leftJSON) == string(rightJSON)
}
