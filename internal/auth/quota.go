package auth

import (
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// Summary 生成一行额度摘要。
func Summary(response *pluginapi.QuotaFetchResponse) string {
	if response == nil {
		return ""
	}
	parts := make([]string, 0, len(response.Summary)+2)
	for _, metric := range response.Summary {
		label := strings.TrimSpace(metric.Label)
		if label == "" {
			label = metric.Key
		}
		parts = append(parts, label+" "+FormatValue(metric.Value))
	}
	for _, group := range response.Groups {
		for _, bucket := range group.Buckets {
			label := strings.TrimSpace(group.DisplayName)
			if label == "" {
				label = bucket.Window
			}
			parts = append(parts, label+" "+bucket.Description)
		}
	}
	return strings.Join(parts, " · ")
}

// FormatValue 格式化额度数值。
func FormatValue(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// QuotaTotals 是「剩余 / 总额」的折算结果。
type QuotaTotals struct {
	Remaining int64
	Total     int64
	// Unlimited 报告剩余额度无上限（上游以 -1 / unit=unlimited 表达）。
	// 无上限时 Remaining 没有意义，展示层应显示「无限」而不是数字。
	Unlimited bool
}

// Totals 折算「剩余 / 总额」。
//
// 键名按族识别（标准键、credit_*、user_quota_* / addon_quota_*、token_plan）。
// **新增供应商必须复用其中一族**：这个函数只认白名单，写别的键名会静默
// 得到 0/0，页面就会一直显示 0 / 0（Trae 曾用 remaining_credit 踩过）。
//
// 一个账号可能同时给出多个族（Qoder 的套餐额度 user_quota_* 与拓展包
// addon_quota_*）。优先级按**是否真有容量**而不是出现顺序：Qoder Free 计划的
// userQuota 是合法的 `{used:0,total:0,remaining:0}`，按出现顺序取会把整个
// 账号锁死在 0/0，即使拓展包里还有额度。只有所有族都没有容量时才回落到
// 第一个出现的族——那时 0/0 是真实语义（额度已用尽 / 未开通）。
func Totals(response *pluginapi.QuotaFetchResponse) QuotaTotals {
	if response == nil {
		return QuotaTotals{}
	}
	families := make([]quotaFamily, 0, 2)
	position := make(map[string]int, 2)
	for _, metric := range response.Summary {
		entry, known := quotaKeyRoles[metric.Key]
		if !known {
			continue
		}
		if entry.role == roleRemaining && (strings.EqualFold(metric.Unit, "unlimited") || metric.Value < 0) {
			return QuotaTotals{Unlimited: true}
		}
		index, okFamily := position[entry.family]
		if !okFamily {
			index = len(families)
			position[entry.family] = index
			families = append(families, quotaFamily{})
		}
		family := &families[index]
		switch entry.role {
		case roleRemaining:
			family.remaining, family.hasRemaining = int64(metric.Value), true
		case roleTotal:
			family.total, family.hasTotal = int64(metric.Value), true
		default:
			family.used += int64(metric.Value)
		}
	}
	if len(families) == 0 {
		return QuotaTotals{}
	}

	chosen := families[0]
	for _, family := range families {
		if family.hasCapacity() {
			chosen = family
			break
		}
	}
	// 没有总额、但有已用量时反推：让「只报已用」的供应商也能显示比例。
	// 显式给出了 total=0 的族不反推——那是「没有这份权益」，不是「没给数」。
	if !chosen.hasTotal && chosen.used > 0 {
		chosen.total = chosen.remaining + chosen.used
	}
	return QuotaTotals{Remaining: chosen.remaining, Total: chosen.total}
}

// quotaFamily 是同一族额度键（remaining / total / used）的汇总。
type quotaFamily struct {
	remaining, total, used int64
	hasRemaining, hasTotal bool
}

// hasCapacity 报告该族是否有真实额度（任一维度为正）。
func (f quotaFamily) hasCapacity() bool {
	return (f.hasRemaining && f.remaining > 0) || (f.hasTotal && f.total > 0)
}

// quotaKeyRoles 是额度键的唯一词表：键 → (族, 角色)。
//
// 增删键只改这张表。曾经 remaining / total / used 各自维护一份白名单、再加一份
// 族名映射，三份知识必须同步——漏一处就会静默算出 0/0。
var quotaKeyRoles = map[string]struct {
	family string
	role   int
}{
	"remaining":             {"default", roleRemaining},
	"total":                 {"default", roleTotal},
	"used":                  {"default", roleUsed},
	"credit_remain":         {"credit", roleRemaining},
	"credit_total":          {"credit", roleTotal},
	"credit_used":           {"credit", roleUsed},
	"user_quota_remaining":  {"user_quota", roleRemaining},
	"user_quota_total":      {"user_quota", roleTotal},
	"user_quota_used":       {"user_quota", roleUsed},
	"addon_quota_remaining": {"addon_quota", roleRemaining},
	"addon_quota_total":     {"addon_quota", roleTotal},
	"addon_quota_used":      {"addon_quota", roleUsed},
	// token_plan 族：MiniMax Token Plan 的 token 余量。点数（default 族）与
	// token 计划并存时各自成族，Totals 按「有容量者优先」取族，互不覆盖。
	"token_remaining": {"token_plan", roleRemaining},
}

// 额度键的角色：一个族里每个角色至多一个键。
const (
	roleRemaining = iota
	roleTotal
	roleUsed
)
