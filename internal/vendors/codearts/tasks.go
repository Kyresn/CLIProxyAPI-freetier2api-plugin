package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
	// NotRecorded 表示本次没有真正领到积分（未开通 / 无活动 / 不可领取）。
	// 调用方不应把它记成「今日已签到」。
	NotRecorded bool
}

func SupportsCheckin() bool {
	return true
}

// ---- 每日签到（积分活动）----
//
// 协议来自对码道 IDE 的逆向与参考实现：
//   GET  {snap}/snap-manager/v1/statistics/plugin      账户/套餐（判断积分账户）
//   GET  {snap}/v1/ops/delivery?channel=IDE            活动列表
//   POST {snap}/v1/ops/claim   {campaignId, channel}   领取
//   POST {snap}/v1/ops/confirm {campaignId}            确认（响应 data.id 非空时）
//
// 该协议没有幂等键、也没有「今天已签到」业务码，幂等只能靠活动列表预检。

// accountInfo 是账户/套餐信息里签到与额度需要的字段。
type accountInfo struct {
	Package struct {
		IsCreditPackage bool   `json:"is_credit_package"`
		IsTokenPackage  bool   `json:"is_token_package"`
		Status          string `json:"status"`
		SpecCode        string `json:"spec_code"`
		PackageNameCN   string `json:"package_name_cn"`
		PackageNameEN   string `json:"package_name_en"`
	} `json:"package"`
	Metrics []accountMetric `json:"metrics"`
}

// accountMetric 是 statistics/plugin 里的一项额度指标。
//
// 字段名来自 IDE 逆向：package_credit_amount / used / remain。数值可能是
// 数字或数字字符串，故用 any 承接后统一解析。
type accountMetric struct {
	Name   string `json:"name"`
	Amount any    `json:"package_credit_amount"`
	Used   any    `json:"package_credit_used"`
	Remain any    `json:"package_credit_remain"`
}

// creditMetricLabels 是积分指标名到展示名的映射。
var creditMetricLabels = map[string]string{
	"usageTotalPackageCredit":    "总积分包",
	"usageBasicPackageCredit":    "基础积分包",
	"usageOnDemandPackageCredit": "按需积分包",
	"usageBonusPackageCredit":    "赠送积分包",
}

// totalCreditMetric 是总额指标名（其余分类是它的构成明细，不能重复累加）。
const totalCreditMetric = "usageTotalPackageCredit"

// opsActivity 是活动列表里的每日签到条目。
type opsActivity struct {
	CampaignID string
	Type       string
	Claimable  bool
	Status     string
	Amount     int64
}

// claimedStatuses 是活动已领取 / 已核销的状态值（IDE 据此禁用领取按钮）。
var claimedStatuses = map[string]bool{"CLAIMED": true, "CONFIRMED": true, "CONSUMED": true}

// Checkin 执行每日签到（积分活动）。
func Checkin(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, fmt.Errorf("credential is nil or missing ak")
	}

	info, errInfo := fetchAccountInfo(ctx, cred)
	if errInfo != nil {
		// 账户信息查不到时不能直接判失败：Token 计费账户本来就不在积分活动
		// 范围，仍可能走福利 Token 通道。兜底也失败时再报原始错误。
		if result, errFallback := claimBenefitTokenFallback(ctx, cred, false); errFallback == nil {
			return result, nil
		}
		return nil, errInfo
	}
	if !info.Package.IsCreditPackage {
		// Token 计费账户不在积分活动范围，但仍可能有每日免费福利 Token。
		return claimBenefitTokenFallback(ctx, cred, info.Package.IsTokenPackage)
	}

	activity, errActivity := fetchDailyActivity(ctx, cred)
	if errActivity != nil {
		return nil, errActivity
	}
	if activity == nil {
		return &CheckinResult{NotRecorded: true, Message: "未找到每日签到活动"}, nil
	}
	if !activity.Claimable {
		if claimedStatuses[strings.ToUpper(activity.Status)] {
			return &CheckinResult{Already: true, Message: "今天已领取"}, nil
		}
		return &CheckinResult{
			NotRecorded: true,
			Message:     fmt.Sprintf("当前不可领取（status=%s）", activity.Status),
		}, nil
	}
	if activity.CampaignID == "" {
		return nil, fmt.Errorf("签到活动缺少 campaignId，无法领取")
	}

	claim, errClaim := claimOps(ctx, cred, activity.CampaignID)
	if errClaim != nil {
		return nil, errClaim
	}
	// 服务端要求确认时才补 confirm（判据：响应 data.id 非空）。确认失败不改变
	// 「已领取」的事实：积分已进入待确认态，报失败会让用户重复点击。
	if claim.benefitID != "" {
		_, _ = signedSnapJSON(ctx, http.MethodPost, SnapEngineApiHost+EpOpsConfirm, cred,
			[]byte(`{"campaignId":"`+jsonEscape(activity.CampaignID)+`"}`))
	}

	credit := claim.credit
	if credit == 0 {
		credit = activity.Amount
	}
	message := "签到成功"
	if credit > 0 {
		message = fmt.Sprintf("签到成功 +%d 积分", credit)
	}
	return &CheckinResult{Credit: credit, Message: message}, nil
}

