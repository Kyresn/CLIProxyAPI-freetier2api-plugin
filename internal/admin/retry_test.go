package admin

import (
	"context"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// 两处重试的流程测试：额度查询（刷新 + 瞬时故障）与批量动作（刷新）。
//
// 这两条路径都容易写成「重试了，但用的是旧凭证」——单测里第一次请求成功，
// 线上表现为「刷新后仍然 401」。断言调用序列而不只是最终结果。

func TestQueryQuotaWithRetryReusesRefreshedCredential(t *testing.T) {
	oldCredential := &core.Credential{Token: "old"}
	newCredential := &core.Credential{Token: "new"}

	var tokens []string
	query := func(credential *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
		tokens = append(tokens, credential.TokenValue())
		switch len(tokens) {
		case 1:
			// 凭证过期：必须先刷新，再用新凭证重查。
			return nil, &core.Failure{Kind: core.FailureKindCredential, Status: http.StatusUnauthorized}
		case 2:
			// 紧接着的瞬时故障：重试仍要沿用刷新后的凭证。
			return nil, &core.Failure{Kind: core.FailureKindTransient, Status: http.StatusBadGateway}
		default:
			return &pluginapi.QuotaFetchResponse{}, nil
		}
	}
	refreshCalls := 0
	refresh := func() *core.Credential {
		refreshCalls++
		return newCredential
	}

	if _, errQuota := queryQuotaWithRetry(context.Background(), query, oldCredential, refresh); errQuota != nil {
		t.Fatalf("queryQuotaWithRetry: %v", errQuota)
	}
	want := []string{"old", "new", "new"}
	if len(tokens) != len(want) {
		t.Fatalf("调用序列 = %v, want %v", tokens, want)
	}
	for index := range want {
		if tokens[index] != want[index] {
			t.Fatalf("调用序列 = %v, want %v", tokens, want)
		}
	}
	if refreshCalls != 1 {
		t.Fatalf("刷新次数 = %d, want 1", refreshCalls)
	}
}

func TestQueryQuotaWithRetryRetriesTransientWithoutRefresh(t *testing.T) {
	credential := &core.Credential{Token: "same"}
	calls := 0
	query := func(*core.Credential) (*pluginapi.QuotaFetchResponse, error) {
		calls++
		if calls == 1 {
			return nil, &core.Failure{Kind: core.FailureKindTransient, Status: http.StatusBadGateway}
		}
		return &pluginapi.QuotaFetchResponse{}, nil
	}
	refreshCalls := 0
	refresh := func() *core.Credential {
		refreshCalls++
		return credential
	}

	if _, errQuota := queryQuotaWithRetry(context.Background(), query, credential, refresh); errQuota != nil {
		t.Fatalf("queryQuotaWithRetry: %v", errQuota)
	}
	if calls != 2 {
		t.Fatalf("查询次数 = %d, want 2", calls)
	}
	if refreshCalls != 0 {
		t.Fatalf("瞬时故障不应触发刷新，实际刷新 %d 次", refreshCalls)
	}
}

func TestQueryQuotaWithRetryKeepsOriginalErrorWhenRefreshFails(t *testing.T) {
	credential := &core.Credential{Token: "old"}
	calls := 0
	query := func(*core.Credential) (*pluginapi.QuotaFetchResponse, error) {
		calls++
		return nil, &core.Failure{Kind: core.FailureKindCredential, Status: http.StatusUnauthorized}
	}
	// 刷新失败（返回 nil）时不得用旧凭证再打一次。
	if _, errQuota := queryQuotaWithRetry(context.Background(), query, credential, func() *core.Credential { return nil }); errQuota == nil {
		t.Fatal("刷新失败时应保留原错误")
	}
	if calls != 1 {
		t.Fatalf("查询次数 = %d, want 1（不得用旧凭证重试）", calls)
	}
}

func TestRunWithAuthRetryOnlyOn401(t *testing.T) {
	oldCredential := &core.Credential{Token: "old"}
	newCredential := &core.Credential{Token: "new"}

	refreshCalls := 0
	actionCalls := 0
	outcome, errAction := runWithAuthRetry(oldCredential, func() *core.Credential {
		refreshCalls++
		return newCredential
	}, func(credential *core.Credential) (accountActionOutcome, error) {
		actionCalls++
		if actionCalls == 1 {
			return accountActionOutcome{}, &core.Failure{Kind: core.FailureKindCredential, Status: http.StatusUnauthorized}
		}
		if credential.TokenValue() != "new" {
			t.Fatalf("重试必须使用刷新后的凭证，实际 %q", credential.TokenValue())
		}
		return accountActionOutcome{Message: "ok"}, nil
	})
	if errAction != nil || outcome.Message != "ok" {
		t.Fatalf("重试后应成功: outcome=%+v err=%v", outcome, errAction)
	}
	if refreshCalls != 1 || actionCalls != 2 {
		t.Fatalf("刷新 %d 次 / 动作 %d 次, want 1 / 2", refreshCalls, actionCalls)
	}

	// 非 401（参数错误）不刷新：刷新不会变好，只会多打一次上游。
	refreshCalls = 0
	_, _ = runWithAuthRetry(oldCredential, func() *core.Credential {
		refreshCalls++
		return newCredential
	}, func(*core.Credential) (accountActionOutcome, error) {
		return accountActionOutcome{}, &core.Failure{Kind: core.FailureKindBadParams, Status: http.StatusBadRequest}
	})
	if refreshCalls != 0 {
		t.Fatalf("400 不应触发刷新，实际刷新 %d 次", refreshCalls)
	}
}
