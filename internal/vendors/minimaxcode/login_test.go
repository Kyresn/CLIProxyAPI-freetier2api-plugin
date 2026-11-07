package minimaxcode

// 登录与刷新阶段的错误分类。
//
// refresh_token 被撤销时上游回 400 invalid_grant：按状态码只能归到「客户端
// 错误」，于是刷新失败被当成上游抖动反复重试——账号既不标坏，也没有任何
// 重新授权提示。这里锁定撤销类错误必须落到凭证失效（401）。

import (
	"net/http"
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestClassifyRevokedRefreshToken(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantKind   core.FailureKind
		wantStatus int
	}{
		{
			name:       "invalid_grant 是撤销不是参数错误",
			status:     http.StatusBadRequest,
			body:       `{"error":"invalid_grant","error_description":"refresh token revoked"}`,
			wantKind:   core.FailureKindCredential,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid_refresh_token 同样按凭证失效",
			status:     http.StatusBadRequest,
			body:       `{"error":"invalid_refresh_token"}`,
			wantKind:   core.FailureKindCredential,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "401 保持凭证失效",
			status:     http.StatusUnauthorized,
			body:       `{"error":"unauthorized"}`,
			wantKind:   core.FailureKindCredential,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "5xx 仍是可重试抖动",
			status:     http.StatusInternalServerError,
			body:       `upstream unavailable`,
			wantKind:   core.FailureKindTransient,
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := Classify(test.status, test.body)
			if failure.Kind != test.wantKind {
				t.Fatalf("kind = %s, want %s", failure.Kind, test.wantKind)
			}
			if got := failure.HTTPStatus(); got != test.wantStatus {
				t.Fatalf("HTTPStatus = %d, want %d", got, test.wantStatus)
			}
			if wantCredential := test.wantKind == core.FailureKindCredential; isCredentialFailure(failure) != wantCredential {
				t.Fatalf("isCredentialFailure = %v, want %v", !wantCredential, wantCredential)
			}
		})
	}
}

// TestIsCredentialFailureIgnoresPlainErrors 验证普通错误不会被当成凭证失效：
// 网络超时导致的登录校验失败不该让用户重走授权。
func TestIsCredentialFailureIgnoresPlainErrors(t *testing.T) {
	if isCredentialFailure(errPlain) {
		t.Fatal("plain error must not be treated as a credential failure")
	}
	if isCredentialFailure(nil) {
		t.Fatal("nil error must not be treated as a credential failure")
	}
}

var errPlain = &plainError{}

type plainError struct{}

func (e *plainError) Error() string { return "network timeout" }
