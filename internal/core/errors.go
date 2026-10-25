package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 本文件定义供应商错误的中立契约。
//
// 背景：插件把「上游错误」翻译成宿主认得的 HTTP 状态码，宿主据此决定
// 冷却/禁用/轮转。此前根层只认 workbuddy.Error，其它供应商的错误全部退化成
// 502 retryable（401 不会让凭证失效、429 不会触发限流处置）。现在每个供应商
// 的分类错误都实现本接口，由 ToPluginError 统一翻译。

// UpstreamError 是供应商分类后的上游错误。
//
// 方法名刻意避开 Kind / Status / Message：各家的 Error 结构体已经用了这些
// 字段名，而同名字段与方法在 Go 里不能共存。
type UpstreamError interface {
	error
	// HTTPStatus 返回映射给宿主的 HTTP 状态码。
	HTTPStatus() int
	// Retryable 报告宿主是否应该换一个凭证重试同一请求。
	Retryable() bool
	// UpstreamKind 返回错误分类标识（用于日志与错误码）。
	UpstreamKind() string
}

// UpstreamDetail 由能提供上游原始响应正文的错误实现。
//
// 用途：流式错误帧与日志里优先给用户看上游原文（诊断价值更高），
// 而不是插件重写的分类描述。
type UpstreamDetail interface {
	error
	UpstreamMessage() string
}

// ToPluginError 把供应商错误翻译成宿主契约错误。
//
// 非分类错误按 502 可重试处理：那通常是本地问题（编解码、连接），
// 换号重试是安全的。
func ToPluginError(err error) error {
	if err == nil {
		return nil
	}
	var upstreamErr UpstreamError
	if errors.As(err, &upstreamErr) {
		status := upstreamErr.HTTPStatus()
		if status <= 0 {
			status = http.StatusBadGateway
		}
		code := upstreamCode(upstreamErr.UpstreamKind())
		if upstreamErr.Retryable() {
			return RetryablePluginError(code, upstreamErr.Error(), status)
		}
		return NewPluginError(code, upstreamErr.Error(), status)
	}
	return RetryablePluginError("upstream_request_failed", err.Error(), http.StatusBadGateway)
}

// upstreamCode 生成错误码：upstream_<kind>。
func upstreamCode(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "upstream_error"
	}
	return "upstream_" + kind
}

// envelope 是 CPA 插件 ABI 的 JSON 信封，与宿主 cpasdk/pluginabi.Envelope 对齐。
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// envelopeError 携带可选 HTTP 状态码；宿主据此决定冷却或轮转当前凭证。
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// PluginError 是插件内部的结构化错误：可携带 HTTP 状态码与可重试标记。
//
// HTTPStatus 是插件与宿主之间最重要的契约之一：宿主按它决定凭证处置
// （402 长冷却 / 429 短冷却 / 401 禁用 / 403 软冷却），见 conductor_cooldown.go。
type PluginError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

// Error 实现 error 接口。
func (e *PluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// NewPluginError 构造一个带 HTTP 状态码的插件错误。
func NewPluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, HTTPStatus: status}
}

// RetryablePluginError 构造一个可重试的插件错误：宿主会换一个凭证重试同一请求。
func RetryablePluginError(code, message string, status int) *PluginError {
	return &PluginError{Code: code, Message: message, Retryable: true, HTTPStatus: status}
}

// OKEnvelope 把任意结果序列化为成功信封。
func OKEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal result: %w", errMarshal)
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope 构造失败信封（无 HTTP 状态码，宿主按 500 处理）。
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// ErrorEnvelopeFromError 把业务错误转成信封并保留 HTTP 状态码。
//
// 对 *PluginError 用类型断言而非 errors.As：插件内部没有错误包装链，
// 断言既够用又能避免把包装过的错误误判成结构化错误。
func ErrorEnvelopeFromError(err error) []byte {
	if err == nil {
		return ErrorEnvelope("plugin_error", "unknown error")
	}
	if pluginErr, ok := err.(*PluginError); ok {
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
			Code:       pluginErr.Code,
			Message:    pluginErr.Message,
			Retryable:  pluginErr.Retryable,
			HTTPStatus: pluginErr.HTTPStatus,
		}})
		return raw
	}
	return ErrorEnvelope("plugin_error", err.Error())
}

// ---------------------------------------------------------------------------
// 上游错误分类
//
// 各供应商只负责把上游响应翻译成这里的 FailureKind/Failure；宿主状态码、可重试
// 语义与错误码字符串统一由本文件给出。这样供应商之间不会互相 import，
// 也不会各自复制一份 HTTPStatus/Retryable 映射。

// FailureKind 是上游错误的可处置分类。
type FailureKind string

