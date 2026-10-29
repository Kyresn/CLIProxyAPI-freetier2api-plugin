package minimaxcode

// 本文件实现 MiniMax Code 的上游错误分类。
//
// 上游用三种手段混合表达错误：HTTP 状态码、包裹在 200 响应里的业务码，
// 以及流式正文里的「数字码: 自然语言」文本（如 `50110:insufficient balance`）。
// 分类结果决定宿主怎么处置账号：
//   - 凭证失效 → 禁用账号，停止路由；
//   - 余额/额度耗尽 → 402 长冷却到次日；
//   - 限流 → 429 短冷却；
//   - 5xx / 网络 → 502 换号重试；
//   - 请求本身有问题 → 4xx 不罚账号。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"freetier2api-plugin/internal/core"
)

// 关键词表。判定顺序有意义：凭证失效先于余额、余额先于限流——
// 上游会把「登录过期」写成余额话术的变体，先判余额会误判成 402。
var (
	credentialMarkers = []string{
		"token invalid", "invalid token", "token expired", "token is expired",
		"unauthorized", "unauthenticated", "login required", "not logged in",
		"凭证无效", "凭证已过期", "登录已过期", "请重新登录", "未登录",
		// OAuth 刷新失败的标准错误码：refresh_token 被撤销或轮换失效时，
		// 上游回 400 invalid_grant，按状态码只能归到「客户端错误」。
		"invalid_grant", "invalid refresh token", "invalid_refresh_token", "token revoked",
	}
	creditMarkers = []string{
		"insufficient balance", "insufficient_balance", "insufficient credit",
		"no credit", "credit exhausted", "credits exhausted", "out of credit",
		"quota exceeded", "quota exhausted", "payment required",
		"余额不足", "积分不足", "额度不足", "额度用尽", "积分用完", "余额用完",
	}
	rateLimitMarkers = []string{
		"rate limit", "rate-limit", "rate-limited", "too many requests",
		"请求过于频繁", "限流",
	}
)

// Classify 把一次上游失败翻译成共享的上游错误。
//
// status 传 0 表示「HTTP 层没有错误信号」（流内业务错误、正文错误文本）。
// 此时分类完全由正文决定，且必须把 Status 归零——否则 Failure.HTTPStatus
// 会原样透传 200，宿主会把一次明确的余额耗尽当成成功。
func Classify(status int, body string) *core.Failure {
	lower := strings.ToLower(body)
	kind := core.FailureKindTransient
	if status >= http.StatusBadRequest {
		kind = core.ClassifyStatus(status)
	}
	if core.ContainsAny(lower, credentialMarkers) {
		kind = core.FailureKindCredential
	} else if core.ContainsAny(lower, creditMarkers) {
		kind = core.FailureKindHardCredit
	} else if core.ContainsAny(lower, rateLimitMarkers) {
		kind = core.FailureKindRateLimit
	}

	// 状态码与分类不一致时以分类为准（例如 400 正文写着 token expired：
	// 宿主需要的是 401 才能停用坏账号）。只有分类沿用了状态码推导时才透传。
	passthrough := status >= http.StatusBadRequest && kind == core.ClassifyStatus(status)
	if !passthrough {
		status = 0
	}
	return &core.Failure{
		Kind:   kind,
		Status: status,
		Code:   upstreamCode(body),
		Msg:    core.Truncate(body, 300),
	}
}

// upstreamCode 从正文里取上游业务码。
//
// 形态：`{"code":50110,...}` 或流内文本 `50110:insufficient balance (1008)`。
// 取不到就返回空串——不编造一个看似存在的错误码。
func upstreamCode(body string) string {
	var payload struct {
		Code      json.RawMessage `json:"code"`
		ErrorCode json.RawMessage `json:"error_code"`
		Status    json.RawMessage `json:"status_code"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &payload); errUnmarshal == nil {
		for _, raw := range []json.RawMessage{payload.Code, payload.ErrorCode, payload.Status} {
			if code := strings.Trim(strings.TrimSpace(string(raw)), `"`); code != "" && code != "null" {
				return code
			}
		}
	}
	trimmed := strings.TrimSpace(body)
	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	if digits >= 4 && digits < len(trimmed) && trimmed[digits] == ':' {
		return trimmed[:digits]
	}
	return ""
}

// isCredentialFailure 报告错误是否属于「凭证不可用」类。
//
// 登录落盘前用它把两类失败分开：凭证本身不可用（不能写盘，直接报错让用户
// 重来）与网络抖动（token 已经拿到，不影响落盘）。
func isCredentialFailure(err error) bool {
	var failure *core.Failure
	if errors.As(err, &failure) {
		return failure.Kind == core.FailureKindCredential
	}
	return false
}
