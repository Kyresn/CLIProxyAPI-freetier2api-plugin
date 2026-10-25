package minimaxcode

import (
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
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