// opsClaimResult 是一次领取响应的关键字段。
type opsClaimResult struct {
	credit    int64
	benefitID string
}

// claimOps 调用 /v1/ops/claim，body 与官方客户端逐字节一致。
func claimOps(ctx context.Context, cred *Credential, campaignID string) (*opsClaimResult, error) {
	body := []byte(`{"campaignId":"` + jsonEscape(campaignID) + `","channel":"` + OpsChannel + `"}`)
	data, errClaim := signedSnapJSON(ctx, http.MethodPost, SnapEngineApiHost+EpOpsClaim, cred, body)
	if errClaim != nil {
		return nil, errClaim
	}
	result := &opsClaimResult{
		credit: firstInt(data, "benefitAmount", "credit", "credits", "creditAmount", "amount"),
	}
	if id, okID := data["id"]; okID && id != nil {
		if text := strings.TrimSpace(fmt.Sprintf("%v", id)); text != "" && text != "null" {
			result.benefitID = text
		}
	}
	return result, nil
}

// fetchAccountInfo 查询账户/套餐信息（判断是否积分计费账户）。
func fetchAccountInfo(ctx context.Context, cred *Credential) (*accountInfo, error) {
	data, errFetch := signedSnapJSON(ctx, http.MethodGet, SnapEngineApiHost+EpPackageInfo, cred, nil)
	if errFetch != nil {
		return nil, fmt.Errorf("账户信息查询失败: %w", errFetch)
	}
	encoded, _ := json.Marshal(data)
	var info accountInfo
	if errDecode := json.Unmarshal(encoded, &info); errDecode != nil {
		return nil, fmt.Errorf("decode account info: %w", errDecode)
	}
	return &info, nil
}

// fetchDailyActivity 查询活动列表并挑出每日登录领取条目。
func fetchDailyActivity(ctx context.Context, cred *Credential) (*opsActivity, error) {
	url := SnapEngineApiHost + EpOpsDelivery + "?channel=" + OpsChannel
	data, errFetch := signedSnapJSON(ctx, http.MethodGet, url, cred, nil)
	if errFetch != nil {
		return nil, fmt.Errorf("活动列表查询失败: %w", errFetch)
	}
	items, okItems := data["items"].([]any)
	if !okItems {
		return nil, fmt.Errorf("活动列表响应缺少 items 字段")
	}
	for _, item := range items {
		record, okRecord := item.(map[string]any)
		if !okRecord || text(record, "type") != OpsDailyLoginType {
			continue
		}
		return &opsActivity{
			CampaignID: identifier(record, "campaignId"),
			Type:       text(record, "type"),
			Claimable:  boolValue(record, "claimable"),
			Status:     text(record, "status"),
			Amount:     firstInt(record, "benefitAmount", "amount", "creditAmount"),
		}, nil
	}
	return nil, nil
}

// signedSnapJSON 发一次 snap-access 签名请求并解开 {code,message,data} 信封。
func signedSnapJSON(ctx context.Context, method, url string, cred *Credential, body []byte) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, errReq := http.NewRequestWithContext(ctx, method, url, reader)
	if errReq != nil {
		return nil, errReq
	}
	signSnapRequest(req, body, cred)

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("snap request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read snap response: %w", errRead)
	}
	if resp.StatusCode >= 400 {
		// 走统一分类：STS 过期的 400（APIG.0602）必须归成可续期的凭证错误，
		// 宿主的保活重试才会先刷新再重查。
		return nil, Classify(resp.StatusCode, string(raw))
	}
	var envelope map[string]any
	if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
		return nil, fmt.Errorf("decode snap response: %w", errDecode)
	}
	if code, okCode := envelope["code"].(float64); okCode && code != 0 {
		message := core.FirstNonEmpty(text(envelope, "message"), text(envelope, "msg"))
		if message == "" {
			message = fmt.Sprintf("业务码 %v", code)
		}
		return nil, fmt.Errorf("snap business error: %s", message)
	}
	if data, okData := envelope["data"].(map[string]any); okData {
		return data, nil
	}
	return envelope, nil
}

