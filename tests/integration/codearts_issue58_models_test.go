package tests

// 本文件覆盖 issue #58 的模型目录一致性：
// 控制台清单（model.static / 刷新目录）必须与 /v1/models（model.for_auth）同源。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/vendors/codearts"
)

// TestCodeArtsStaticAndAuthModelsStayInSync 验证控制台目录与 /v1/models 同源。
//
// issue #58：StaticModels 不传凭证只拿内置目录（6 个），ModelsForAuth 加上福利
// 目录（11 个），于是面板刷新一次模型就少 5 个。两条路径都必须带凭证探测。
func TestCodeArtsStaticAndAuthModelsStayInSync(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "codearts-user-1.json", codeartsCredentialJSON)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		switch {
		case strings.Contains(url, codearts.EpModelBuiltin):
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body: `{"count":2,"builtinModels":[{"model_id":"GLM-5.2","model_name":"GLM-5.2"},` +
					`{"model_id":"kimi-k2.6-VL","model_name":"kimi-k2.6-VL"}]}`,
			}
		case strings.Contains(url, codearts.EpBenefitConfig):
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body: `{"error_code":"0000","result":{"models":[{"model_id":"kimi-k2.6-vl","model_name":"kimi-k2.6-vl"},` +
					`{"model_id":"glm-5.3","model_name":"glm-5.3"}]}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{}`}
	})

	authRaw := callMethod(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{AuthID: "1"})
	var authResp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(authRaw, &authResp); errUnmarshal != nil {
		t.Fatalf("decode model.for_auth: %v", errUnmarshal)
	}
	staticRaw := callMethod(t, pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{})
	var staticResp pluginapi.ModelResponse
	if errUnmarshal := json.Unmarshal(staticRaw, &staticResp); errUnmarshal != nil {
		t.Fatalf("decode model.static: %v", errUnmarshal)
	}

	authIDs := modelIDSet(t, authResp.Models)
	staticIDs := modelIDSet(t, staticResp.Models)
	if len(authIDs) != 3 || len(staticIDs) != 3 {
		t.Fatalf("两条路径都应返回 2 内置 + 1 福利（大小写重复已合并）: auth=%v static=%v", authIDs, staticIDs)
	}
	for id := range authIDs {
		if !staticIDs[id] {
			t.Fatalf("model.static 缺少 %q（控制台会少模型）: static=%v", id, staticIDs)
		}
	}
	if !authIDs["glm-5.3"] {
		t.Fatalf("福利模型必须进 /v1/models: %v", authIDs)
	}
	if authIDs["kimi-k2.6-vl"] {
		t.Fatalf("大小写重复必须合并成内置目录的写法: %v", authIDs)
	}
}

func modelIDSet(t *testing.T, models []pluginapi.ModelInfo) map[string]bool {
	t.Helper()
	set := make(map[string]bool, len(models))
	for _, model := range models {
		set[model.ID] = true
	}
	return set
}
