package admin

// 本文件锁定账号表额度列的「没有总额」展示。
//
// AutoClaw 的积分钱包只返回 total_balance，没有总额/已用概念；auth.Totals
// 于是得到 Total=0。旧页面无条件拼 remain + " / " + total，显示成 10000 / 0
// 并配一条红色 0% 空槽，用户会以为额度坏了。

import (
	"strings"
	"testing"
)

func TestConsolePageHidesMissingTotal(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		"var hasTotal = quota.total > 0;",
		"上游只提供可用余额",
		// 没有总额时总额位必须是占位符而不是数字 0。
		"quota.remain + \" / \" + (hasTotal ? quota.total : \"\u2014\")",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the missing-total guard (%s)", literal)
		}
	}
	// 无条件拼接会把「没有总额」渲染成 / 0，必须不再存在。
	if strings.Contains(page, "quota.remain + \" / \" + quota.total") {
		t.Fatal("console page must not render remain / total unconditionally")
	}
}

// TestConsolePageShowsQuotaFailureReason 锁定额度失败原因的展示出口。
//
// 后端 runQuotaBatch 把失败原因写进缓存的 quota.message 已久，但页面在
// !quota.ok 时只渲染「-- / --」，用户拿到的信息与「上游没有额度接口」没有
// 区别，无法判断该重新授权还是稍后再查。横幅也只报一个失败计数。
func TestConsolePageShowsQuotaFailureReason(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		`var reason = quota.message || "额度查询失败";`,
		"remainCell.title = reason;",
		"function shortReason(text)",
		// 横幅逐账号明细：前 5 条 + 折叠剩余。
		"failures.slice(0, 5).map(function (r)",
		`"…等 " + failures.length + " 个账号失败"`,
		// 明细靠多行排布，缺了 white-space 会被挤成一行。
		"white-space: pre-line;",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing quota failure rendering (%s)", literal)
		}
	}
}
