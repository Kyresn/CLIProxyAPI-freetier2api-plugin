package auth

import (
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// TestTotalsRecognizesEveryVendorKeyFamily 锁定「剩余 / 总额」的键名词表。
//
// Totals 只认白名单，供应商写别的键名会**静默**得到 0/0，控制台就一直显示
// 0 / 0。这里把仓库里各供应商实际在用的命名族全部钉住，新增供应商若另起
// 一套命名，先在这里失败，而不是上线后表现成一个空进度条。
func TestTotalsRecognizesEveryVendorKeyFamily(t *testing.T) {
	cases := []struct {
		name    string
		summary []pluginapi.QuotaMetric
		want    QuotaTotals
	}{
		{
			name: "标准键（CodeArts / MiniMax / Trae SOLO 早期）",
			summary: []pluginapi.QuotaMetric{
				{Key: "remaining", Value: 80},
				{Key: "total", Value: 100},
				{Key: "used", Value: 20},
			},
			want: QuotaTotals{Remaining: 80, Total: 100},
		},
		{
			name: "credit_* 族（WorkBuddy / Trae）",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: 4794.81},
				{Key: "credit_total", Value: 4800},
				{Key: "credit_used", Value: 5.19},
			},
			want: QuotaTotals{Remaining: 4794, Total: 4800},
		},
		{
			name: "quota_* 族（Qoder）",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 900},
				{Key: "user_quota_total", Value: 1000},
				{Key: "addon_quota_remaining", Value: 50},
				{Key: "addon_quota_total", Value: 100},
			},
			want: QuotaTotals{Remaining: 900, Total: 1000},
		},
		{
			name: "只有已用、没有总额时反推",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: 80},
				{Key: "credit_used", Value: 20},
			},
			want: QuotaTotals{Remaining: 80, Total: 100},
		},
		{
			name: "合法零额度不被后续额度族覆盖",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 0},
				{Key: "addon_quota_remaining", Value: 50},
				{Key: "user_quota_total", Value: 100},
				{Key: "addon_quota_total", Value: 200},
			},
			want: QuotaTotals{Remaining: 0, Total: 100},
		},
		{
			// Qoder Free 计划：套餐额度是合法的 {used:0,total:0,remaining:0}，
			// 真正有数的是拓展包。按出现顺序取会把账号永久锁死在 0/0。
			name: "首个族为 0/0 时让位给有容量的族（Qoder Free + 拓展包）",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 0},
				{Key: "user_quota_total", Value: 0},
				{Key: "user_quota_used", Value: 0},
				{Key: "addon_quota_remaining", Value: 50},
				{Key: "addon_quota_total", Value: 200},
				{Key: "addon_quota_used", Value: 150},
			},
			want: QuotaTotals{Remaining: 50, Total: 200},
		},
		{
			name: "所有族都是 0/0 时保留第一个族（额度已用尽 / 未开通）",
			summary: []pluginapi.QuotaMetric{
				{Key: "user_quota_remaining", Value: 0},
				{Key: "user_quota_total", Value: 0},
				{Key: "addon_quota_remaining", Value: 0},
				{Key: "addon_quota_total", Value: 0},
			},
			want: QuotaTotals{Remaining: 0, Total: 0},
		},
		{
			name: "只给了 total=0 时不从 used 反推（presence 决定，非数值）",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: 0},
				{Key: "credit_total", Value: 0},
				{Key: "credit_used", Value: 20},
			},
			want: QuotaTotals{Remaining: 0, Total: 0},
		},
		{
			name: "无限额度（Trae 用 -1 / unlimited 表达）",
			summary: []pluginapi.QuotaMetric{
				{Key: "credit_remain", Value: -1, Unit: "unlimited"},
				{Key: "credit_total", Value: 0},
			},
			want: QuotaTotals{Unlimited: true},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Totals(&pluginapi.QuotaFetchResponse{Summary: testCase.summary})
			if got != testCase.want {
				t.Fatalf("Totals = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// TestTotalsIgnoresUnknownKeys 说明白名单的边界：不认识的键不会参与折算。
//
// 这条不是「期望行为值得保留」，而是把静默失败固定成可观测的断言——
// 有人给 Totals 加新族时，这里会提醒同步补上白名单。
func TestTotalsIgnoresUnknownKeys(t *testing.T) {
	got := Totals(&pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "remaining_credit", Value: 4794},
		{Key: "total_credit", Value: 4800},
		{Key: "used_credit", Value: 5},
	}})
	if got != (QuotaTotals{}) {
		t.Fatalf("未登记的键族不应被折算，得到 %+v", got)
	}
}

// TestTokenPlanFamilyIndependentOfDefault 锁定 token_plan 族与 default 族
// 并存时的取值行为：点数（default）有容量时优先，token 余量不覆盖点数；
// 点数耗尽时回落 token_plan。MiniMax Token Plan 与 op 点数并存的账号
// 依赖这一行为：同 key 会撞族，token 余量会被点数静默吞掉。
func TestTokenPlanFamilyIndependentOfDefault(t *testing.T) {
	both := &pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "remaining", Value: 500, Unit: "点"},
		{Key: "token_remaining", Value: 14291681135, Unit: "tokens"},
	}}
	if got := Totals(both); got.Remaining != 500 {
		t.Fatalf("default family must win when it has capacity, got %d", got.Remaining)
	}
	zeroPoints := &pluginapi.QuotaFetchResponse{Summary: []pluginapi.QuotaMetric{
		{Key: "remaining", Value: 0, Unit: "点"},
		{Key: "token_remaining", Value: 14291681135, Unit: "tokens"},
	}}
	if got := Totals(zeroPoints); got.Remaining != 14291681135 {
		t.Fatalf("token_plan family must take over when default is empty, got %d", got.Remaining)
	}
}
