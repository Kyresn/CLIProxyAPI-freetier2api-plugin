package trae

import (
	"encoding/json"
	"fmt"
	"net/http"

	"freetier2api-plugin/internal/core"
)

// 本文件实现 Trae 家族的上游错误分类：解析 Trae 家族上游错误码与业务码。

// Classify 把 Trae 上游响应分类成共享的上游错误。
func Classify(status int, body string) *core.Failure {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &payload)

	msg := core.FirstNonEmpty(payload.Message, payload.Msg, body)
	code := ""
	if payload.Code != 0 {
		code = intString(payload.Code)
	}

	switch {
	case status == http.StatusUnauthorized || payload.Code == 4001:
		return &core.Failure{Kind: core.FailureKindCredential, Status: status, Code: code, Msg: msg}
	case payload.Code == 1005:
		// 1005 是套餐/硬额度限制，不是凭证失效。
		return &core.Failure{Kind: core.FailureKindHardCredit, Status: status, Code: "1005", Msg: core.FirstNonEmpty(msg, "套餐权益不足")}
	case payload.Code == 4008:
		return &core.Failure{Kind: core.FailureKindQuota, Status: status, Code: "4008", Msg: core.FirstNonEmpty(msg, "额度不足")}
	case status == http.StatusTooManyRequests || payload.Code == 429:
		return &core.Failure{Kind: core.FailureKindRateLimit, Status: status, Code: code, Msg: msg}
	case payload.Code == 9074:
		// 9074 = 设备级签到人数过多：可稍后重试，但不是 HTTP 限流。
		return &core.Failure{Kind: core.FailureKindBusy, Status: status, Code: "9074", Msg: core.FirstNonEmpty(msg, "签到人数过多，请稍后再试")}
	case status >= 500:
		return &core.Failure{Kind: core.FailureKindTransient, Status: status, Code: code, Msg: msg}
	default:
		return &core.Failure{Kind: core.FailureKindClient, Status: status, Code: code, Msg: msg}
	}
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