// text / boolValue / identifier / firstInt 是对上游 JSON 的宽容取值助手。
func text(source map[string]any, key string) string {
	value, okValue := source[key].(string)
	if !okValue {
		return ""
	}
	return strings.TrimSpace(value)
}

func boolValue(source map[string]any, key string) bool {
	switch value := source[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	}
	return false
}

// identifier 把标识符字段统一成字符串（服务端可能下发数字，如 campaignId=1）。
func identifier(source map[string]any, key string) string {
	switch value := source[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return fmt.Sprintf("%.0f", value)
	case json.Number:
		return value.String()
	}
	return ""
}

// firstInt 按候选键顺序取第一个非零整数（兼容数字字符串）。
func firstInt(source map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if parsed := int64(core.IntFromAny(source[key])); parsed != 0 {
			return parsed
		}
	}
	return 0
}

// jsonEscape 把字符串转成 JSON 字符串字面量内容（不含引号）。
func jsonEscape(value string) string {
	encoded, _ := json.Marshal(value)
	return strings.Trim(string(encoded), `"`)
}

// ---- Token 计费账户的福利 Token 兜底 ----
//
// 积分活动只对「已升级到积分计费模式」的账号开放。Token 计费账户仍走旧的
// 福利 Token 通道（每日 1000 万），两条链路互不替代。

// BenefitGatewayStatus 报告福利通道是否开放。
type BenefitGatewayStatus struct {
	Enabled                 bool `json:"enabled"`
	IsExaminationDomain     bool `json:"is_examination_domain"`
	ExaminationModelEnabled bool `json:"examination_model_enable"`
}

// BenefitClaim 是一次福利领取的结果。
type BenefitClaim struct {
	Channel  string `json:"channel"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
}

// claimBenefitTokenFallback 为 Token 计费账户领取每日免费福利 Token。
func claimBenefitTokenFallback(ctx context.Context, cred *Credential, isTokenAccount bool) (*CheckinResult, error) {
	status, errStatus := fetchBenefitGatewayStatus(ctx, cred)
	if errStatus != nil {
		// 福利通道查询失败时，至少把「不在积分活动范围」的事实说清楚。
		prefix := "非积分计费账户"
		if isTokenAccount {
			prefix = "Token 计费账户"
		}
		return nil, fmt.Errorf("%s，且福利通道查询失败: %w", prefix, errStatus)
	}
	if !status.Enabled {
		return &CheckinResult{
			NotRecorded: true,
			Message:     "当前账号未开放每日签到（既不是积分计费账户，也无福利 Token 活动）",
		}, nil
	}
	claim, errClaim := claimBenefitToken(ctx, cred)
	if errClaim != nil {
		return nil, errClaim
	}
	message := "已领取当日免费福利 Token（每日 1000 万，当日 24:00 清零）"
	if claim.UserName != "" {
		message = "已为 " + claim.UserName + " 领取当日免费福利 Token"
	}
	return &CheckinResult{Message: message}, nil
}

// fetchBenefitGatewayStatus 查询福利通道是否开放（snap 主机）。
func fetchBenefitGatewayStatus(ctx context.Context, cred *Credential) (*BenefitGatewayStatus, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, SnapEngineApiHost+EpBenefitGateway, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit gateway status failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var status BenefitGatewayStatus
	if errUnmarshal := json.Unmarshal(body, &status); errUnmarshal != nil {
		return nil, fmt.Errorf("decode benefit gateway status: %w", errUnmarshal)
	}
	return &status, nil
}

// claimBenefitToken 调用福利领取入口（幂等 GET）。
func claimBenefitToken(ctx context.Context, cred *Credential) (*BenefitClaim, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitClaim, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts claim benefit failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("claim benefit response %d: %s", resp.StatusCode, DescribeClaimError(string(body)))
	}
	var envelope struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			Channel  string `json:"channel"`
			UserID   string `json:"user_id"`
			UserName string `json:"user_name"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("decode claim benefit response: %w", errUnmarshal)
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("claim benefit %s: %s", envelope.ErrorCode, DescribeClaimError(envelope.ErrorMsg))
	}
	return &BenefitClaim{
		Channel:  core.FirstNonEmpty(envelope.Result.Channel, "codearts"),
		UserID:   envelope.Result.UserID,
		UserName: envelope.Result.UserName,
	}, nil
}