const (
	FailureKindNone            FailureKind = "none"
	FailureKindHardCredit      FailureKind = "hard_credit"
	FailureKindSoftRate        FailureKind = "soft_rate"
	FailureKindSessionDead     FailureKind = "session_dead"
	FailureKindNotFound        FailureKind = "not_found"
	FailureKindServer          FailureKind = "server"
	FailureKindContentBlocked  FailureKind = "content_blocked"
	FailureKindChannelRejected FailureKind = "channel_rejected"
	FailureKindBadParams       FailureKind = "bad_params"
	FailureKindAccountFault    FailureKind = "account_fault"
	FailureKindModelBlocked    FailureKind = "model_blocked"
	FailureKindWAFBlock        FailureKind = "waf_block"
	FailureKindPromptTooLong   FailureKind = "prompt_too_long"
	FailureKindImageInvalid    FailureKind = "image_invalid"
	FailureKindClient          FailureKind = "client"

	// 兼容通用供应商的短分类。
	FailureKindCredential FailureKind = "credential"
	FailureKindRateLimit  FailureKind = "rate_limit"
	FailureKindTransient  FailureKind = "transient"
	FailureKindQuota      FailureKind = "quota"
	FailureKindBusy       FailureKind = "busy"
)

// String 返回稳定的类别名（用于日志与错误码）。
func (k FailureKind) String() string {
	if k == "" {
		return "unknown"
	}
	return string(k)
}

// Failure 是带分类的上游错误。
//
// Msg 保持上游原文；Code 是上游业务码的可读形式（可为空）。RetryAfter /
// ResetAt 是上游明示的等待信息，零值表示未给出。
type Failure struct {
	Kind       FailureKind
	Status     int
	Code       string
	Msg        string
	RetryAfter time.Duration
	ResetAt    time.Time

	// RetryableOverride 允许供应商覆盖通用的可重试判定（nil 表示按 FailureKind 推导）。
	RetryableOverride *bool
}

// Failure 实现 error 接口。
func (e *Failure) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return fmt.Sprintf("upstream %s [%d/%s]: %s", e.Kind, e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("upstream %s [%d]: %s", e.Kind, e.Status, e.Msg)
}

// HTTPStatus 返回映射给宿主的 HTTP 状态码。
//
// 上游状态码通常已经是宿主认得的（401/402/429/5xx），优先透传；只有
// Status==0（网络层失败或 200 包体里的业务错误）才按 FailureKind 推导。
func (e *Failure) HTTPStatus() int {
	if e == nil {
		return http.StatusBadGateway
	}
	if e.Status > 0 {
		return e.Status
	}
	switch e.Kind {
	case FailureKindHardCredit, FailureKindQuota:
		return http.StatusPaymentRequired
	case FailureKindSoftRate, FailureKindRateLimit:
		return http.StatusTooManyRequests
	case FailureKindSessionDead, FailureKindCredential:
		return http.StatusUnauthorized
	case FailureKindNotFound, FailureKindModelBlocked:
		return http.StatusNotFound
	case FailureKindServer, FailureKindTransient:
		return http.StatusBadGateway
	case FailureKindWAFBlock:
		return http.StatusForbidden
	case FailureKindBusy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// Retryable 报告宿主是否应该换一个凭证重试同一请求。
func (e *Failure) Retryable() bool {
	if e == nil {
		return false
	}
	if e.RetryableOverride != nil {
		return *e.RetryableOverride
	}
	switch e.Kind {
	case FailureKindContentBlocked, FailureKindChannelRejected, FailureKindPromptTooLong,
		FailureKindImageInvalid, FailureKindBadParams, FailureKindClient, FailureKindCredential:
		return false
	default:
		return true
	}
}

// UpstreamKind 返回错误分类标识。
func (e *Failure) UpstreamKind() string { return e.Kind.String() }

// UpstreamMessage 返回上游原始正文（实现 core.UpstreamDetail）。
func (e *Failure) UpstreamMessage() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

// Bool 返回 bool 指针（构造 RetryableOverride 用）。
func Bool(value bool) *bool { return &value }

// ClassifyStatus 是通用 HTTP 状态分类：调用方可在此基础上追加业务码规则。
func ClassifyStatus(status int) FailureKind {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return FailureKindCredential
	case status == http.StatusPaymentRequired:
		return FailureKindHardCredit
	case status == http.StatusTooManyRequests:
		return FailureKindRateLimit
	case status >= 500:
		return FailureKindTransient
	case status >= 400:
		return FailureKindClient
	default:
		return FailureKindTransient
	}
}

// Truncate 截断过长的上游正文，避免错误信息淹没日志。
func Truncate(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 || len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "..."
}
