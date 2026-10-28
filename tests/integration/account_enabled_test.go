package tests

// 本文件锁定「批量启用/停用账号」的端到端行为。
//
// 停用不是删除：凭证文件与 token 都保留，只在 JSON 上写宿主的 disabled 标记，
// 宿主扫盘后该账号退出路由；启用删掉标记即恢复。批量端点因此必须能按
// account_ids 只作用于选中的账号。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"freetier2api-plugin/internal/core"
)

// accountEnabledResult 是 POST /accounts/enabled 的响应形状。
type accountEnabledResult struct {
	OK      bool `json:"ok"`
	Changed int  `json:"changed"`
	Results []struct {
		AuthID  string `json:"auth_id"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"results"`
}

// hostAuthByName 取假宿主里某个账号的当前状态。
func hostAuthByName(t *testing.T, host *fakeHost, name string) core.HostAuth {
	t.Helper()
	for _, entry := range host.SnapshotAuthFiles() {
		if entry.Name == name || entry.ID == name {
			return entry
		}
	}
	t.Fatalf("宿主里没有账号 %q", name)
	return core.HostAuth{}
}

// TestAccountEnableDisableWritesDisabledFlag 覆盖停用 → 启用整条链路：
// 停用写 disabled=true 并保留 token，启用把标记删掉。
func TestAccountEnableDisableWritesDisabledFlag(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	name := "traecn-toggle-1.json"
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, []byte(traeCredentialJSON), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	host.addAccount("1", name, traeCredentialJSON)
	host.SetAuthFilePath("1", path)

	response := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{name},
		"enabled":     false,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/enabled = %d: %s", response.StatusCode, response.Body)
	}
	var payload accountEnabledResult
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode: %v (%s)", errUnmarshal, response.Body)
	}
	if !payload.OK || payload.Changed != 1 {
		t.Fatalf("停用失败: %s", response.Body)
	}
	if !hostAuthByName(t, host, name).Disabled {
		t.Fatal("停用后宿主侧账号必须标记为 disabled")
	}

	// 凭证文件仍在原位，token 原样保留（停用不等于删除）。
	onDisk, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("停用不应删除凭证文件: %v", errRead)
	}
	var fields map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(onDisk, &fields); errUnmarshal != nil {
		t.Fatalf("凭证不再是合法 JSON: %v", errUnmarshal)
	}
	if string(fields["access_token"]) != `"test-access-token"` {
		t.Fatalf("停用不应改动 token，实际 = %s", fields["access_token"])
	}

	// 停用第二次：已是目标状态，不应重复写盘。
	repeat := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{name},
		"enabled":     false,
	})
	var repeatPayload accountEnabledResult
	if errUnmarshal := json.Unmarshal(repeat.Body, &repeatPayload); errUnmarshal != nil {
		t.Fatalf("decode repeat: %v", errUnmarshal)
	}
	if repeatPayload.Changed != 0 {
		t.Fatalf("已处于目标状态时不应重复写盘，changed = %d", repeatPayload.Changed)
	}

	enableResponse := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{name},
		"enabled":     true,
	})
	if enableResponse.StatusCode != http.StatusOK {
		t.Fatalf("启用 = %d: %s", enableResponse.StatusCode, enableResponse.Body)
	}
	if hostAuthByName(t, host, name).Disabled {
		t.Fatal("启用后宿主侧账号必须恢复可用")
	}
	onDisk, errRead = os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read after enable: %v", errRead)
	}
	if errUnmarshal := json.Unmarshal(onDisk, &fields); errUnmarshal != nil {
		t.Fatalf("enable payload: %v", errUnmarshal)
	}
	if _, exists := fields["disabled"]; exists {
		t.Fatalf("启用必须删掉 disabled 键，实际 = %s", onDisk)
	}
}

// TestAccountEnabledOnlyTouchesTargets 确认批量端点只改选中的账号。
func TestAccountEnabledOnlyTouchesTargets(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)

	dir := t.TempDir()
	for _, name := range []string{"traecn-batch-1.json", "traecn-batch-2.json"} {
		if errWrite := os.WriteFile(filepath.Join(dir, name), []byte(traeCredentialJSON), 0o600); errWrite != nil {
			t.Fatalf("write %s: %v", name, errWrite)
		}
	}
	host.addAccount("1", "traecn-batch-1.json", traeCredentialJSON)
	host.addAccount("2", "traecn-batch-2.json", traeCredentialJSON)
	host.SetAuthFilePath("1", filepath.Join(dir, "traecn-batch-1.json"))
	host.SetAuthFilePath("2", filepath.Join(dir, "traecn-batch-2.json"))

	response := managementCallWithBody(t, "POST", "/accounts/enabled", map[string]any{
		"account_ids": []string{"traecn-batch-1.json"},
		"enabled":     false,
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/enabled = %d: %s", response.StatusCode, response.Body)
	}
	if !hostAuthByName(t, host, "traecn-batch-1.json").Disabled {
		t.Fatal("选中的账号应被停用")
	}
	if hostAuthByName(t, host, "traecn-batch-2.json").Disabled {
		t.Fatal("未选中的账号不应被停用")
	}
}
