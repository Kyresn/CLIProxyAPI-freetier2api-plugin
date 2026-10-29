package minimaxcode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/hostbridge"
)

// membershipFixture 是 2026-10-25 实测 get_membership_info 的裁剪响应。
func membershipFixture() map[string]any {
	return map[string]any{
		"has_token_plan":        true,
		"token_plan_tier":       "Go",
		"token_plan_expires_at": float64(1795219200000),
		"tp_remains":            14291681135.49,
		"op_group_id":           "2104384490896888672",
		"opcredit_balance":      float64(0),
		"is_migrated_to_op":     true,
	}
}

// TestExtractTokenPlanHead 验证 membership 的 Token Plan 概要提取。
func TestExtractTokenPlanHead(t *testing.T) {
	head := extractTokenPlanHead(membershipFixture())
	if head == nil {
		t.Fatal("head = nil, want token plan")
	}
	if head.planName != "MiniMax Token Plan Go" {
		t.Fatalf("planName = %q", head.planName)
	}
	if head.groupID != "2104384490896888672" {
		t.Fatalf("groupID = %q", head.groupID)
	}
	if head.expiresAtMS != 1795219200000 {
		t.Fatalf("expiresAtMS = %d", head.expiresAtMS)
	}
	if len(head.metrics) != 1 || head.metrics[0].Key != "token_remaining" {
		t.Fatalf("metrics = %+v", head.metrics)
	}
}

// TestExtractTokenPlanHeadUnsubscribed 验证未订阅（或全零）时返回 nil，
// 调用方回落 op 点数展示。
func TestExtractTokenPlanHeadUnsubscribed(t *testing.T) {
	if head := extractTokenPlanHead(map[string]any{
		"has_token_plan": false,
		"tp_remains":     0.0,
	}); head != nil {
		t.Fatalf("unsubscribed head = %+v", head)
	}
}

// TestQuotaResponseTokenPlan 验证 Token Plan 分支：订阅名/TierID 由它决定、
// 为 0 的 op 点数指标让位、窗口组原样带上。
func TestQuotaResponseTokenPlan(t *testing.T) {
	head := &tokenPlanHead{
		planName: "MiniMax Token Plan Go",
		metrics: []pluginapi.QuotaMetric{
			{Key: "token_remaining", Label: "Token Plan 剩余", Value: 14291681135, Unit: "tokens"},
		},
	}
	groups := []pluginapi.QuotaGroup{{DisplayName: "5 小时窗口"}}
	response := quotaResponse(map[string]any{
		"plan_name":        "Basic",
		"opcredit_balance": float64(0),
	}, head, groups)

	if response.Subscription == nil || response.Subscription.Plan != "MiniMax Token Plan Go" ||
		response.Subscription.TierID != "minimax-token-plan" {
		t.Fatalf("subscription = %+v", response.Subscription)
	}
	if len(response.Summary) != 1 || response.Summary[0].Key != "token_remaining" {
		t.Fatalf("summary = %+v, want only token_remaining", response.Summary)
	}
	if len(response.Groups) != 1 || response.Groups[0].DisplayName != "5 小时窗口" {
		t.Fatalf("groups = %+v", response.Groups)
	}

	// 没有 Token Plan：保留 op 点数与 minimax 档位。
	plain := quotaResponse(map[string]any{"opcredit_balance": float64(500)}, nil, nil)
	if plain.Subscription.TierID != "minimax" || plain.Subscription.Plan != "MiniMax Code" {
		t.Fatalf("plain subscription = %+v", plain.Subscription)
	}
	if len(plain.Summary) != 1 || plain.Summary[0].Key != "remaining" || plain.Summary[0].Value != 500 {
		t.Fatalf("plain summary = %+v", plain.Summary)
	}
}

