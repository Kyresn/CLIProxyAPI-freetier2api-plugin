package core

// 本文件放跨供应商复用的额度展示助手。
//
// 各供应商的额度响应结构不同，但「套餐有效期」与「比例夹紧」两类展示口径
// 是一致的：vendor 之间不能互相 import，这类共用的纯函数只能下沉到 core。

import (
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// QuotaExpiryGroup 构造「套餐有效期」展示组。
//
// 有效期不是「重置时间」：一次性礼包到期即失效、不会重置，因此日期放
// Description 而不是 ResetTime（后者会被渲染成「下次重置」）。也不给
// RemainingFraction——宿主会把它画成进度条，而有效期没有「剩余比例」。
func QuotaExpiryGroup(expiresAt time.Time) *pluginapi.QuotaGroup {
	return &pluginapi.QuotaGroup{DisplayName: "套餐有效期", Buckets: []pluginapi.QuotaBucket{{
		Window:      "有效期",
		Description: "套餐到期 " + expiresAt.UTC().Format(time.RFC3339),
	}}}
}

// ClampFraction 把额度比例夹到 [0, 1]。
func ClampFraction(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