// DescribeClaimError 把福利接口的错误码翻成人话。
func DescribeClaimError(message string) string {
	switch {
	case strings.Contains(message, "APIG.0602"):
		return "凭据（STS security token）已过期，无法领取；请重新登录或等保活刷新"
	case strings.Contains(message, "APIG.0301"), strings.Contains(message, "APIG.0303"):
		return "签名校验失败，凭据可能已失效"
	case strings.Contains(message, "4006"):
		return "今日福利名额已用完"
	case strings.Contains(message, "4004"):
		return "福利权益不存在或已过期"
	case strings.Contains(message, "4003"):
		return "账号已被禁用"
	}
	return message
}

// FetchQuota 查询账号额度。
//
// 积分计费账户用 statistics/plugin 的 metrics（与签到同一套 snap 协议）；
// 福利 Token 接口只适用于 Token 计费账户。此前对所有账号都打福利接口，
// 积分账户会得到 `4000 invalid channel, group not found`，额度永远刷不出来。
func FetchQuota(ctx context.Context, cred *Credential) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessKeyID == "" {
		return nil, fmt.Errorf("credential is nil or missing ak")
	}

	info, errInfo := fetchAccountInfo(ctx, cred)
	if errInfo == nil && info.Package.IsCreditPackage {
		if quota, okQuota := creditQuota(info); okQuota {
			return quota, nil
		}
		// 积分账户但上游没下发任何积分指标：如实显示 0，而不是回退到
		// 不适用的福利接口（那只会得到 invalid channel）。
		return noCreditQuota(info), nil
	}

	balance, errBalance := fetchBenefitBalance(ctx, cred)
	if errBalance != nil {
		if errInfo != nil {
			return nil, fmt.Errorf("账户信息查询失败: %w", errInfo)
		}
		return nil, errBalance
	}
	if balance == nil {
		// 上游对「没有福利权益」的账号回 4004 benefit not found。那是**账号状态**
		// 而不是插件故障（同一账号的福利模型清单能正常列出、签到也返回成功），
		// 如实回报 0/0 并写清原因，好过让整列显示成「-- / --」让人以为查询失败。
		return noBenefitQuota(), nil
	}

	dailyLeft := balance.DailyTokenLimit - balance.DailyTokensUsed
	if dailyLeft < 0 {
		dailyLeft = 0
	}
	monthlyLeft := balance.MonthlyTokenLimit - balance.MonthlyTokensUsed
	if monthlyLeft < 0 {
		monthlyLeft = 0
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts 每日免费福利 Token",
			TierName: core.FirstNonEmpty(balance.Channel, "benefit"),
			TierID:   "benefit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "每日免费额度", Value: float64(balance.DailyTokenLimit)},
			{Key: "remaining", Label: "今日剩余", Value: float64(dailyLeft)},
			{Key: "used", Label: "今日已用", Value: float64(balance.DailyTokensUsed)},
			{Key: "benefit_monthly_total", Label: "每月额度", Value: float64(balance.MonthlyTokenLimit)},
			{Key: "benefit_monthly_remaining", Label: "本月剩余", Value: float64(monthlyLeft)},
		},
	}, nil
}

