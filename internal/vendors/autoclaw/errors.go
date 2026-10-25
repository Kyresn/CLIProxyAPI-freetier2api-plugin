package autoclaw

// 本文件实现 AutoClaw 的上游错误分类。
//
// 分类依据 Agent2API 的 classify()：
//   - 业务码 410000 → 会话失效（凭证禁用，需重新登录）；
//   - 余额/额度文案与 HTTP 402 → hard_credit（长冷却）；
//   - HTTP 429 → 限流（短冷却）；
//   - 其余按通用状态码分类。

import (
	"net/http"
	"strconv"
	"strings"

	"freetier2api-plugin/internal/core"
)

// hardCreditMarkers 是余额/额度类文案（中英文，来自上游实测）。
var hardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "not enough credit",
	"insufficient balance", "balance not enough", "limit reached", "usage limit",
	"余额不足", "额度不足", "积分不足", "积分用完", "额度用尽", "没有积分", "配额不足",
}

// deadMarkers 是会话失效类文案。
var deadMarkers = []string{
	"not logged in", "unauthorized", "invalid token", "token expired",
	"login required", "session expired", "offline user session", "auth.inject failed",
	"未登录", "登录已过期", "请先登录", "token 失效",
}

// Classify 按 HTTP 状态码、业务码与响应体分类一个上游错误。
func Classify(status int, code int64, body string) *core.Failure {
	if DeadCode(code) {
		return failure(http.StatusUnauthorized, code, body, core.FailureKindSessionDead)
	}
	lower := strings.ToLower(body)
	for _, marker := range hardCreditMarkers {
		if strings.Contains(lower, marker) {
			return failure(http.StatusPaymentRequired, code, body, core.FailureKindHardCredit)
		}
	}
	for _, marker := range deadMarkers {
		if strings.Contains(lower, marker) {
			return failure(http.StatusUnauthorized, code, body, core.FailureKindSessionDead)
		}
	}
	switch {
	case status == http.StatusPaymentRequired:
		return failure(status, code, body, core.FailureKindHardCredit)
	case status == http.StatusTooManyRequests:
		return failure(status, code, body, core.FailureKindSoftRate)
	}
	kind := core.ClassifyStatus(status)
	return &core.Failure{
		Status: status,
		Code:   codeText(code),
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}

// failure 构造一个带业务码的 Failure。
func failure(status int, code int64, body string, kind core.FailureKind) *core.Failure {
	return &core.Failure{
		Status: status,
		Code:   codeText(code),
		Msg:    core.Truncate(body, 300),
		Kind:   kind,
	}
}

// codeText 把业务码渲染进 Code 字段（0 表示无业务码）。
func codeText(code int64) string {
	if code <= 0 {
		return ""
	}
	return strconv.FormatInt(code, 10)
}
