package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// TestIsAuthExpiredOnlyMatches401 锁定「只在 401 时才值得刷新一次」。
//
// 额度查询里任何 4xx/5xx 都触发一次保活是最省事的做法，但代价是上游改版或账号
// 无权限时也会白白多打一次上游，还可能把真正的错误盖住。只认 401。
func TestIsAuthExpiredOnlyMatches401(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 插件错误", core.NewPluginError("upstream_credential", "invalid access token", http.StatusUnauthorized), true},
		{"401 上游分类错误", &core.Failure{Kind: core.FailureKindCredential, Status: 401, Msg: "invalid access token"}, true},
		{"403 权限问题", core.NewPluginError("upstream_credential", "forbidden", http.StatusForbidden), false},
		{"502 上游异常", core.NewPluginError("upstream_request_failed", "bad gateway", http.StatusBadGateway), false},
		{"非凭证的分类错误", &core.Failure{Kind: core.FailureKindServer, Status: 500}, false},
		{"普通错误", errors.New("boom"), false},
		{"被包装的 401", fmt.Errorf("查询失败: %w", core.NewPluginError("upstream_credential", "expired", http.StatusUnauthorized)), true},
	}
	for _, c := range cases {
		if got := isAuthExpired(c.err); got != c.want {
			t.Fatalf("%s: isAuthExpired = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIsTransientFailureOnlyTrustsClassification 锁定「只按分类重试」。
//
// 按状态码猜（502/503 一律重试）会把上游明确拒绝也当成抖动；分类结果才是
// 唯一依据，供应商要把传输层失败归成 FailureKindTransient。
func TestIsTransientFailureOnlyTrustsClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transient 分类错误", &core.Failure{Kind: core.FailureKindTransient, Status: 502}, true},
		{"被包装的 transient", fmt.Errorf("查询失败: %w", &core.Failure{Kind: core.FailureKindTransient}), true},
		{"502 插件错误未分类", core.NewPluginError("codearts_snap_transport", "snap request failed: EOF", http.StatusBadGateway), false},
		{"503 上游不可用", core.NewPluginError("upstream_unavailable", "unavailable", http.StatusServiceUnavailable), false},
		{"401 凭证问题", core.NewPluginError("upstream_credential", "expired", http.StatusUnauthorized), false},
		{"400 参数问题", core.NewPluginError("upstream_request", "bad request", http.StatusBadRequest), false},
		{"普通错误", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := isTransientFailure(c.err); got != c.want {
			t.Fatalf("%s: isTransientFailure = %v, want %v", c.name, got, c.want)
		}
	}
}

// quotaVendor / plainVendor 用于验证「能力标志」与「查询结果」是两件事。
type quotaCapableVendor struct{ fakeVendor }

func (v *quotaCapableVendor) Quota(context.Context, *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	return &pluginapi.QuotaFetchResponse{}, nil
}

// TestSupportsQuotaComesFromCapability 锁定额度列的状态判定来源。
//
// 页面过去靠「结果里有没有这条」反推状态，于是「上游没有额度接口」（OpenCode
// ZEN）与「还没查」无法区分，账号会永久停在「查询中…」。能力必须来自接口断言。
func TestSupportsQuotaComesFromCapability(t *testing.T) {
	plain := &fakeVendor{Meta: core.Meta{VendorID: "plain"}}
	if supportsQuota(plain) {
		t.Fatal("vendor without QuotaProvider must report supports_quota=false")
	}
	capable := &quotaCapableVendor{fakeVendor{Meta: core.Meta{VendorID: "capable"}}}
	if !supportsQuota(capable) {
		t.Fatal("vendor implementing QuotaProvider must report supports_quota=true")
	}
}

// TestQuotaCacheKeyIsolatedByVendorAndAccount 锁定额度缓存键的唯一性。
//
// 键里必须同时含供应商与账号身份：过去把 Name / Label / AuthIndex 也当键，
// 跨供应商同名就会互相串值（OpenCode ZEN 显示 WorkBuddy 额度的事故）。
func TestQuotaCacheKeyIsolatedByVendorAndAccount(t *testing.T) {
	if quotaCacheKey("workbuddycn", "a", "") == quotaCacheKey("opencodezen", "a", "") {
		t.Fatal("cache key must include vendor: same account id across vendors must not collide")
	}
	if quotaCacheKey("workbuddycn", "a", "") == quotaCacheKey("workbuddycn", "b", "") {
		t.Fatal("cache key must include account identity")
	}
	if quotaCacheKey("", "a", "") != "" || quotaCacheKey("v", "", "") != "" {
		t.Fatal("incomplete identity must produce no key")
	}
	if quotaCacheKey("v", "", "auth-1") == "" {
		t.Fatal("AuthID must serve as a fallback identity when FileID is empty")
	}
}

// TestQuotaCacheIsolatedByVendor 锁定读取侧：同名账号跨供应商不共享额度。
func TestQuotaCacheIsolatedByVendor(t *testing.T) {
	s := &Service{quotaCache: map[string]quotaResult{}}
	s.cacheQuotaResult(quotaResult{VendorID: "workbuddycn", UID: "acc-1", Remain: 10, Total: 20})
	if q := s.getCachedQuota("opencodezen", "acc-1", "auth-1"); q != nil {
		t.Fatalf("must not read another vendor's quota: %+v", q)
	}
	if q := s.getCachedQuota("workbuddycn", "acc-1", "auth-1"); q == nil || q.Remain != 10 {
		t.Fatalf("own quota must be readable, got %+v", q)
	}
}
