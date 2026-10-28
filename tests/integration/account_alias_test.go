package tests

// 本文件锁定「账号别名」的端到端行为：别名优先展示、按账号身份落库、
// 上游凭证变化后仍然保留。
//
// 真实痛点：部分平台（WorkBuddy/Trae 等）只回一个不可修改的默认昵称，
// 多账号时概览里根本认不出谁是谁；而昵称跨账号并不唯一，不能拿去当别名的键。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// aliasAccount 只保留别名相关的字段。
type aliasAccount struct {
	FileID      string `json:"file_id"`
	Label       string `json:"label"`
	NativeLabel string `json:"native_label"`
	Alias       string `json:"alias"`
}

func aliasAccountsByFileID(t *testing.T) map[string]aliasAccount {
	t.Helper()
	resp := managementCall(t, "GET", "/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/status = %d: %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Accounts []aliasAccount `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status: %v (%s)", errUnmarshal, resp.Body)
	}
	out := make(map[string]aliasAccount, len(payload.Accounts))
	for _, account := range payload.Accounts {
		out[account.FileID] = account
	}
	return out
}

func setAccountAlias(t *testing.T, fileID, alias string) {
	t.Helper()
	resp := managementCallWithBody(t, "POST", "/accounts/alias", map[string]string{"file_id": fileID, "alias": alias})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/accounts/alias %q = %d: %s", fileID, resp.StatusCode, resp.Body)
	}
}

// traecnSharedCredential 造一份昵称固定为 shared-account 的 Trae 凭证。
func traecnSharedCredential(accessToken string) string {
	return fmt.Sprintf(`{"vendor":"traecn","region":"cn","access_token":%q,"uid":"2487573687317868","label":"shared-account"}`, accessToken)
}

// TestAccountAliasWinsDisplayAndIsolatesAccounts 覆盖别名的主链路：
// 上游昵称与同名账号互不影响、别名优先展示、token 轮换后保留、清空即恢复。
func TestAccountAliasWinsDisplayAndIsolatesAccounts(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "traecn-shared.json", traecnSharedCredential("test-access-token"))
	host.addAccount("2", "opencodezen-shared.json", `{"api_key":"sk-test-opencode","label":"shared-account"}`)

	// 未设别名：行为与引入别名之前一致——两个账号都显示各自的上游昵称。
	baseline := aliasAccountsByFileID(t)
	for _, fileID := range []string{"traecn-shared", "opencodezen-shared"} {
		account, ok := baseline[fileID]
		if !ok {
			t.Fatalf("missing account %q in /status: %+v", fileID, baseline)
		}
		if account.Label != "shared-account" || account.Alias != "" {
			t.Fatalf("%s baseline = %+v, want upstream nickname and no alias", fileID, account)
		}
	}

	setAccountAlias(t, "traecn-shared", "工作号")
	afterSet := aliasAccountsByFileID(t)
	if got := afterSet["traecn-shared"]; got.Label != "工作号" || got.Alias != "工作号" {
		t.Fatalf("alias must win the display chain: %+v", got)
	}
	// 键是账号身份而非昵称：同昵称的另一个账号不能被顺带改名。
	if got := afterSet["opencodezen-shared"]; got.Label != "shared-account" || got.Alias != "" {
		t.Fatalf("same-nickname account must keep its own name: %+v", got)
	}

	// token 刷新 / 重登写回同一凭证文件：文件名没变，别名必须保留。
	host.SetAccountCredential("1", traecnSharedCredential("rotated-access-token"))
	if got := aliasAccountsByFileID(t)["traecn-shared"]; got.Label != "工作号" || got.Alias != "工作号" {
		t.Fatalf("alias must survive token rotation: %+v", got)
	}

	// 清空别名恢复上游名称。
	setAccountAlias(t, "traecn-shared", "")
	if got := aliasAccountsByFileID(t)["traecn-shared"]; got.Label != "shared-account" || got.Alias != "" {
		t.Fatalf("clearing an alias must restore the upstream name: %+v", got)
	}
}

// TestAccountNativeLabelStaysUpstream 锁定 native_label 的语义：它始终是上游名称，
// 不随别名变化——页面靠它判断「填回原名 = 不设别名」。
func TestAccountNativeLabelStaysUpstream(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "traecn-native.json", traecnSharedCredential("test-access-token"))

	setAccountAlias(t, "traecn-native", "工作号")
	account := aliasAccountsByFileID(t)["traecn-native"]
	if account.Label != "工作号" || account.Alias != "工作号" {
		t.Fatalf("aliased account = %+v, want label 工作号", account)
	}
	if account.NativeLabel != "shared-account" {
		t.Fatalf("native_label = %q, want 上游名称 shared-account", account.NativeLabel)
	}
}
