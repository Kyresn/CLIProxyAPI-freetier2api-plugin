package opencodezen

import (
	"net/http"
	"testing"

	"freetier2api-plugin/internal/core"
)

// freeUsageLimitBody 是免费层额度耗尽时上游的原文（2026-10-26 实测
// POST https://opencode.ai/inference/v1/chat/completions，HTTP 429，
// Retry-After 指向次日 UTC 0 点）。
const freeUsageLimitBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`

// TestClassifyFreeUsageLimitIsRateLimit 锁定免费层额度的分类。
//
// 官方 v2 客户端在同一出口 IP 上拿到的是同一个 429，所以这是额度而不是
// 客户端标识问题：按 rate_limit 处理，让宿主短路冷却，不要误判成凭证失效。
func TestClassifyFreeUsageLimitIsRateLimit(t *testing.T) {
	failure := Classify(http.StatusTooManyRequests, freeUsageLimitBody)
	if failure.Kind != core.FailureKindRateLimit {
		t.Fatalf("kind = %s，期望 %s", failure.Kind, core.FailureKindRateLimit)
	}
	if got := failure.HTTPStatus(); got != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d，期望 429", got)
	}
	if failure.Msg != freeUsageLimitBody {
		t.Fatalf("上游原文被改写：%q", failure.Msg)
	}
}

// TestClassifyFreeTierErrorIsClient 锁定旧的 403 客户端风控分类。
//
// FreeTierError 是「非官方客户端」的硬拒绝，与凭证无关；按 client 处理
// 可保留账号继续尝试其它模型。
func TestClassifyFreeTierErrorIsClient(t *testing.T) {
	body := `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode."}}`
	failure := Classify(http.StatusForbidden, body)
	if failure.Kind != core.FailureKindClient {
		t.Fatalf("kind = %s，期望 %s", failure.Kind, core.FailureKindClient)
	}
}