// TestParseTokenPlanRemains 验证 JSON→窗口组装配：5 小时 / 7 天分组、
// status=2 按 0%、status=3 与缺字段跳过、非对象行忽略。
func TestParseTokenPlanRemains(t *testing.T) {
	raw := []byte(`{"model_remains":[
		{"model_name":"general","current_interval_remaining_percent":99,"current_interval_status":1,
		 "end_time":1792915791846,"current_interval_usage_count":3,"current_interval_total_count":100,
		 "current_weekly_remaining_percent":50,"current_weekly_status":1,"weekly_end_time":1793510591846},
		{"model_name":"MiniMax-M4","current_interval_remaining_percent":0,"current_interval_status":2,
		 "end_time":1792915791846},
		{"model_name":"dropped","current_interval_remaining_percent":40,"current_interval_status":3},
		{"model_name":""},
		"not-an-object"
	]}`)

	groups := parseTokenPlanRemains(raw)
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want 5h + 7d", groups)
	}
	interval := groups[0]
	if interval.DisplayName != "5 小时窗口" || len(interval.Buckets) != 2 {
		t.Fatalf("interval group = %+v", interval)
	}
	if got := interval.Buckets[0].RemainingFraction; got < 0.989 || got > 0.991 {
		t.Fatalf("general interval fraction = %v, want 0.99", got)
	}
	if got := interval.Buckets[0].ResetTime; got != "2026-10-25T08:09:51Z" {
		t.Fatalf("ResetTime = %q, want UTC RFC3339", got)
	}
	if got := interval.Buckets[1].RemainingFraction; got != 0 {
		t.Fatalf("exhausted bucket fraction = %v, want 0", got)
	}
	weekly := groups[1]
	if weekly.DisplayName != "7 天窗口" || len(weekly.Buckets) != 1 || weekly.Buckets[0].RemainingFraction != 0.5 {
		t.Fatalf("weekly group = %+v", weekly)
	}
}

// TestPlanWindowBucket 验证窗口行的折算：百分比标尺、重置时间、已耗尽与跳过语义。
func TestPlanWindowBucket(t *testing.T) {
	// status=1：正常，99% 剩余。
	bucket, ok := planWindowBucket("通用额度", "5 小时", float64(99), float64(1),
		float64(1792915791846), float64(0), float64(0))
	if !ok {
		t.Fatal("active window must render")
	}
	if bucket.RemainingFraction < 0.98 || bucket.RemainingFraction > 1.0 {
		t.Fatalf("fraction = %v", bucket.RemainingFraction)
	}
	if bucket.ResetTime != "2026-10-25T08:09:51Z" {
		t.Fatalf("ResetTime = %q", bucket.ResetTime)
	}

	// 1% 必须按 1% 展示：不能把 <=1 的小数当 0..1 标尺（会变成 100%）。
	bucket, ok = planWindowBucket("通用额度", "5 小时", float64(1), float64(1), nil, nil, nil)
	if !ok || bucket.RemainingFraction != 0.01 {
		t.Fatalf("1%% window = ok:%v fraction:%v", ok, bucket.RemainingFraction)
	}

	// status=2：耗尽且无百分比，按 0% 展示。
	bucket, ok = planWindowBucket("通用额度", "5 小时", nil, float64(2), nil, nil, nil)
	if !ok || bucket.RemainingFraction != 0 {
		t.Fatalf("exhausted window = ok:%v fraction:%v", ok, bucket.RemainingFraction)
	}

	// status=3：上游不展示，跳过。
	if _, ok = planWindowBucket("通用额度", "5 小时", float64(50), float64(3), nil, nil, nil); ok {
		t.Fatal("dropped window must not render")
	}

	// 非 status=2 且缺百分比（字段缺失/改名）：不渲染——0% 假进度条比不显示更误导。
	if _, ok = planWindowBucket("通用额度", "5 小时", nil, float64(1), nil, nil, nil); ok {
		t.Fatal("missing percent without status=2 must not render")
	}
	if _, ok = planWindowBucket("通用额度", "7 天", "", float64(0), nil, nil, nil); ok {
		t.Fatal("empty-string percent must not render")
	}
}

// ---- 签到：timezone_id / 业务码 / claim_result ----

