package core

import (
	"net/http"
	"testing"
)

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        *Failure
		wantStatus int
		wantRetry  bool
	}{
		{"credential", &Failure{Kind: FailureKindCredential}, http.StatusUnauthorized, false},
		{"rate", &Failure{Kind: FailureKindRateLimit}, http.StatusTooManyRequests, true},
		{"quota", &Failure{Kind: FailureKindHardCredit}, http.StatusPaymentRequired, true},
		{"bad params", &Failure{Kind: FailureKindBadParams}, http.StatusBadRequest, false},
		{"channel rejected", &Failure{Kind: FailureKindChannelRejected}, http.StatusBadRequest, false},
		{"status passthrough", &Failure{Kind: FailureKindClient, Status: http.StatusConflict}, http.StatusConflict, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.HTTPStatus(); got != tc.wantStatus {
				t.Fatalf("HTTPStatus() = %d, want %d", got, tc.wantStatus)
			}
			if got := tc.err.Retryable(); got != tc.wantRetry {
				t.Fatalf("Retryable() = %v, want %v", got, tc.wantRetry)
			}
		})
	}
}
