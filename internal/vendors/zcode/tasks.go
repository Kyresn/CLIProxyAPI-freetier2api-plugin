package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// billingHeaders 构造计费族请求头。
//
// 平台/语言/时区/设备 ID 全部取账号档案：上游按设备聚类做风控，
// 全局伪装值会让所有账号聚成同一台机器。
func billingHeaders(cred *Credential) map[string]string {
	headers := upstreamAuthHeaders(cred)
	headers["Content-Type"] = "application/json"
	return headers
}

// billingRequest 发起一次计费族请求并解析业务码。
//
// 返回业务码与响应体（解析失败时业务码为 -1）。网络与鉴权失败统一返回
// 分类后的上游错误，调用方不必各自判断状态码。
func billingRequest(ctx context.Context, cred *Credential, method, path string, payload any) (int, map[string]any, error) {
	return billingRequestWithHeaders(ctx, cred, method, path, payload, nil)
}

// billingRequestWithHeaders 是 billingRequest 的带附加头变体。
//
// 领取套餐要在身份头之外补人机验证码头（CaptchaHeader/CaptchaRegionHeader），
// 而额度查询不需要——用附加头参数区分两者，避免让每个调用方自己拼头。
func billingRequestWithHeaders(ctx context.Context, cred *Credential, method, path string, payload any, extra map[string]string) (int, map[string]any, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return 0, nil, errMarshal
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, errNew := http.NewRequestWithContext(ctx, method, BillingBase+path, bodyReader)
	if errNew != nil {
		return 0, nil, errNew
	}
	for key, value := range billingHeaders(cred) {
		req.Header.Set(key, value)
	}
	for key, value := range extra {
		req.Header.Set(key, value)
	}

	resp, errDo := hostbridge.Client(ctx, billingHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, nil, transientErr("计费请求失败: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return 0, nil, transientErr("读取计费响应失败: " + errRead.Error())
	}

	// 非 2xx 一律交给 Classify：它内部已经处理了「403 先判验证码挑战、再判凭证失效」
	// 的顺序（见 errors.go），此处不重复那套判定。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, nil, Classify(resp.StatusCode, string(raw))
	}

	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal != nil {
		return -1, nil, nil
	}
	return businessCode(parsed), parsed, nil
}

// businessCode 取上游业务码；缺失或非数字返回 -1（视为失败）。
func businessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	switch v := body["code"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if out, errParse := strconv.Atoi(strings.TrimSpace(v)); errParse == nil {
			return out
		}
	}
	return -1
}

// dataMap 取响应体的 data 对象。
func dataMap(body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	data, _ := body["data"].(map[string]any)
	return data
}

// billingBlockReason 报告该凭证为何不能打计费族；可打时返回空串。
//
// 计费族只服务 Coding Plan（JWT）。纯 API Key 账号没有套餐，不该产生
// billing 流量——既拿不到数据，又会白增一条上游风控信号。
func billingBlockReason(cred *Credential) string {
	if cred == nil {
		return "凭证缺失"
	}
	if !cred.UsesPlanChannel() {
		return "该账号没有 Coding Plan 凭证（JWT），不参与套餐与额度查询"
	}
	return ""
}

// classifyBillingOutcome 把计费族的业务失败码翻成可读文案。
func classifyBillingOutcome(code int, body map[string]any) string {
	serverMsg := core.FirstNonEmpty(core.RawStringField(body, "msg"), core.RawStringField(body, "message"))
	base := map[int]string{
		codePlanNotFound:    "套餐不存在",
		codePlanClosed:      "活动已结束或套餐暂不可领取",
		codePlanClaimed:     "该套餐已经领取过",
		codePlanNotEligible: "不符合领取条件",
		codeDailyQuotaUsed:  "今日领取名额已用完",
		3001:                "请求参数不全（缺少设备标识）",
		3007:                "验证码校验失败",
		401:                 "请先登录后再领取",
	}[code]
	if base == "" {
		base = "上游返回业务错误"
	}
	if serverMsg != "" {
		return base + "（" + serverMsg + "）"
	}
	return base
}

// fetchQuota 查询 ZCode 账号的套餐与余量。
func (v *adapter) fetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil {
		return nil, transientErr("zcode credential is nil")
	}

	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "ZCode Coding Plan",
			TierName: "Standard",
			TierID:   "zcode",
		},
	}

	if reason := billingBlockReason(cred); reason != "" {
		// 非 Coding Plan 账号：如实说明，不伪装成「查询成功但无数据」。
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "status", Label: "账号状态", Value: 1, Unit: reason,
		})
		// 带上原因：页面据此在按钮上给出说明，而不是显示成「没有可领套餐」。
		response.Claim = &pluginapi.ClaimStatus{Error: reason}
		return response, nil
	}

	// 一次快照同时供给三处：订阅名、各模型余量、套餐领取状态。
	// 多出的那一次 preview 请求换来的是「按钮是否置灰」不必另开轮询端点。
	snapshot := v.fetchPlanSnapshot(ctx, cred)
	applySubscription(snapshot, response)
	response.Claim = claimStatusOf(snapshot)
	response.Summary = append(response.Summary, snapshot.metrics...)
	response.Groups = append(response.Groups, snapshot.groups...)
	if snapshot.errBalance != nil {
		return nil, snapshot.errBalance
	}
	if len(snapshot.metrics) == 0 {
		// 上游在活动额度生效前不会生成 balances 桶，只回空额度；但授予量已经
		// 写在 plans[].entitlements 里。用授予明细兜底展示「剩余 / 总额」，
		// 并注明生效时刻，页面不必等到生效那一刻才有数字。
		note := "尚未领取体验套餐（Start Plan），领取后才有额度"
		if metrics, groups, effectiveAt := planGrantQuota(snapshot.plans, snapshot.serverTime); len(metrics) > 0 {
			response.Summary = append(response.Summary, metrics...)
			response.Groups = append(response.Groups, groups...)
			note = "套餐已领取，额度尚未生效"
			if effectiveAt > 0 {
				note = "套餐已领取，额度 " + time.Unix(effectiveAt, 0).UTC().Format(time.RFC3339) + " 生效"
			}
		} else if len(activePlans(snapshot.plans, snapshot.serverTime)) > 0 {
			// 有套餐但没有可解析的授予明细：领了，桶还没生成。
			note = "套餐已领取，额度桶尚未生效"
		}
		response.Summary = append(response.Summary, pluginapi.QuotaMetric{
			Key: "status", Label: "账号状态", Value: 1, Unit: note,
		})
	}
	return response, nil
}

