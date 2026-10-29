package codearts

// 本文件实现华为云 CodeArts 的上游错误分类。
//
// 上游的错误有两个落点，两边都要认：
//   - HTTP 4xx/5xx 的响应体；
//   - HTTP 200 响应体 / SSE `data:` 帧里的业务错误信封（error_code + error_msg）。
//     后一种不识别就会被当成「流里没有 finish_reason」，被插件的通用错误覆盖，
//     用户看不到 benefit not found 这类可操作原因（issue #58）。

import (
	"encoding/json"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
)

const (
	// benefitCodeMarker 是「账号对该模型没有免费福利配额」的 InferHub 错误码前缀。
	benefitCodeMarker = "InferHub.4004.200"
	benefitMsgMarker  = "benefit not found"
	// benefitHint 是给用户的可操作提示，附在上游原文之后而不是替换它。
	benefitHint = "未领取免费福利模型配额，请在管理端领取或开启自动领取"
)

// errorEnvelope 是 CodeArts / InferHub 的业务错误信封。
//
// 与 OpenAI 的 {"error":{...}} 不是一回事：后者是已经翻译好的错误帧，
// 只有前者带 error_code / error_msg。
type errorEnvelope struct {
	ErrorCode    string          `json:"error_code"`
	ErrorMsg     string          `json:"error_msg"`
	ErrorMessage string          `json:"error_message"`
	Message      string          `json:"message"`
	Error        json.RawMessage `json:"error"`
	Details      []errorDetail   `json:"details"`
}

// errorDetail 是 details[] 的一条：上游把 requestId / traceId 塞在这里。
type errorDetail struct {
	ErrorCode string `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
}

// carriesError 报告信封里是否真的带业务错误。
func (e errorEnvelope) carriesError() bool {
	return strings.TrimSpace(e.ErrorCode) != "" || strings.TrimSpace(e.ErrorMsg) != ""
}

// errorField 返回 error 字段的字符串形态（有的接口把错误文案放在这里）。
func (e errorEnvelope) errorField() string {
	var text string
	if len(e.Error) > 0 && json.Unmarshal(e.Error, &text) == nil {
		return strings.TrimSpace(text)
	}
	return ""
}

// message 拼出给用户看的正文：上游原文 + requestId / traceId。
//
// 上游把定位信息放在 details[] 里（"requestId: xxx" / "traceId: xxx"），丢掉它们
// 等于把用户唯一能反馈给上游的线索丢掉。
func (e errorEnvelope) message() string {
	text := strings.TrimSpace(core.FirstNonEmpty(e.ErrorMsg, e.ErrorMessage, e.Message))
	trace := make([]string, 0, 3)
	if code := strings.TrimSpace(e.ErrorCode); code != "" {
		trace = append(trace, "code="+code)
	}
	for _, detail := range e.Details {
		value := strings.TrimSpace(detail.ErrorMsg)
		index := strings.Index(value, ":")
		if index <= 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value[:index])) {
		case "requestid", "traceid":
			trace = append(trace, strings.TrimSpace(value[:index])+"="+strings.TrimSpace(value[index+1:]))
		}
	}
	if len(trace) > 0 {
		if text != "" {
			text += " "
		}
		text += "(" + strings.Join(trace, " ") + ")"
	}
	return text
}

// Classify 把 CodeArts 的上游响应分类成共享的上游错误。
func Classify(status int, body string) *core.Failure {
	var envelope errorEnvelope
	_ = json.Unmarshal([]byte(body), &envelope)

	code := core.FirstNonEmpty(envelope.ErrorCode, envelope.errorField())
	msg := core.FirstNonEmpty(envelope.ErrorMsg, envelope.ErrorMessage, envelope.Message,
		envelope.errorField(), body)
	if envelope.carriesError() {
		// 信封里的正文更精确：error_msg 原文 + details 里的 requestId / traceId。
		msg = envelope.message()
	}
	if strings.TrimSpace(msg) == "" {
		// 上游 500 有时连 body 都没有：空正文会让用户看到一条没有任何信息的错误。
		msg = http.StatusText(status)
	}
	return classifyFailure(status, code, msg, false)
}

// ErrorFromPayload 识别 HTTP 200 响应体 / SSE data: 帧里的业务错误信封。
//
// 返回的 Failure 不带 Status：这条错误发生在「传输成功」的响应里，状态码由
// 分类推导（见 core.Failure.HTTPStatus），而不是照抄上游的 200。
func ErrorFromPayload(payload []byte) (*core.Failure, bool) {
	var envelope errorEnvelope
	if errUnmarshal := json.Unmarshal(payload, &envelope); errUnmarshal != nil {
		return nil, false
	}
	if !envelope.carriesError() {
		return nil, false
	}
	return classifyFailure(0, strings.TrimSpace(envelope.ErrorCode), envelope.message(), true), true
}

// OpenAIErrorFrame 把业务错误翻成 OpenAI 错误帧。
//
// 宿主只认 {"error":{"message":...}} 形态的终止帧（见宿主 chunkTerminalError）：
// 认不出上游帧时它会用「流提前结束」覆盖真实原因，这正是 issue #58 的现象。
func OpenAIErrorFrame(failure *core.Failure) ([]byte, error) {
	return json.Marshal(map[string]any{
		"error": map[string]any{
			"message": failure.Msg,
			"type":    "upstream_error",
			"code":    failure.Code,
		},
	})
}

// classifyFailure 归纳 (status, code, msg) → Failure。
//
// payloadErr 表示错误出现在 200 包体/帧里（status 传 0）：这类错误的处置要按
// 「这条请求/这个模型不可用」来判，不能照搬 HTTP 状态码规则。
func classifyFailure(status int, code, msg string, payloadErr bool) *core.Failure {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		strings.Contains(code, "ExpiredToken") || strings.Contains(code, "InvalidToken"):
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: msg}
	case isBenefitMissing(code, msg):
		if payloadErr {
			// 该账号对这个模型没有免费福利：换模型或换账号即可，凭证本身没坏。
			// 归到 model_blocked（可重试、404）而不是 credential——401 会让宿主
			// 直接禁用凭证，把「某个模型不可用」升级成「账号失效」。
			return &core.Failure{Kind: core.FailureKindModelBlocked, Code: code, Msg: withBenefitHint(msg)}
		}
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: withBenefitHint(msg)}
	case status == http.StatusTooManyRequests || strings.Contains(code, "Throttling"):
		return &core.Failure{Kind: core.FailureKindRateLimit, Status: status, Code: code, Msg: msg}
	case status >= 500:
		return &core.Failure{Kind: core.FailureKindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Code: code, Msg: msg}
	}
}

// isBenefitMissing 判断是否为「账号没有该模型的免费福利」。
func isBenefitMissing(code, msg string) bool {
	return strings.Contains(code, benefitCodeMarker) ||
		strings.Contains(strings.ToLower(msg), benefitMsgMarker)
}

// withBenefitHint 保留上游原文（含 requestId / traceId），再补一句可操作提示。
func withBenefitHint(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.Contains(msg, benefitHint) {
		return msg
	}
	if msg == "" {
		return benefitHint
	}
	return msg + " — " + benefitHint
}