// creditQuota 把 statistics/plugin 的 metrics 转成宿主的额度结构。
//
// 总额取 usageTotalPackageCredit（分类是它的构成明细，不能重复累加）；
// 总额缺失时才回退为分类求和。没有任何积分指标时 ok=false。
func creditQuota(info *accountInfo) (*pluginapi.QuotaFetchResponse, bool) {
	if info == nil {
		return nil, false
	}
	var (
		totalAmount, totalUsed, totalRemain float64
		sawTotal                            bool
		sawMetric                           bool
		sumAmount, sumUsed, sumRemain       float64
	)
	buckets := make([]pluginapi.QuotaBucket, 0, len(info.Metrics))
	for _, metric := range info.Metrics {
		label, okLabel := creditMetricLabels[strings.TrimSpace(metric.Name)]
		if !okLabel {
			continue
		}
		sawMetric = true
		amount := core.FloatFromAny(metric.Amount)
		used := core.FloatFromAny(metric.Used)
		remain := core.FloatFromAny(metric.Remain)
		sumAmount += amount
		sumUsed += used
		sumRemain += remain
		if strings.TrimSpace(metric.Name) == totalCreditMetric {
			totalAmount, totalUsed, totalRemain = amount, used, remain
			sawTotal = true
		}
		// 额度为 0 的分类不列资源包：列出来只会让「N 个资源包」虚高。
		if amount <= 0 && remain <= 0 {
			continue
		}
		fraction := 0.0
		if amount > 0 {
			fraction = core.ClampFraction(remain / amount)
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window:            label,
			RemainingFraction: fraction,
			Description:       fmt.Sprintf("剩余 %.2f / 总额 %.2f", remain, amount),
		})
	}
	if !sawMetric {
		return nil, false
	}
	if !sawTotal {
		totalAmount, totalUsed, totalRemain = sumAmount, sumUsed, sumRemain
	}
	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts 积分",
			TierName: core.FirstNonEmpty(info.Package.PackageNameCN, info.Package.PackageNameEN, "积分计费"),
			TierID:   core.FirstNonEmpty(info.Package.SpecCode, "credit"),
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "积分总额", Value: totalAmount},
			{Key: "remaining", Label: "剩余积分", Value: totalRemain},
			{Key: "used", Label: "已用积分", Value: totalUsed},
		},
	}
	if len(buckets) > 0 {
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "积分包明细",
			Buckets:     buckets,
		}}
	}
	return response, true
}

// noCreditQuota 是积分账户但上游未返回指标时的额度结果。
func noCreditQuota(info *accountInfo) *pluginapi.QuotaFetchResponse {
	tierName := "积分计费"
	specCode := "credit"
	if info != nil {
		tierName = core.FirstNonEmpty(info.Package.PackageNameCN, info.Package.PackageNameEN, tierName)
		specCode = core.FirstNonEmpty(info.Package.SpecCode, specCode)
	}
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "CodeArts 积分", TierName: tierName, TierID: specCode},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "积分总额（上游未返回积分指标）", Value: 0},
			{Key: "remaining", Label: "剩余积分", Value: 0},
			{Key: "used", Label: "已用积分", Value: 0},
		},
	}
}

// noBenefitQuota 是「账号没有福利权益」时的额度结果。
func noBenefitQuota() *pluginapi.QuotaFetchResponse {
	return &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "CodeArts 每日免费福利 Token",
			TierName: "未开通",
			TierID:   "benefit",
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "total", Label: "每日免费额度（上游未开通福利权益，无可查额度）", Value: 0},
		},
	}
}

// benefitBalance 是福利通道的额度数据。
type benefitBalance struct {
	Channel           string
	DailyTokenLimit   int64
	DailyTokensUsed   int64
	MonthlyTokenLimit int64
	MonthlyTokensUsed int64
}

// fetchBenefitBalance 查询福利额度。
//
// 返回 (nil, nil) 表示上游明确回「该账号没有福利记录」——不是错误，是答案。
func fetchBenefitBalance(ctx context.Context, cred *Credential) (*benefitBalance, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, BenefitHost+EpBenefitBalance, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Agent-Type", "PromptCenter")
	signHuaweiRequest(req, nil, cred)

	client := hostbridge.Client(ctx, 20*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("codearts benefit balance failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	var envelope struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
		Result    struct {
			Channel           string `json:"channel"`
			DailyTokenLimit   int64  `json:"daily_token_limit"`
			DailyTokensUsed   int64  `json:"daily_tokens_used"`
			MonthlyTokenLimit int64  `json:"monthly_token_limit"`
			MonthlyTokensUsed int64  `json:"monthly_tokens_used"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode benefit balance: %w", err)
	}
	// 0000 是成功码；4004 是「没有福利记录」，两者都不是错误退出。
	if envelope.ErrorCode == "4004" || strings.Contains(envelope.ErrorMsg, "benefit not found") {
		return nil, nil
	}
	if envelope.ErrorCode != "" && envelope.ErrorCode != "0000" {
		return nil, fmt.Errorf("benefit balance %s: %s", envelope.ErrorCode, DescribeClaimError(envelope.ErrorMsg))
	}
	return &benefitBalance{
		Channel:           envelope.Result.Channel,
		DailyTokenLimit:   envelope.Result.DailyTokenLimit,
		DailyTokensUsed:   envelope.Result.DailyTokensUsed,
		MonthlyTokenLimit: envelope.Result.MonthlyTokenLimit,
		MonthlyTokensUsed: envelope.Result.MonthlyTokensUsed,
	}, nil
}