// applySubscription 用已持有的套餐补充订阅名（失败时保留默认值）。
//
// 数据来自套餐快照里已经拿到的 billing/balance，不额外发请求。
func applySubscription(snapshot planSnapshot, response *pluginapi.QuotaFetchResponse) {
	plan := subscriptionPlan(snapshot.plans, snapshot.balances, snapshot.serverTime)
	if plan == nil {
		return
	}
	name := core.RawStringField(plan, "name")
	if name == "" {
		return
	}
	response.Subscription.Plan = name
	response.Subscription.TierName = core.FirstNonEmpty(
		normalizePlanID(core.FirstNonEmpty(
			core.RawStringField(plan, "plan_id"),
			core.RawStringField(plan, "planId"),
		)),
		"zcode",
	)
}

// subscriptionPlan 选出与该账号额度桶关联最紧的生效套餐。
//
// 关联证据只有到期时刻：实测 billing/balance 的 balances 不带 plan_id，
// 而桶的 expires_at 与套餐的 ends_at 相等即同一份权益（1024 礼包与 Basic
// 并存时靠它区分，而不是靠数组顺序——数组第一个 active 未必是正在用的那份）。
// 没有额度桶、或到期对不上时退回最早的生效套餐，与既有展示一致，不凭空造名。
func subscriptionPlan(plans, balances []map[string]any, serverTime int64) map[string]any {
	active := activePlans(plans, serverTime)
	if len(active) == 0 {
		return nil
	}
	if len(balances) == 0 {
		return active[0]
	}
	bucketExpiries := make(map[int64]int, len(balances))
	for _, bucket := range balances {
		if expires := core.GetUnixSeconds(bucket, "expires_at"); expires > 0 {
			bucketExpiries[expires]++
		}
	}
	best, bestHits := active[0], 0
	for _, plan := range active {
		if hits := bucketExpiries[core.GetUnixSeconds(plan, "ends_at")]; hits > bestHits {
			best, bestHits = plan, hits
		}
	}
	return best
}

// balanceQuota 把 billing/balance 的额度桶一次遍历折成（指标, 窗口组）。
//
// 单遍历同时产出两份：逐桶指标进摘要串，窗口组驱动宿主卡片的进度条；
// 分两趟各自解析同一响应曾让口径漂移（P1 审查意见）。
// 同名桶按出现顺序去重计入汇总（上游同模型多条时只计首条，避免重复加总）。
func balanceQuota(balances []map[string]any) ([]pluginapi.QuotaMetric, []pluginapi.QuotaGroup) {
	if len(balances) == 0 {
		return nil, nil
	}

	var totalRemaining, totalTotal, totalUsed float64
	seen := map[string]bool{}
	parsed := 0
	metrics := make([]pluginapi.QuotaMetric, 0, len(balances))
	group := pluginapi.QuotaGroup{DisplayName: "模型额度"}
	for _, entry := range balances {
		name := core.FirstNonEmpty(core.RawStringField(entry, "show_name"), core.RawStringField(entry, "model"))
		if name == "" {
			continue
		}
		parsed++
		remaining := floatField(entry, "remaining_units")
		used := floatField(entry, "used_units")
		total := floatField(entry, "total_units")
		if !seen[name] {
			seen[name] = true
			totalRemaining += remaining
			totalTotal += total
			totalUsed += used
		}
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key:   "remaining_" + sanitizeMetricKey(name),
			Label: name + " 剩余",
			Value: remaining,
			Unit:  "tokens",
		})
		if used > 0 {
			metrics = append(metrics, pluginapi.QuotaMetric{
				Key:   "used_" + sanitizeMetricKey(name),
				Label: name + " 已用",
				Value: used,
				Unit:  "tokens",
			})
		}

		// 比例口径与 qoder 一致：有总额按比例，总额缺失但仍有余额时
		// 视为满额（上游只给余额时不能画成 0%，那是「已用尽」的假象）。
		fraction := 0.0
		switch {
		case total > 0:
			fraction = core.ClampFraction(remaining / total)
		case remaining > 0:
			fraction = 1
		}
		bucket := pluginapi.QuotaBucket{
			Window:            name,
			RemainingFraction: fraction,
			Description:       fmt.Sprintf("已用 %s / 共 %s tokens", formatTokens(used), formatTokens(total)),
		}
		if expires := core.GetUnixSeconds(entry, "expires_at"); expires > 0 {
			// ResetTime 的既定语义是「重置时间」，而 one_time 礼包不重置、
			// 到期即失效——这里按桶粒度放 expires_at 供「失效」参考，
			// 套餐级的到期展示见 planExpiryGroup（走 Description）。
			bucket.ResetTime = time.Unix(expires, 0).UTC().Format(time.RFC3339)
		}
		group.Buckets = append(group.Buckets, bucket)
	}
	if parsed == 0 {
		// 全部条目解析失败：不给「剩余 0」的假汇总，交回空指标。
		return nil, nil
	}
	// default 族汇总放最前：账号表的「剩余 / 总额」靠这两个键算比例。
	summary := []pluginapi.QuotaMetric{
		{Key: "remaining", Label: "剩余", Value: totalRemaining, Unit: "tokens"},
	}
	if totalTotal > 0 {
		summary = append(summary, pluginapi.QuotaMetric{Key: "total", Label: "总量", Value: totalTotal, Unit: "tokens"})
	}
	if totalUsed > 0 {
		summary = append(summary, pluginapi.QuotaMetric{Key: "used", Label: "已用", Value: totalUsed, Unit: "tokens"})
	}
	return append(summary, metrics...), []pluginapi.QuotaGroup{group}
}

// planExpiryGroup 从仍生效的套餐里取**最近一个到期时刻**作为套餐有效期提示。
//
// 口径与 heldPlanIDs 一致（经 activePlans 共用）：空 status 视作 active、
// active 但已过 ends_at 视作 expired。多套餐并存时取最近的到期——用户
// 关心的是「下一份什么时候没」，取最晚会把近期到期藏起来。
// 不占 ResetTime（那是「重置时间」语义，礼包不重置），走 Description。
func planExpiryGroup(plans []map[string]any, serverTime int64) *pluginapi.QuotaGroup {
	nearest := int64(0)
	for _, plan := range activePlans(plans, serverTime) {
		if ends := core.GetUnixSeconds(plan, "ends_at"); ends > 0 && (nearest == 0 || ends < nearest) {
			nearest = ends
		}
	}
	if nearest == 0 {
		return nil
	}
	return core.QuotaExpiryGroup(time.Unix(nearest, 0))
}

