package zcode

import (
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestClassifyOrderingCaptchaBeforeAuth(t *testing.T) {
	// 403 + captcha 文案是验证码挑战，不是凭证失效——先判 403 会把账号错杀。
	risk := Classify(403, `{"code":3007,"msg":"captcha verify failed"}`)
	if risk.HTTPStatus() != 403 {
		t.Fatalf("验证码挑战应映射为软冷却(403)，得到 %d", risk.HTTPStatus())
	}

	// 纯 403 才是凭证失效。
	dead := Classify(403, `{"msg":"forbidden"}`)
	if dead.HTTPStatus() != 403 {
		t.Fatalf("凭证失效应映射为 403，得到 %d", dead.HTTPStatus())
	}
	if !strings.Contains(strings.ToLower(dead.Msg), "forbidden") && !strings.Contains(dead.Msg, "凭证") {
		t.Fatalf("unexpected message: %q", dead.Msg)
	}

	// 401 → 鉴权失败。
	if got := Classify(401, `{}`); got.HTTPStatus() != 401 {
		t.Fatalf("401 应映射为 401，得到 %d", got.HTTPStatus())
	}
}

func TestClassifyBusinessCodeWithoutStatus(t *testing.T) {
	// 上游以 HTTP 200 包装业务错误时，status=0 必须由 Kind 推导，
	// 不能把 200 当成错误的 HTTP 码透传给宿主。
	errQuota := Classify(0, `{"code":1005,"msg":"daily quota exhausted"}`)
	if errQuota.HTTPStatus() != 402 {
		t.Fatalf("额度耗尽应映射为 402，得到 %d", errQuota.HTTPStatus())
	}
}

// TestClassify500WrappedPlanConcurrencyLimit 验证上游把 Start Plan 并发超限
// 包成 500 时按文案归 SoftRate，而不是落进 5xx 上游故障分支计入熔断。
func TestClassify500WrappedPlanConcurrencyLimit(t *testing.T) {
	body := `{"message":"model concurrency limit exceeded","type":"server_error","code":"internal_server_error"}`
	if failure := Classify(http.StatusInternalServerError, body); failure.Kind != core.FailureKindSoftRate {
		t.Fatalf("kind = %v, want SoftRate", failure.Kind)
	}
}

// TestClassifyModelBucketExhausted 验证 1005 + "exceed quota limit" 归入
// 模型级避让（ModelBlocked）而不是账号级冷却（HardCredit）：活动礼包的
// 单模型桶耗尽时，同账号的其他模型（如 Flash）应继续可用。
func TestClassifyModelBucketExhausted(t *testing.T) {
	body := `{"code":1005,"msg":"exceed quota limit","logid":"x"}`
	failure := Classify(200, body)
	if failure.Kind != core.FailureKindModelBlocked {
		t.Fatalf("kind = %v, want ModelBlocked", failure.Kind)
	}
	if failure.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.HTTPStatus())
	}
}

// TestClassifyAccountLevelQuotaStillHardCredit 验证账号级额度信号仍走
// HardCredit：402、以及其他措辞的 1005（如领取名额用完）不按模型避让。
func TestClassifyAccountLevelQuotaStillHardCredit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"payment_required", http.StatusPaymentRequired, `{"msg":"no credit"}`},
		{"code1005_other_wording", 200, `{"code":1005,"msg":"daily claim quota used"}`},
		{"insufficient_marker", 200, `{"code":7,"msg":"insufficient balance"}`},
	}
	for _, testCase := range cases {
		failure := Classify(testCase.status, testCase.body)
		if failure.Kind != core.FailureKindHardCredit {
			t.Fatalf("%s: kind = %v, want HardCredit", testCase.name, failure.Kind)
		}
	}
}

// TestClassifyModelBucketExhaustedPriority 锁死模型桶耗尽的分类优先级：
// 402 是账号级欠费，不能被单模型桶抢走；403 仍是凭证失效；429 才按模型避让。
func TestClassifyModelBucketExhaustedPriority(t *testing.T) {
	body := `{"code":1005,"msg":"exceed quota limit","logid":"x"}`
	cases := []struct {
		name   string
		status int
		kind   core.FailureKind
		http   int
	}{
		{"payment_required_wins", http.StatusPaymentRequired, core.FailureKindHardCredit, http.StatusPaymentRequired},
		{"forbidden_is_session_dead", http.StatusForbidden, core.FailureKindSessionDead, http.StatusForbidden},
		{"too_many_requests_is_model_blocked", http.StatusTooManyRequests, core.FailureKindModelBlocked, http.StatusTooManyRequests},
	}
	for _, testCase := range cases {
		failure := Classify(testCase.status, body)
		if failure.Kind != testCase.kind || failure.HTTPStatus() != testCase.http {
			t.Fatalf("%s: kind=%v status=%d, want kind=%v status=%d",
				testCase.name, failure.Kind, failure.HTTPStatus(), testCase.kind, testCase.http)
		}
	}
}

// TestClassifyModelNotAllowedStatus 验证 200 信封里的 3006 换成语义状态码，
// 而不是把 200 当成错误的 HTTP 码透传给宿主。
func TestClassifyModelNotAllowedStatus(t *testing.T) {
	failure := Classify(http.StatusOK, `{"code":3006,"msg":"model not allowed"}`)
	if failure.Kind != core.FailureKindModelBlocked {
		t.Fatalf("kind = %v, want ModelBlocked", failure.Kind)
	}
	if failure.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", failure.HTTPStatus())
	}
}
