package codearts

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// streamErrorFrame 是 issue #58 里上游用 HTTP 200 的 data: 帧返回的业务错误。
// 只把 requestId / traceId 换成了合成值，结构与上游原文一致。
const streamErrorFrame = `{"error_code":"InferHub.4004.200","error_msg":"benefit not found","details":[` +
	`{"error_code":"InferHub.4004.200","error_msg":"requestId: req-fixture-0001"},` +
	`{"error_code":"InferHub.4004.200","error_msg":"timestamps: 202610291647"},` +
	`{"error_code":"InferHub.4004.200","error_msg":"modelId: codearts-glm-5.3"},` +
	`{"error_code":"InferHub.4004.200","error_msg":"traceId: trace-fixture-0001"}]}`

// TestErrorFromPayloadKeepsUpstreamTrace 锁定 200 包体/帧里业务错误的识别与原文保留。
//
// 上游把 requestId / traceId 埋在 details[] 里：丢掉它们，用户就没有任何可反馈
// 给上游的线索；而分类若落到 credential，宿主会直接禁用凭证。
func TestErrorFromPayloadKeepsUpstreamTrace(t *testing.T) {
	failure, okFailure := ErrorFromPayload([]byte(streamErrorFrame))
	if !okFailure {
		t.Fatal("error_code/error_msg 信封必须被识别为业务错误")
	}
	if failure.Kind != core.FailureKindModelBlocked {
		t.Fatalf("kind = %s, want model_blocked（换模型/换号即可，不能判成凭证失效）", failure.Kind)
	}
	if failure.Code != "InferHub.4004.200" {
		t.Fatalf("code = %q, want InferHub.4004.200", failure.Code)
	}
	for _, want := range []string{"benefit not found", "requestId=req-fixture-0001",
		"traceId=trace-fixture-0001", "code=InferHub.4004.200", benefitHint} {
		if !strings.Contains(failure.Msg, want) {
			t.Fatalf("message 缺少 %q: %s", want, failure.Msg)
		}
	}
}

// TestErrorFromPayloadIgnoresNormalFrames 验证合法帧不会被误判成错误。
func TestErrorFromPayloadIgnoresNormalFrames(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"message":"host generated error","type":"server_error"}}`,
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"error_code":"","error_msg":""}`,
		`not json`,
	} {
		if _, okFailure := ErrorFromPayload([]byte(payload)); okFailure {
			t.Fatalf("不该识别为业务错误: %s", payload)
		}
	}
}

// TestClassifyHTTPEnvelopeKeepsTrace 验证 HTTP 错误体同样按信封解析。
func TestClassifyHTTPEnvelopeKeepsTrace(t *testing.T) {
	failure := Classify(http.StatusForbidden, streamErrorFrame)
	if failure.Kind != core.FailureKindCredential {
		t.Fatalf("kind = %s, want credential（HTTP 403 的处置沿用原规则）", failure.Kind)
	}
	if !strings.Contains(failure.Msg, "requestId=req-fixture-0001") {
		t.Fatalf("HTTP 错误体也要保留 requestId: %s", failure.Msg)
	}
}

// TestClassifyEmptyBodyIsReadable 验证上游 500 无正文时也有可读原因。
func TestClassifyEmptyBodyIsReadable(t *testing.T) {
	failure := Classify(http.StatusInternalServerError, "")
	if failure.Kind != core.FailureKindTransient || failure.Msg != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("failure = %+v, want transient + readable status text", failure)
	}
}

// TestOpenAIErrorFrame 锁定错误帧形态：宿主只认 {"error":{"message":...}}。
func TestOpenAIErrorFrame(t *testing.T) {
	failure, okFailure := ErrorFromPayload([]byte(streamErrorFrame))
	if !okFailure {
		t.Fatal("fixture 必须是可识别的业务错误")
	}
	frame, errFrame := OpenAIErrorFrame(failure)
	if errFrame != nil {
		t.Fatalf("marshal error frame: %v", errFrame)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(frame, &payload); errUnmarshal != nil {
		t.Fatalf("decode error frame: %v (%s)", errUnmarshal, frame)
	}
	if !strings.Contains(payload.Error.Message, "benefit not found") {
		t.Fatalf("message 必须保留上游原文: %s", payload.Error.Message)
	}
	if payload.Error.Type != "upstream_error" || payload.Error.Code != "InferHub.4004.200" {
		t.Fatalf("error frame = %+v", payload.Error)
	}
}

// TestClassifySnapErrors 锁住 snap 网关错误的分类：正文原因要保留，STS 过期
// 必须归成可续期的凭证错误（状态 401），宿主的保活重试才会触发。
func TestClassifySnapErrors(t *testing.T) {
	signature := Classify(400, `{"error_code":"APIG.0301","error_msg":"verify ak sk signature fail"}`)
	if !strings.Contains(signature.Msg, "APIG.0301") || !strings.Contains(signature.Msg, "signature fail") {
		t.Fatalf("签名错误正文丢失: %q", signature.Msg)
	}

	expired := Classify(400, `{"error_code":"APIG.0602","error_msg":"Bad request: the security token has expired"}`)
	if expired.Kind != core.FailureKindCredential || expired.HTTPStatus() != http.StatusUnauthorized {
		t.Fatalf("STS 过期应归为 401 凭证错误: kind=%v status=%d", expired.Kind, expired.HTTPStatus())
	}
}