// activePlans 返回仍生效的套餐条目（原样引用，不拷贝）。
//
// 判定口径唯一：status 缺失视作 active（上游历史形态），status 非 active
// 排除；active 但已过 ends_at 同样排除（官方客户端 normalizeStartPlanExpiry
// 的归一）。heldPlanIDs 与 planExpiryGroup 共用本函数，保证「已持有」
// 与「有效期」两张视图永远一致。
func activePlans(plans []map[string]any, serverTime int64) []map[string]any {
	out := make([]map[string]any, 0, len(plans))
	for _, entry := range plans {
		status := strings.ToLower(strings.TrimSpace(core.RawStringField(entry, "status")))
		if status != "" && status != "active" {
			continue
		}
		if endsAt := core.GetUnixSeconds(entry, "ends_at"); endsAt > 0 && serverTime > 0 && endsAt <= serverTime {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// formatTokens 把 token 数格式化为整数字符串（小数四舍五入）。
func formatTokens(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64)
}

// sanitizeMetricKey 把模型名转成稳定的指标键。
func sanitizeMetricKey(name string) string {
	replacer := strings.NewReplacer(" ", "_", ".", "_", "/", "_", "-", "_")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(name)))
}

// floatField 宽松取浮点字段（JSON 数字一律 float64）。
func floatField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// Coding Plan 套餐的探测与人工领取。
//
// 链路只有两步，与参考实现 zcode2api/app/claim.py 同形：
//
//  1. GETbilling/preview?app_version=&platform= → data.plans[]（上游投放）
//  2. POST billing/claim  body {"plan_id":...}   （必须带人机验证令牌）
//
// **领取必须由人完成**：上游对 billing/claim 强制校验阿里云无痕验证令牌，
// 没有令牌就是 3007。参考项目为此写的「无痕验证（免浏览器）」求解器有 4500 行
// Node，本插件不重复造；改为把 SDK 初始化参数交给控制台页，在用户浏览器里
// 渲染滑块，用户通过后插件只做转发（见 core.PlanClaimer 契约）。
//
// 上游业务码：
//
//	0 成功 / 1001 套餐不存在 / 1002 活动结束 / 1003 已领取过
//	1004 不符合条件 / 1005 今日名额用完 / 3007 验证码失败 / 3012 风控拦截

// 领取相关的业务码（errors.go 已有的通用码不在此重复）。
const (
	codePlanNotFound    = 1001 // 套餐不存在（活动已下线）
	codePlanClosed      = 1002 // 活动已结束或暂不可领取
	codePlanClaimed     = 1003 // 已经领取过
	codePlanNotEligible = 1004 // 不符合领取条件
	codeDailyQuotaUsed  = 1005 // 今日领取名额已用完
)

// 领取节流：同一出口（= 同一台 CPA 主机）连打会触发 WAF 1005/3012，
// 因此两次领取之间留出最小间隔。参考实现按代理端口做同样的事，这里没有
// 代理概念，按实例内节流等价。
//
// **状态属于实例**：同一进程里的多个 App 各有自己的节流窗口，互不影响
// （与 README 的实例隔离约定一致，也和测试里多 App 并存的场景相符）。
const defaultClaimMinInterval = 15 * time.Second

// claimGate 是一个 vendor 实例的领取节流器。
type claimGate struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// newClaimGate 构造节流器，默认间隔为 defaultClaimMinInterval。
func newClaimGate() *claimGate {
	return &claimGate{interval: defaultClaimMinInterval}
}

// setInterval 覆盖最小间隔（0 表示不节流）。
func (g *claimGate) setInterval(interval time.Duration) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.interval = interval
	g.mu.Unlock()
}

// wait 阻塞到允许发起下一次领取为止。
//
// 返回 false 表示 ctx 已取消：调用方应放弃本次领取而不是强发。
func (g *claimGate) wait(ctx context.Context) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	interval := g.interval
	wait := interval - time.Since(g.last)
	g.mu.Unlock()
	if interval <= 0 || wait <= 0 {
		return true
	}
	return core.SleepContext(ctx, wait)
}

// markIssued 记录一次领取已发出（节流计时起点）。
func (g *claimGate) markIssued() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
}

// ── 验证码配置 ──────────────────────────────────────────────────────────────

// FetchCaptchaConfig 取阿里云无痕验证的 SDK 初始化参数。
//
// 来源是**免鉴权公开端点** client/configs 的 data.configs.captcha——插件读模型
// 清单时已经在打这个端点，这里只是多解析一层字段，不额外增加请求。
func FetchCaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	fallback := core.CaptchaConfig{
		Enabled: true,
		SceneID: captchaFallbackScene,
		Region:  captchaFallbackRegion,
		Prefix:  captchaFallbackPrefix,
	}

	endpoint := ZCodeOrigin + PathClientConfigs + "?app_version=" + ClientAppVersion
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errNew != nil {
		return fallback, nil
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, errDo := hostbridge.Client(ctx, 15*time.Second).Do(req)
	if errDo != nil {
		return fallback, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return fallback, nil
	}

	config := parseCaptchaConfig(raw)
	if config.SceneID == "" {
		// 上游没给 sceneId 时保留兜底的prefix/region，只补齐能拿到的字段。
		config.Prefix = core.FirstNonEmpty(config.Prefix, fallback.Prefix)
		config.Region = core.FirstNonEmpty(config.Region, fallback.Region)
		return fallback, nil
	}
	config.Enabled = true
	return config, nil
}

