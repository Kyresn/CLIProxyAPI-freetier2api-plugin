package admin

// 本文件锁定「账号别名」管理入口的落库约定：键是账号身份（file_id），
// 不是昵称/邮箱。
//
// 昵称跨账号不唯一、上游改一次就变；拿它做键的话，别名会在重登后丢失，
// 或串到另一个同昵称的账号上。这里钉住两条：写入/清除都按 file_id 操作，
// 清除必须把键真正删掉（不留空串条目）。

import (
	"encoding/json"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

func accountAliasRequestBody(t *testing.T, fileID, alias string) pluginapi.ManagementRequest {
	t.Helper()
	body, errMarshal := json.Marshal(map[string]string{"file_id": fileID, "alias": alias})
	if errMarshal != nil {
		t.Fatalf("marshal alias request: %v", errMarshal)
	}
	return pluginapi.ManagementRequest{Method: http.MethodPost, Body: body}
}

func TestAccountAliasStoresTrimmedValue(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	response := service.handleAccountAliasRequest(accountAliasRequestBody(t, " workbuddycn-u1 ", " 工作号 "))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, response.Body)
	}
	aliases := deps.store.Snapshot().AccountAliases
	if len(aliases) != 1 || aliases["workbuddycn-u1"] != "工作号" {
		t.Fatalf("aliases = %#v, want key workbuddycn-u1 -> 工作号", aliases)
	}
}

func TestAccountAliasEmptyValueClearsKey(t *testing.T) {
	deps := newStubDeps(t)
	service := newServiceWithDeps(t, deps)

	service.handleAccountAliasRequest(accountAliasRequestBody(t, "workbuddycn-u1", "工作号"))
	response := service.handleAccountAliasRequest(accountAliasRequestBody(t, "workbuddycn-u1", "  "))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d: %s", response.StatusCode, response.Body)
	}
	if aliases := deps.store.Snapshot().AccountAliases; len(aliases) != 0 {
		t.Fatalf("clearing an alias must drop the key, got %#v", aliases)
	}
}

func TestAccountAliasRequiresAccountIdentity(t *testing.T) {
	service := newService(t)
	response := service.handleAccountAliasRequest(accountAliasRequestBody(t, "  ", "工作号"))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.StatusCode, http.StatusBadRequest, response.Body)
	}
}
