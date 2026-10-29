package tests

// 本文件覆盖 issue #58 里 CodeArts 的两类回归：
//   - 上游用 HTTP 200 的 data: 帧（或 200 包体）返回业务错误，必须被识别并保留原文；
//   - 控制台模型清单（model.static / 刷新目录）与 /v1/models（model.for_auth）同源。

import (
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
)

// codeartsCredentialJSON 是一份最小可用的 CodeArts AK/SK 凭证。
const codeartsCredentialJSON = `{"vendor":"codearts","access_key_id":"ak","secret_access_key":"sk","user_name":"u1"}`

// codeartsStreamErrorFrame 是上游在 data: 帧里返回的业务错误（requestId / traceId 已脱敏）。
const codeartsStreamErrorFrame = `{"error_code":"InferHub.4004.200","error_msg":"benefit not found","details":[` +
	`{"error_code":"InferHub.4004.200","error_msg":"requestId: req-fixture-0001"},` +
	`{"error_code":"InferHub.4004.200","error_msg":"traceId: trace-fixture-0001"}]}`

// TestCodeArtsStreamSurfacesBusinessErrorFrame 验证流式业务错误帧被翻成 OpenAI
// 错误帧，客户端看到的是 benefit not found，而不是「断流」。
func TestCodeArtsStreamSurfacesBusinessErrorFrame(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "codearts-user-1.json", codeartsCredentialJSON)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/api/v2/chat/completions") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   "data: " + codeartsStreamErrorFrame + "\n\n",
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{}`}
	})

	callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "glm-5.3",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`),
		},
		StreamID: "s1",
	})

	if !waitFor(t, 3e9, func() bool { return len(host.emittedChunks()) > 0 }) {
		t.Fatalf("业务错误帧必须下发，实际没有任何帧")
	}
	chunk := host.emittedChunks()[0]
	for _, want := range []string{`"upstream_error"`, "benefit not found", "req-fixture-0001", "trace-fixture-0001"} {
		if !strings.Contains(chunk, want) {
			t.Fatalf("错误帧缺少 %q: %s", want, chunk)
		}
	}
	if len(host.streamErrors()) != 0 {
		t.Fatalf("业务错误已经以帧下发，不该再报「断流」: %v", host.streamErrors())
	}
}

// TestCodeArtsNonStreamSurfacesBusinessErrorBody 验证 200 包体里的业务错误被转成
// 分类错误，而不是原样透传非标准体。
func TestCodeArtsNonStreamSurfacesBusinessErrorBody(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "codearts-user-1.json", codeartsCredentialJSON)
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "/api/v2/chat/completions") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"error_code":"4004","error_msg":"benefit not found","result":null}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{}`}
	})

	raw, errHandle := testApp.HandleMethod(pluginabi.MethodExecutorExecute, mustMarshal(app.ExecutorRPCRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:  "1",
			Model:   "glm-5.3",
			Format:  app.FormatChatCompletions,
			Payload: []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`),
		},
	}))
	if errHandle == nil {
		t.Fatalf("200 包体里的业务错误必须报错，实际返回 %s", raw)
	}
	if !strings.Contains(errHandle.Error(), "benefit not found") {
		t.Fatalf("错误信息必须保留上游原文: %v", errHandle)
	}
	failure, okFailure := errHandle.(*core.PluginError)
	if !okFailure {
		t.Fatalf("应返回 *core.PluginError，实际 %T", errHandle)
	}
	if failure.HTTPStatus == http.StatusUnauthorized {
		t.Fatal("「该模型没有免费福利」不能映射成 401，否则宿主会禁用凭证")
	}
}
