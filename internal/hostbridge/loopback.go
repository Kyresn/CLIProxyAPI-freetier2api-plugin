package hostbridge

// 本文件提供把宿主 HTTP 桥接到真实网络的 loopback 实现。
//
// 用途：
//   - 供应商单测需要验证出站请求形态时，不必各自复制一份宿主桩；
//   - 本地探针 / 排障时可直接复用同一套转发逻辑。
//
// 它只实现 host.http.do（一次性响应）；流式桥接仍由宿主负责。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginabi"
)

// ConfigureLoopback 把 host.http.do 指向真实 HTTP 客户端。
//
// 调用方负责在测试结束时恢复：hostbridge.Configure(nil)。
func ConfigureLoopback() {
	Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, nil
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var request rpcHostHTTPRequest
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		var bodyReader io.Reader
		if len(request.Body) > 0 {
			bodyReader = bytes.NewReader(request.Body)
		}
		httpRequest, errNew := http.NewRequest(request.Method, request.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for key, values := range request.Headers {
			for _, value := range values {
				httpRequest.Header.Add(key, value)
			}
		}
		response, errDo := http.DefaultClient.Do(httpRequest)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = response.Body.Close() }()
		body, errRead := io.ReadAll(response.Body)
		if errRead != nil {
			return nil, errRead
		}
		return json.Marshal(map[string]any{
			"StatusCode": response.StatusCode,
			"Headers":    response.Header,
			"Body":       body,
		})
	})
}
