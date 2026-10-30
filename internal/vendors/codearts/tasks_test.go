package codearts

import (
	"net/http"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestIdentifierAcceptsNumberAndString 锁住 campaignId 的数字/字符串两种形态。
//
// 服务端实测下发数字（campaignId=1）；只接受字符串会让领取因「缺少
// campaignId」失败。
func TestIdentifierAcceptsNumberAndString(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"number", float64(1), "1"},
		{"string", "abc", "abc"},
		{"missing", nil, ""},
	}
	for _, testCase := range cases {
		source := map[string]any{}
		if testCase.value != nil {
			source["campaignId"] = testCase.value
		}
		if got := identifier(source, "campaignId"); got != testCase.want {
			t.Errorf("%s: identifier = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

// TestFirstIntFallsBack 锁住积分数多级回退（benefitAmount 优先）。
func TestFirstIntFallsBack(t *testing.T) {
	source := map[string]any{
		"benefitAmount": float64(1000),
		"amount":        float64(5),
	}
	if got := firstInt(source, "benefitAmount", "amount"); got != 1000 {
		t.Fatalf("firstInt = %d, want 1000", got)
	}
	if got := firstInt(map[string]any{"amount": "12"}, "benefitAmount", "amount"); got != 12 {
		t.Fatalf("字符串数字 = %d, want 12", got)
	}
}

// TestSignSnapRequestKeepsAgentTypeUnsigned 锁住 Agent-Type/X-Language 不参与签名。
//
// 参考实现实测：把它们放进 SignedHeaders 会让网关回 APIG.0301 验签失败。
func TestSignSnapRequestKeepsAgentTypeUnsigned(t *testing.T) {
	req, errNew := http.NewRequest(http.MethodPost, SnapEngineApiHost+EpOpsClaim, strings.NewReader(`{}`))
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Agent-Type", "PromptCenter")
	req.Header.Set("X-Language", "zh-cn")
	signSnapRequest(req, []byte(`{}`), &Credential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st"})

	authorization := req.Header.Get("Authorization")
	if !strings.Contains(authorization, "SDK-HMAC-SHA256") {
		t.Fatalf("缺少签名头: %q", authorization)
	}
	if strings.Contains(authorization, "agent-type") || strings.Contains(authorization, "x-language") {
		t.Fatalf("Agent-Type/X-Language 不应进入 SignedHeaders: %q", authorization)
	}
	if !strings.Contains(authorization, "content-type") {
		t.Fatalf("POST 的 Content-Type 应参与签名: %q", authorization)
	}
	// 签名后追加的头仍必须发送（服务端路由需要）。
	if got := req.Header.Get("Agent-Type"); got != "PromptCenter" {
		t.Fatalf("Agent-Type = %q", got)
	}
}

// TestCreditQuotaUsesTotalMetric 锁住积分额度取总额指标，不重复累加分类。
func TestCreditQuotaUsesTotalMetric(t *testing.T) {
	info := &accountInfo{}
	info.Package.IsCreditPackage = true
	info.Package.PackageNameCN = "企业版"
	info.Metrics = []accountMetric{
		{Name: "usageTotalPackageCredit", Amount: float64(1000), Used: float64(200), Remain: "800"},
		{Name: "usageBasicPackageCredit", Amount: float64(1000), Used: float64(200), Remain: float64(800)},
		{Name: "unknownMetric", Amount: float64(9999)},
	}
	quota, okQuota := creditQuota(info)
	if !okQuota {
		t.Fatal("应解析出积分额度")
	}
	values := map[string]float64{}
	for _, metric := range quota.Summary {
		values[metric.Key] = metric.Value
	}
	if values["total"] != 1000 || values["remaining"] != 800 || values["used"] != 200 {
		t.Fatalf("summary = %+v, want total 1000 / remaining 800 / used 200", values)
	}
	if quota.Subscription == nil || quota.Subscription.TierName != "企业版" {
		t.Fatalf("subscription = %+v", quota.Subscription)
	}
	if len(quota.Groups) != 1 || len(quota.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v, want 2 个积分包", quota.Groups)
	}
}

// TestCreditQuotaWithoutMetrics 锁住「积分账户但无指标」时 ok=false。
func TestCreditQuotaWithoutMetrics(t *testing.T) {
	info := &accountInfo{}
	info.Package.IsCreditPackage = true
	info.Metrics = []accountMetric{{Name: "unknownMetric", Amount: float64(1)}}
	if _, okQuota := creditQuota(info); okQuota {
		t.Fatal("没有积分指标时不应解析出额度")
	}
}

// TestMetricFloatAcceptsString 锁住上游数字字符串形态。
func TestMetricFloatAcceptsString(t *testing.T) {
	if got := core.FloatFromAny("12.5"); got != 12.5 {
		t.Fatalf("core.FloatFromAny = %v, want 12.5", got)
	}
	if got := core.FloatFromAny(nil); got != 0 {
		t.Fatalf("core.FloatFromAny(nil) = %v", got)
	}
}

// TestClassifySnapErrors 锁住 snap 网关错误的分类：正文原因要保留，STS 过期
// 必须归成可续期的凭证错误（状态 401），宿主的保活重试才会触发。
func TestClassifySnapErrors(t *testing.T) {
	signature := Classify(400, `{"error_code":"APIG.0301","error_msg":"verify ak sk signature fail"}`)
	if !strings.Contains(signature.Msg, "APIG.0301") || !strings.Contains(signature.Msg, "signature fail") {
		t.Fatalf("签名错误正文丢失: %q", signature.Msg)
	}

	expired := Classify(400, `{"error_code":"APIG.0602","error_msg":"Bad request: the security token has expired"}`)
	if expired.Kind != core.FailureKindCredential || expired.HTTPStatus() != http.StatusUnauthorized {
		t.Fatalf("STS 过期应归为 401 凭证错误: kind=%v status=%d", expired.Kind, expired.HTTPStatus())
	}
}
