// Package hostbridgetest 提供测试用的宿主 HTTP 桥替身。
//
// 只允许在 *_test.go 里引用：它把 host.http.do 换成直连 httptest 的转发器，
// 让供应商包的单测能校验真实出站请求（方法 / URL / 头 / 体）与真实响应解码，
// 又不依赖宿主进程。此前每个供应商各自抄了一份，假响应键名与请求体处理各不
// 相同，测试与线上宿主的实际形态就此脱节。
package hostbridgetest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/hostbridge"
)

// UseLoopbackHost 把宿主 HTTP 桥换成直连 httptest 的转发器。
//
// 只接管 host.http.do；其余方法（流式读取等）原样返回空响应，由各自的用例
// 自行配置桥接。
func UseLoopbackHost(t testing.TB) {
	t.Helper()
	hostbridge.Configure(func(_, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, nil
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var request struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		if errDecode := json.Unmarshal(rawPayload, &request); errDecode != nil {
			return nil, errDecode
		}

		var bodyReader io.Reader
		if len(request.Body) > 0 {
			bodyReader = strings.NewReader(string(request.Body))
		}
		httpReq, errNew := http.NewRequest(request.Method, request.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for key, values := range request.Headers {
			for _, value := range values {
				httpReq.Header.Add(key, value)
			}
		}

		resp, errDo := (&http.Client{Timeout: 10 * time.Second}).Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		// 用宿主线上形态的键名（StatusCode / Headers / Body）：hostbridge 两种
		// 形态都认，替身固定最贴近真实宿主的一种。
		return json.Marshal(map[string]any{
			"StatusCode": resp.StatusCode,
			"Headers":    resp.Header,
			"Body":       respBody,
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}