// stubCheckinHost 把宿主 HTTP 桥换成固定响应：签到状态与领取各返回给定正文，
// 其它路径（prepare 的 config/agent/channel-connections）返回空对象。
func stubCheckinHost(t *testing.T, statusBody, claimBody string) string {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method string `json:"method"`
			URL    string `json:"url"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		parsed, errParse := url.Parse(req.URL)
		if errParse != nil {
			return nil, errParse
		}
		body := "{}"
		switch parsed.Path {
		case EpCheckinStatus:
			body = statusBody
		case EpCheckinClaim:
			body = claimBody
		}
		return json.Marshal(map[string]any{
			"StatusCode": http.StatusOK,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(body),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	return "https://agent.test"
}

func checkinCredential() *Credential {
	return &Credential{Region: RegionGlobal, AccessToken: "tok", AuthMode: "oauth"}
}

// TestSigninQuerySendsTimezoneID 锁住签到接口必填的 timezone_id。
func TestSigninQuerySendsTimezoneID(t *testing.T) {
	query := buildSigninQuery(checkinCredential(), 1000, false)
	if !strings.Contains(query, "timezone_id=") {
		t.Fatalf("query 缺少 timezone_id: %s", query)
	}
	if strings.Contains(query, "op_ticket=") {
		t.Fatalf("线上 query 不应包含 op_ticket: %s", query)
	}
}

// TestCheckinFailsOnBusinessErrorEnvelope 锁住 HTTP 200 + base_resp 非零必须失败。
func TestCheckinFailsOnBusinessErrorEnvelope(t *testing.T) {
	base := stubCheckinHost(t,
		`{"base_resp":{"status_code":0},"data":{"days":[]}}`,
		`{"base_resp":{"status_code":1001,"status_msg":"invalid timezone_id"},"data":{}}`)
	_, errCheckin := Checkin(context.Background(), checkinCredential(), base)
	if errCheckin == nil {
		t.Fatal("业务码非零应报错，不能当成签到成功")
	}
	if !strings.Contains(errCheckin.Error(), "invalid timezone_id") {
		t.Fatalf("错误应带上游原因: %v", errCheckin)
	}
}

// TestCheckinRejectsUnknownClaimResult 锁住 claim_result 只接受 1/2。
func TestCheckinRejectsUnknownClaimResult(t *testing.T) {
	base := stubCheckinHost(t,
		`{"base_resp":{"status_code":0},"data":{"days":[]}}`,
		`{"base_resp":{"status_code":0},"data":{"claim_result":0,"points":800}}`)
	_, errCheckin := Checkin(context.Background(), checkinCredential(), base)
	if errCheckin == nil {
		t.Fatal("claim_result=0 应报错")
	}
}

// TestCheckinClaimsPoints 锁住 claim_result=1 时上报 points（不是 points+bonus）。
func TestCheckinClaimsPoints(t *testing.T) {
	base := stubCheckinHost(t,
		`{"base_resp":{"status_code":0},"data":{"days":[]}}`,
		`{"base_resp":{"status_code":0},"data":{"claim_result":1,"points":800,"bonus_points":400}}`)
	result, errCheckin := Checkin(context.Background(), checkinCredential(), base)
	if errCheckin != nil {
		t.Fatalf("Checkin: %v", errCheckin)
	}
	if result.Already || result.Credit != 800 {
		t.Fatalf("结果 = %+v, want +800", result)
	}
}

// TestSigninBusinessError 覆盖 base_resp 缺失/零码/非零码三种形态。
func TestSigninBusinessError(t *testing.T) {
	if err := signinBusinessError(map[string]any{}); err != nil {
		t.Fatalf("缺少 base_resp 不应报错: %v", err)
	}
	if err := signinBusinessError(map[string]any{"base_resp": map[string]any{"status_code": float64(0)}}); err != nil {
		t.Fatalf("status_code=0 不应报错: %v", err)
	}
	err := signinBusinessError(map[string]any{"base_resp": map[string]any{
		"status_code": float64(7), "status_msg": "boom",
	}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("非零业务码应带原因: %v", err)
	}
}