// parseCaptchaConfig 从 client/configs 响应里提取 data.configs.captcha。
func parseCaptchaConfig(body []byte) core.CaptchaConfig {
	var payload struct {
		Data struct {
			Configs struct {
				Captcha struct {
					Enabled bool   `json:"enabled"`
					SceneID string `json:"sceneId"`
					Region  string `json:"region"`
					Prefix  string `json:"prefix"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return core.CaptchaConfig{}
	}
	captcha := payload.Data.Configs.Captcha
	return core.CaptchaConfig{
		Enabled: captcha.Enabled,
		SceneID: strings.TrimSpace(captcha.SceneID),
		Region:  strings.TrimSpace(captcha.Region),
		Prefix:  strings.TrimSpace(captcha.Prefix),
	}
}

// ── 套餐探测 ────────────────────────────────────────────────────────────────

// planSnapshot 是一次「账号套餐视图」：上游投放了什么、账号已经持有什么。
//
// 全部取自 billing 族：投放情况在 billing/preview，持有情况与余量在
// billing/balance（billing/current 已废弃，balance 的 data.plans 才是权威）。
// 一次快照就是两个请求，供额度查询、按钮状态与领取前的校验复用。
type planSnapshot struct {
	// offered 是上游当前投放的套餐（按优先级降序）。
	offered []core.PlanInfo
	// held 是账号已持有且仍生效的套餐 ID 集合。
	held map[string]bool
	// plans 是 balance 返回的原始套餐条目（订阅名与状态展示用）。
	plans []map[string]any
	// balances 是 balance 返回的原始额度桶条目（订阅名按到期时刻与套餐关联）。
	balances []map[string]any
	// serverTime 是 balance 响应里的服务端时间（秒，毫秒已归一）；缺失为 0。
	serverTime int64
	// metrics 是 balance 换算出的各模型余量。
	metrics []pluginapi.QuotaMetric
	// groups 是 balance 换算出的窗口组（宿主卡片渲染进度条与到期时间）。
	groups []pluginapi.QuotaGroup
	// errPreview 是 preview 请求的失败原因（balance 失败时 held 为空）。
	errPreview error
	// errBalance 是 balance 请求的失败原因；非空时额度结果不完整。
	errBalance error
	// errActivation 是活跃上报的失败原因，用于解释「为什么一个活动都看不到」。
	errActivation string
}

// claimable 返回尚未领取的投放套餐。
func (s planSnapshot) claimable() []core.PlanInfo {
	out := make([]core.PlanInfo, 0, len(s.offered))
	for _, plan := range s.offered {
		if !s.held[plan.PlanID] {
			out = append(out, plan)
		}
	}
	return out
}

// fetchPlanSnapshot 拉取套餐视图。
//
// 顺序有语义：活跃上报必须**先于** preview——上游按「当日活跃」决定投不投活动，
// 反过来做会让新的一天永远拿不到活动（见 activation.go）。
//
// balance 失败不算整体失败：此时把持有集合当作空（结果偏保守——已领的套餐
// 会被再列成可领，用户点下去拿到上游 1003，语义依然正确），但额度与订阅名
// 会缺失，调用方据 errBalance 决定是否上报失败。preview 失败记进 errPreview。
func (v *adapter) fetchPlanSnapshot(ctx context.Context, cred *Credential) planSnapshot {
	snapshot := planSnapshot{held: map[string]bool{}}
	snapshot.errActivation = v.ensureActivation(ctx, cred)

	code, body, errBalance := billingRequest(ctx, cred, http.MethodGet,
		PathBillingBalance+"?app_version="+url.QueryEscape(ClientAppVersion), nil)
	switch {
	case errBalance != nil:
		snapshot.errBalance = errBalance
	case code != codeSuccess:
		snapshot.errBalance = Classify(0, classifyBillingOutcome(code, body))
	case !billingEnvelopeComplete(body):
		// 业务码成功但缺 balances 数组：这是上游改版或响应被截断，不是
		// 「该账号没有额度」。当作成功会让页面把默认订阅名与空额度当真，
		// 用户看不出异常；按 transient 上报，下一次查询还有机会恢复。
		snapshot.errBalance = transientErr("ZCode billing/balance 响应缺少 balances 数组")
	default:
		snapshot.serverTime = serverSeconds(body)
		snapshot.plans = planRows(body)
		snapshot.balances = objectRows(dataMap(body)["balances"])
		snapshot.held = heldPlanIDs(snapshot.plans, snapshot.serverTime)
		snapshot.metrics, snapshot.groups = balanceQuota(snapshot.balances)
		if expiry := planExpiryGroup(snapshot.plans, snapshot.serverTime); expiry != nil {
			snapshot.groups = append(snapshot.groups, *expiry)
		}
	}

	// 投放有两条通道：billing/preview（计费通道）与 marketing/touch（营销通道）。
	// 国内版实测活动只出现在营销通道，只看计费通道会把「有活动可领」误判成
	// 「已领取全部套餐」——按钮因此一直是灰的。
	//
	// 只有计费通道拿不到投放时才查营销通道：正常情况下额度刷新仍是 balance +
	// preview 两次请求，不给营销端点增加无关流量。
	offered, errPreview := previewOfferedPlans(ctx, cred)
	var marketing []core.PlanInfo
	if errPreview != nil || len(offered) == 0 {
		var errMarketing error
		marketing, errMarketing = queryMarketingTouch(ctx, cred)
		if errMarketing != nil {
			logger.Info("zcode marketing/touch 失败: %v", errMarketing)
		}
		if errPreview != nil && len(marketing) == 0 {
			// 两条都拿不到投放：保留 preview 的失败原因，页面才不会误显示「已领取」。
			snapshot.errPreview = errPreview
			return snapshot
		}
	}
	snapshot.offered = mergeOfferedPlans(offered, marketing)
	return snapshot
}

// previewOfferedPlans 查计费通道投放的套餐，把业务失败码折成错误。
func previewOfferedPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	offered, code, errOffered := fetchOfferedPlans(ctx, cred)
	if errOffered != nil {
		return nil, errOffered
	}
	if code != codeSuccess {
		return nil, claimFailure(code, nil)
	}
	return offered, nil
}

// mergeOfferedPlans 合并两条投放通道的套餐列表。
//
// 同一 plan_id 以计费通道为准（它带完整授予明细），其余按 normalizePlans 的
// 优先级约定重排，保证页面顺序不随上游返回顺序抖动。
func mergeOfferedPlans(primary, extra []core.PlanInfo) []core.PlanInfo {
	seen := make(map[string]bool, len(primary)+len(extra))
	merged := make([]core.PlanInfo, 0, len(primary)+len(extra))
	for _, group := range [][]core.PlanInfo{primary, extra} {
		for _, plan := range group {
			if plan.PlanID == "" || seen[plan.PlanID] {
				continue
			}
			seen[plan.PlanID] = true
			merged = append(merged, plan)
		}
	}
	sortPlansByPriority(merged)
	return merged
}

// planGrantQuota 用套餐授予明细兜底额度展示。
//
// 活动套餐在 effective_at 之前不会生成 balances 桶，上游因此回空额度；但授予
// 量已经写在 plans[].entitlements 里。页面据此仍能显示「剩余 / 总额」并注明
// 生效时刻。balances 非空时不会走到这里。
//
// 只认模型用量类（meter=model_usage、unit_type=token）的授予，与 parsePlanGrants
// 同一口径；按 entitlement_id 去重，避免多套餐重复累加同一份权益。
func planGrantQuota(plans []map[string]any, serverTime int64) ([]pluginapi.QuotaMetric, []pluginapi.QuotaGroup, int64) {
	seen := map[string]bool{}
	group := pluginapi.QuotaGroup{DisplayName: "活动额度"}
	var total float64
	effectiveAt := int64(0)
	for _, plan := range activePlans(plans, serverTime) {
		// 授予量走 parsePlanGrants（与领取弹窗同一口径），生效时刻单独取：
		// PlanGrant 不带时间字段，且只有「尚未生效」的授予需要提示。
		planEffective := pendingEffectiveAt(plan, serverTime)
		if planEffective > 0 && (effectiveAt == 0 || planEffective < effectiveAt) {
			effectiveAt = planEffective
		}
		for _, grant := range parsePlanGrants(plan) {
			if grant.Model == "" || grant.Units <= 0 {
				continue
			}
			// 同一份授予可能出现在多个套餐条目里：按模型 + 授予量去重。
			key := grant.Model + "|" + formatTokens(grant.Units)
			if seen[key] {
				continue
			}
			seen[key] = true
			total += grant.Units
			description := "活动额度 " + formatTokens(grant.Units) + " tokens"
			if planEffective > 0 {
				description += "，" + time.Unix(planEffective, 0).UTC().Format(time.RFC3339) + " 生效"
			}
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
				Window:            grant.Model,
				RemainingFraction: 1,
				Description:       description,
			})
		}
	}
	if total <= 0 {
		return nil, nil, 0
	}
	metrics := []pluginapi.QuotaMetric{
		{Key: "remaining", Label: "剩余", Value: total, Unit: "tokens"},
		{Key: "total", Label: "总量", Value: total, Unit: "tokens"},
	}
	return metrics, []pluginapi.QuotaGroup{group}, effectiveAt
}

// pendingEffectiveAt 返回套餐里最早一个「尚未生效」的授予时刻（Unix 秒）；没有则 0。
func pendingEffectiveAt(plan map[string]any, serverTime int64) int64 {
	earliest := int64(0)
	entitlements, _ := plan["entitlements"].([]any)
	for _, item := range entitlements {
		entry, okEntry := item.(map[string]any)
		if !okEntry {
			continue
		}
		effective := core.GetUnixSeconds(entry, "effective_at")
		if effective <= 0 || serverTime <= 0 || effective <= serverTime {
			continue
		}
		if earliest == 0 || effective < earliest {
			earliest = effective
		}
	}
	return earliest
}

// planRows 取 balance 响应里的 data.plans。
func planRows(body map[string]any) []map[string]any {
	return objectRows(dataMap(body)["plans"])
}

// objectRows 把 JSON 数组里的对象挑出来，忽略其他形态的条目。
func objectRows(value any) []map[string]any {
	items, _ := value.([]any)
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if entry, okEntry := item.(map[string]any); okEntry {
			rows = append(rows, entry)
		}
	}
	return rows
}

// serverSeconds 取响应体里的服务端时间（秒）；缺失时返回 0。
//
// 用服务端时间而不是本机时钟：过期判定要跟上游口径一致，本机时钟漂移会让
// 刚结束的活动继续显示成「已持有」。上游偶尔给 13 位毫秒时间戳，
// core.GetUnixSeconds 负责把两种口径归一到秒。
func serverSeconds(body map[string]any) int64 {
	return core.GetUnixSeconds(dataMap(body), "server_time")
}

// billingEnvelopeComplete 报告 balance 响应是否带齐可解析的额度桶。
//
// 只要求 data.balances 是数组：空数组是合法答案（账号确实没有桶），而字段
// 缺失说明上游改版或响应被截断。plans 不作硬要求——它只影响订阅名展示，
// 把它也判成失败会让「上游少给一个装饰字段」变成查不到额度。
func billingEnvelopeComplete(body map[string]any) bool {
	_, okBalances := dataMap(body)["balances"].([]any)
	return okBalances
}

// heldPlanIDs 从 balance 的 data.plans 里取账号已持有且生效的套餐 ID。
//
// 生效口径见 activePlans（status 缺失视作 active、过 ends_at 视作 expired）：
// status 非 active 的不算持有，否则活动下线后按钮会永远灰着。
func heldPlanIDs(plans []map[string]any, serverTime int64) map[string]bool {
	held := map[string]bool{}
	for _, entry := range activePlans(plans, serverTime) {
		planID := normalizePlanID(core.FirstNonEmpty(
			core.RawStringField(entry, "plan_id"),
			core.RawStringField(entry, "planId"),
		))
		if planID != "" {
			held[planID] = true
		}
	}
	return held
}

// fetchOfferedPlans 拉取上游投放的套餐并按优先级排序。
func fetchOfferedPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, int, error) {
	path := PathBillingPreview +
		"?app_version=" + url.QueryEscape(ClientAppVersion) +
		"&platform=" + url.QueryEscape(cred.Profile.PlatformFull())
	code, body, errRequest := billingRequest(ctx, cred, http.MethodGet, path, nil)
	if errRequest != nil {
		return nil, 0, errRequest
	}
	if code != codeSuccess {
		return nil, code, nil
	}

	return normalizePlans(planRows(body)), codeSuccess, nil
}

// normalizePlans 把原始套餐条目归一化成 PlanInfo 列表。
//
// 丢弃没有 plan_id 的条目、按 plan_id 去重，再按优先级降序 / plan_id 升序排列：
// 同优先级也要有稳定顺序，否则页面展示会随上游返回顺序抖动。billing/preview 与
// marketing/touch 两条链路共用这段约定。
func normalizePlans(rows []map[string]any) []core.PlanInfo {
	plans := make([]core.PlanInfo, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		plan := parsePlan(row)
		if plan == nil || seen[plan.PlanID] {
			continue
		}
		seen[plan.PlanID] = true
		plans = append(plans, *plan)
	}
	sortPlansByPriority(plans)
	return plans
}

// sortPlansByPriority 按优先级降序、plan_id 升序排序。
//
// 两条投放通道（billing/preview 与 marketing/touch）共用同一套顺序约定，
// 否则合并后的列表会随上游返回顺序抖动。
func sortPlansByPriority(plans []core.PlanInfo) {
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Priority != plans[j].Priority {
			return plans[i].Priority > plans[j].Priority
		}
		return plans[i].PlanID < plans[j].PlanID
	})
}

// ProbeActivity 只读探测账号当前可见的活动投放，不执行领取。
//
// 探测允许 API Key 账号参与（如 1024 Dev Day 这类不限 Coding Plan 的活动），
// 因此不走 billingBlockReason（那条通道只服务 JWT）。活跃上报失败只降级成
// Warning：缺 user_id 的 API Key 凭证报不上活跃，但营销通道可能仍有投放，
// 直接失败会让端点对它完全不可用。
func (v *adapter) ProbeActivity(ctx context.Context, cred *core.Credential) (core.ActivityProbeResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.ActivityProbeResult{}, errNative
	}
	if native == nil {
		return core.ActivityProbeResult{}, core.NewPluginError("zcode_credential_missing", "zcode credential is nil", http.StatusBadRequest)
	}
	warning := v.ensureActivation(ctx, native)
	activities, errQuery := queryMarketingTouch(ctx, native)
	if errQuery != nil {
		return core.ActivityProbeResult{}, errQuery
	}
	return core.ActivityProbeResult{Activities: activities, Warning: warning}, nil
}

// PreviewPlans 返回账号当前可领的套餐（已剔除已持有的）。
//
// 空切片表示没有可领的，页面据此把按钮置灰成「已领取」。
func (v *adapter) PreviewPlans(ctx context.Context, cred *core.Credential) ([]core.PlanInfo, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return v.previewPlans(ctx, native)
}

func (v *adapter) previewPlans(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	if reason := billingBlockReason(cred); reason != "" {
		return nil, core.NewPluginError("zcode_no_plan_channel", reason, http.StatusBadRequest)
	}
	snapshot := v.fetchPlanSnapshot(ctx, cred)
	if snapshot.errPreview != nil {
		return nil, snapshot.errPreview
	}
	return snapshot.claimable(), nil
}

// claimStatusOf 把套餐视图折成额度响应里的领取状态。
//
// **「空列表」与「没查到」必须分开**：preview 成功且确实没有投放时才是「已领取」；
// 活跃上报或 preview 失败时说成「已领取」是在骗页面，用户会以为没活动可领而错过。
func claimStatusOf(snapshot planSnapshot) *pluginapi.ClaimStatus {
	status := &pluginapi.ClaimStatus{
		Available: len(snapshot.offered),
		Claimable: len(snapshot.claimable()),
	}
	switch {
	case snapshot.errPreview != nil:
		status.Error = snapshot.errPreview.Error()
	case len(snapshot.offered) == 0 && snapshot.errActivation != "":
		// 活动投放以当日活跃为资格：没报上活跃，空列表不说明「已领完」。
		status.Error = snapshot.errActivation
	}
	return status
}

// parsePlan 归一化一个套餐，缺失 plan_id 时返回 nil。
func parsePlan(raw map[string]any) *core.PlanInfo {
	planID := normalizePlanID(core.FirstNonEmpty(
		core.RawStringField(raw, "plan_id"),
		core.RawStringField(raw, "planId"),
	))
	if planID == "" {
		return nil
	}
	return &core.PlanInfo{
		PlanID:      planID,
		Name:        core.FirstNonEmpty(core.RawStringField(raw, "name"), planID),
		Description: core.RawStringField(raw, "description"),
		Priority:    int(floatField(raw, "priority")),
		Grants:      parsePlanGrants(raw),
		StartsAt:    unixMillis(raw, "starts_at"),
		EndsAt:      unixMillis(raw, "ends_at"),
	}
}

// parsePlanGrants 提取套餐授予的 token 额度明细。
//
// 上游可能直接给 grants[]（已解析形态），也可能给 entitlements[]（原始态）。
// 两种都收，且都为空时返回 nil 而不是空切片——nil 在 JSON 里是"没有"，便于
// 前端区分"没有额度信息"与"额度为 0"。
func parsePlanGrants(raw map[string]any) []core.PlanGrant {
	var grants []core.PlanGrant
	appendGrant := func(name string, units float64, period string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		grants = append(grants, core.PlanGrant{Model: name, Units: units, Period: period})
	}

	if entitlements, okList := raw["entitlements"].([]any); okList {
		for _, item := range entitlements {
			entry, okEntry := item.(map[string]any)
			if !okEntry {
				continue
			}
			// 只认模型用量类的 token 授予：其它计量单位对页面无意义。
			if meter := core.RawStringField(entry, "meter"); meter != "" && meter != "model_usage" {
				continue
			}
			if unit := core.RawStringField(entry, "unit_type"); unit != "" && unit != "token" {
				continue
			}
			appendGrant(
				core.FirstNonEmpty(core.RawStringField(entry, "show_name"), core.RawStringField(entry, "showName")),
				firstPositiveFloat(entry, "grant_units", "grantUnits"),
				core.RawStringField(entry, "period"),
			)
		}
	}
	if len(grants) == 0 {
		if list, okList := raw["grants"].([]any); okList {
			for _, item := range list {
				entry, okEntry := item.(map[string]any)
				if !okEntry {
					continue
				}
				appendGrant(
					core.RawStringField(entry, "name"),
					firstPositiveFloat(entry, "units"),
					core.RawStringField(entry, "period"),
				)
			}
		}
	}
	return grants
}

// firstPositiveFloat 取第一个存在且为正的浮点字段。
func firstPositiveFloat(raw map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value := floatField(raw, key); value > 0 {
			return value
		}
	}
	return 0
}

// normalizePlanID 归一化套餐 ID（上游大小写不统一，去空白后比较）。
func normalizePlanID(planID string) string {
	return strings.ToLower(strings.TrimSpace(planID))
}

// unixMillis 把上游时间戳统一成毫秒；缺失/非正值返回 0。
//
// 秒与毫秒两种口径都由 core.GetUnixSeconds 先归一到秒，避免 13 位毫秒
// 时间戳被再乘 1000 后推到几十年后。
func unixMillis(m map[string]any, keys ...string) int64 {
	seconds := core.GetUnixSeconds(m, keys...)
	if seconds <= 0 {
		return 0
	}
	return seconds * 1000
}

// ── 领取 ────────────────────────────────────────────────────────────────────

// ClaimPlan 领取指定套餐（PlanID 为空时自动挑优先级最高的可领套餐）。
//
// 每次调用都会先做一次实时 preview：领取是用户手动触发的动作，拿到的是
// 点击那一刻的真实可领集合，不复用任何缓存。
func (v *adapter) claimPlan(ctx context.Context, cred *Credential, req core.ClaimPlanRequest) (core.ClaimPlanResponse, error) {
	if reason := billingBlockReason(cred); reason != "" {
		return core.ClaimPlanResponse{}, core.NewPluginError("zcode_no_plan_channel", reason, http.StatusBadRequest)
	}
	if strings.TrimSpace(req.VerifyParam) == "" {
		// 没有令牌就不打上游：必然 3007，白白消耗一次风控计数。
		return core.ClaimPlanResponse{}, core.NewPluginError(
			"zcode_captcha_required", "请先在页面上完成人机验证", http.StatusBadRequest)
	}

	plan, errPick := v.pickClaimPlan(ctx, cred, req.PlanID)
	if errPick != nil {
		return core.ClaimPlanResponse{}, errPick
	}

	if !v.claims.wait(ctx) {
		return core.ClaimPlanResponse{}, core.NewPluginError(
			"zcode_claim_canceled", "领取已取消（等待下一次可提交时间时中断）", http.StatusRequestTimeout)
	}

	headers := map[string]string{CaptchaHeader: strings.TrimSpace(req.VerifyParam)}
	if region := strings.TrimSpace(req.VerifyRegion); region != "" {
		headers[CaptchaRegionHeader] = region
	}
	v.claims.markIssued()

	code, body, errRequest := billingRequestWithHeaders(
		ctx, cred, http.MethodPost, PathBillingClaim,
		map[string]string{"plan_id": plan.PlanID}, headers,
	)
	if errRequest != nil {
		return core.ClaimPlanResponse{}, errRequest
	}
	if code != codeSuccess {
		return core.ClaimPlanResponse{}, claimFailure(code, body)
	}

	// 成功响应带服务端时钟与套餐起止，原样透传给页面展示。
	result := core.ClaimPlanResponse{PlanID: plan.PlanID, PlanName: plan.Name}
	if claimed, okClaimed := dataMap(body)["plan"].(map[string]any); okClaimed {
		result.PlanName = core.FirstNonEmpty(core.RawStringField(claimed, "name"), plan.Name)
		result.StartsAt = unixMillis(claimed, "starts_at")
		result.EndsAt = unixMillis(claimed, "ends_at")
	}
	return result, nil
}

// pickClaimPlan 决定本次领取的目标套餐。
//
// 指定 plan_id 时校验它当前确实可领（预览里存在且未持有）；没指定就取
// 优先级最高的可领项。两者都拿不到候选时报错，避免把无效plan_id 打给上游。
func (v *adapter) pickClaimPlan(ctx context.Context, cred *Credential, planID string) (*core.PlanInfo, error) {
	available, errPreview := v.previewPlans(ctx, cred)
	if errPreview != nil {
		return nil, errPreview
	}
	if len(available) == 0 {
		return nil, core.NewPluginError(
			"zcode_nothing_to_claim", "该账号已领取当前全部可用套餐", http.StatusBadRequest)
	}

	wanted := normalizePlanID(planID)
	if wanted == "" {
		best := available[0]
		return &best, nil
	}
	for index := range available {
		if available[index].PlanID == wanted {
			return &available[index], nil
		}
	}
	return nil, core.NewPluginError(
		"zcode_plan_not_claimable", "该套餐已领取或当前不可领取，请刷新后重试", http.StatusBadRequest)
}

// claimFailure 把领取的业务码翻成对用户可读的错误。
//
// 与额度路径的 classifyBillingOutcome 同源，但领取多了 1003（已领取，按成功
// 语义回报页面好让按钮置灰）与 1005（附名额恢复时间）。
func claimFailure(code int, body map[string]any) error {
	serverMsg := core.FirstNonEmpty(core.RawStringField(body, "msg"), core.RawStringField(body, "message"))
	base := classifyBillingOutcome(code, body)

	// 1005 带名额恢复时间：上游在 data.plan.ends_at 下发，页面据此提示何时再试。
	if code == codeDailyQuotaUsed && serverMsg == "" {
		if nextAt := claimQuotaRecovery(body); nextAt > 0 {
			return core.NewPluginError(
				"zcode_claim_quota_used",
				base+"（名额恢复时间："+formatClaimRecovery(nextAt)+"）",
				http.StatusTooManyRequests,
			)
		}
	}
	return core.NewPluginError("zcode_claim_failed", base, http.StatusBadRequest)
}

// claimQuotaRecovery 从 1005 响应里取名额恢复时间（毫秒）。
func claimQuotaRecovery(body map[string]any) int64 {
	plan, okPlan := dataMap(body)["plan"].(map[string]any)
	if !okPlan {
		return 0
	}
	return unixMillis(plan, "ends_at")
}

// formatClaimRecovery 把恢复时间格式化成「今日/次日 HH:MM:SS」。
func formatClaimRecovery(at int64) string {
	if at <= 0 {
		return "-"
	}
	return time.UnixMilli(at).In(claimTimezone).Format("01-02 15:04:05")
}

// claimTimezone 是名额恢复时间的展示时区（北京时间），
// 与上游「每日 0 点重置」的口径一致。
var claimTimezone = time.FixedZone("Asia/Shanghai", 8*3600)

// CaptchaConfig 实现 core.CaptchaProvider。
func (v *adapter) CaptchaConfig(ctx context.Context) (core.CaptchaConfig, error) {
	return FetchCaptchaConfig(ctx)
}

// ClaimPlan 实现 core.PlanClaimProvider。
func (v *adapter) ClaimPlan(ctx context.Context, cred *core.Credential, req core.ClaimPlanRequest) (core.ClaimPlanResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return core.ClaimPlanResponse{}, errNative
	}
	return v.claimPlan(ctx, native, req)
}

// ---- 活跃上报 ----
//
// 活动投放以「当日活跃」为资格信号：官方桌面端每次启动、每个自然日都会上报
// 埋点，不发这两条事件的账号 preview 恒空。这里只补报，不领取。

// 激活事件上报：让上游把本账号当作「今日活跃的官方客户端」。
//
// 为什么需要它：billing/preview 的返回**不是**纯粹的账号权益快照，活动套餐的投放
// 以「当日活跃」为资格信号。官方桌面端每次启动发 app_launch、每个自然日发一次
// app_daily_active（packages/services/src/telemetry/telemetryCore.ts 的
// reportAppLaunch / reportAppDailyActive）。不发这两条事件的账号，preview 恒空——
// 于是插件算出的可领数为 0，控制台把按钮永久灰成「已领取」，而账号其实一个套餐
// 都没领。这与领取接口本身无关：有活动时手动领取是能成功的。
//
// 事件体严格对齐官方 sendReport 的 16 个字段；端点不在 zcode-plan 之下，也不带
// Authorization（官方该端点同样匿名上报）。

// activationElements 是官方在启动/日活时上报的两个元素名。
var activationElements = []string{"app_launch", "app_daily_active"}

// activationTimeout 是单条埋点的上限：埋点失败不得拖住额度查询。
const activationTimeout = 10 * time.Second

// activationState 是「每个账号每个自然日只上报一次」的进程内去重表。
//
// 状态属于实例而不是落盘：多 App 并存时各自独立（与 claimGate 同一约定）。
// 进程重启后多报一轮无副作用——上游本身按 device_mid + 日期去重。
type activationState struct {
	mu   sync.Mutex
	last map[string]string // uid → 本地日期（账号档案时区）
}

func newActivationState() *activationState {
	return &activationState{last: map[string]string{}}
}

// shouldReport 报告本次是否要上报，并在需要时先占位（避免并发重复上报）。
func (s *activationState) shouldReport(uid string, today string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last[uid] == today {
		return false
	}
	s.last[uid] = today
	return true
}

// forget 撤销占位：上报失败时允许下一次重试。
func (s *activationState) forget(uid string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.last, uid)
	s.mu.Unlock()
}

// localDate 返回账号档案时区下的自然日（YYYY-MM-DD）。
//
// 时区取不到时退回 UTC：去重粒度粗一点没有副作用，只是可能多报一次。
func localDate(profile DeviceProfile, now time.Time) string {
	zone := strings.TrimSpace(profile.Timezone)
	if zone != "" {
		if location, errLoad := time.LoadLocation(zone); errLoad == nil {
			return now.In(location).Format("2006-01-02")
		}
	}
	return now.UTC().Format("2006-01-02")
}

// activationEventBody 构造官方 sendReport 的 16 字段事件体。
func activationEventBody(element string, cred *Credential, userID string) map[string]any {
	profile := cred.Profile
	profile.ensure()
	return map[string]any{
		"event_id":           NewUUID(),
		"client_timezone":    profile.Timezone,
		"client_language":    profile.Language,
		"element_name":       element,
		"event_region":       "app",
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": map[string]any{},
		"user_id":            userID,
		"screen_resolution":  profile.Screen,
		"app_version":        ClientAppVersion,
		"device_os_category": profile.OSCategory(),
		"device_os_version":  profile.OSVersion,
		"device_mid":         profile.DeviceMID,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

// postActivationEvent 上报单条激活事件。
//
// 走 hostbridge.Client 与其他上游请求同一出口；不带 Authorization，只带官方
// 来源头（身份头里与本次上报相关的字段已在事件体里，头本身用于通过网关）。
func postActivationEvent(ctx context.Context, cred *Credential, body map[string]any) error {
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return errMarshal
	}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost,
		ZCodeOrigin+PathEventReport, bytes.NewReader(encoded))
	if errNew != nil {
		return errNew
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("HTTP-Referer", ZCodeOrigin)
	req.Header.Set("X-Title", ClientTitle)

	resp, errDo := hostbridge.Client(ctx, activationTimeout).Do(req)
	if errDo != nil {
		return fmt.Errorf("激活上报失败: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("激活上报 HTTP %d: %s", resp.StatusCode, truncateText(string(raw), 120))
	}
	// 业务码非 0 视为失败；响应非 JSON 时按成功处理（网关可能回空体）。
	var parsed map[string]any
	if errUnmarshal := json.Unmarshal(raw, &parsed); errUnmarshal == nil {
		if code := businessCode(parsed); code != -1 && code != 0 {
			return fmt.Errorf("激活上报业务码 %d: %s", code, truncateText(string(raw), 120))
		}
	}
	return nil
}

// ensureActivation 保证本账号今天已向平台报过活跃，返回失败原因（空串表示无需再报
// 或已成功）。
//
// 失败**不阻断**后续 preview：官方客户端同样不因埋点失败停业务，这里把原因交给
// 调用方决定是展示还是忽略。
func (v *adapter) ensureActivation(ctx context.Context, cred *Credential) string {
	if cred == nil {
		return ""
	}
	userID := strings.TrimSpace(cred.UserID)
	if userID == "" {
		userID = UserIDFromJWT(cred.JWTToken)
	}
	if userID == "" {
		// 无用户身份无法上报（官方事件体以 user_id 归属）；如实说明而不是静默跳过，
		// 否则用户会继续看到一个永远空的活动列表。
		return "凭证缺少 user_id，无法上报活跃事件（活动投放可能不完整；可重新登录或补充 user_id）"
	}

	today := localDate(cred.Profile, time.Now())
	if !v.activations.shouldReport(userID, today) {
		return ""
	}

	for _, element := range activationElements {
		if err := postActivationEvent(ctx, cred, activationEventBody(element, cred, userID)); err != nil {
			v.activations.forget(userID)
			logger.Info("zcode 激活上报失败 (%s): %v", element, err)
			return err.Error()
		}
	}
	return ""
}

// truncateText 截断过长的上游响应片段，避免把整页 HTML 塞进错误消息。
func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// ---- 营销投放 ----
//
// marketing/touch 是 ZCode 的投放通道，与计费族是两条独立链路：只读、不参与
// 领取，且对 API Key 账号同样开放（billing 只服务 JWT 的 Coding Plan）。

// 营销触达（marketing/touch）：ZCode 的投放通道。
//
// 与计费族（billing/*）是两条独立链路：营销通道只读、不参与领取，且对 API Key
// 账号同样开放（billing 只服务 JWT 的 Coding Plan）。投放以「当日活跃」为资格
// 信号，事件上报由调用方在探测前完成（见 activation.go）。

const pathMarketingTouch = "/api/v1/marketing/touch"

// marketingTimeout 是营销投放查询的上限。
//
// 这条通道是额度查询的辅助来源：上游偶发长时间无响应时不能让它把整轮
// 额度查询拖到超时（实测有 60s 以上的挂起）。
const marketingTimeout = 10 * time.Second

// marketingHeaders 复用上游身份与鉴权头；本接口是只读 GET，只额外声明 Accept。
func marketingHeaders(cred *Credential) map[string]string {
	headers := upstreamAuthHeaders(cred)
	headers["Accept"] = "application/json"
	return headers
}

// queryMarketingTouch 拉取账号当前可见的活动投放。
func queryMarketingTouch(ctx context.Context, cred *Credential) ([]core.PlanInfo, error) {
	u, errParse := url.Parse(ZCodeOrigin + pathMarketingTouch)
	if errParse != nil {
		return nil, errParse
	}
	// seq=0 取首屏。上游没有公开游标语义，这里不做猜测性翻页：多翻一轮会把
	// 同一批投放重复拉一遍，反而放大请求量。
	u.RawQuery = "seq=0"

	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if errNew != nil {
		return nil, errNew
	}
	for key, value := range marketingHeaders(cred) {
		req.Header.Set(key, value)
	}

	resp, errDo := hostbridge.Client(ctx, marketingTimeout).Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, billingMaxBody))
	if errRead != nil {
		return nil, errRead
	}
	return parseMarketingTouch(resp.StatusCode, raw)
}

// parseMarketingTouch 解析营销触达响应。
//
// 只认 data.deliveries[].banner.background.args.zcode_plan：活动套餐塞在 banner
// 的渲染参数里，其余投递（纯文案/图片）没有 plan_id，会被 normalizePlans 丢掉。
//
// 不按 starts_at/ends_at 二次过滤：上游已按当前时间筛选投放，本机时钟漂移只会
// 误伤。这里返回的是「上游当前投放」，不是「本账号此刻可领」。
func parseMarketingTouch(status int, raw []byte) ([]core.PlanInfo, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, core.NewPluginError("zcode_activity_response",
			"营销活动响应不是合法 JSON: "+truncateText(string(raw), 120), http.StatusBadGateway)
	}
	if status < 200 || status >= 300 {
		return nil, core.NewPluginError("zcode_activity_http",
			fmt.Sprintf("营销活动查询 HTTP %d: %s", status, truncateText(string(raw), 120)), http.StatusBadGateway)
	}
	// 2xx 且没有业务码时按成功处理：网关可能剥掉信封，与激活上报同一约定。
	if code := businessCode(payload); code != 0 && code != -1 {
		message := core.FirstNonEmpty(core.RawStringField(payload, "msg"), "上游未给出原因")
		return nil, core.NewPluginError("zcode_activity_business",
			fmt.Sprintf("营销活动查询业务码 %d: %s", code, message), http.StatusBadGateway)
	}

	deliveries := objectRows(dataMap(payload)["deliveries"])
	plans := make([]map[string]any, 0, len(deliveries))
	for _, delivery := range deliveries {
		if plan := nestedMap(delivery, "banner", "background", "args", "zcode_plan"); plan != nil {
			plans = append(plans, plan)
		}
	}
	return normalizePlans(plans), nil
}

// nestedMap 沿字段路径逐层取对象，任一层缺失或不是对象时返回 nil。
func nestedMap(raw map[string]any, path ...string) map[string]any {
	for _, key := range path {
		if raw, _ = raw[key].(map[string]any); raw == nil {
			return nil
		}
	}
	return raw
}
