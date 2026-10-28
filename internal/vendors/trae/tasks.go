package trae

// 本文件实现 Trae 家族的每日签到与额度查询。
//
// 签到（仅国内版；国际版无此活动）：
//   - 先 POST checkin_credits/status 查今日状态（enable / checked_in / credits）；
//   - 未签到再 POST checkin_credits/claim 领取。claim 对「今日已签到」同样返回
//     code=0，因此「已签到」只能由 status 判定，status 预查是必要流程。
//   - 业务码 9074 是设备级限流（不是已签到），按限流错误上抛。
//
// 额度（仅国内版）：POST pay/ide_user_ent_usage 取权益包列表，
// 只统计 product_type=2 的积分包（credits_limit=-1 表示无限）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

const (
	// checkinDeviceBrand/Type 与 Trae2api-cn 的默认签到设备一致。
	checkinDeviceBrand = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	checkinDeviceType  = "windows"
)

func SupportsCheckin(cfg Config) bool { return cfg.supportsCheckin() }

type CheckinResult struct {
	Already bool
	Credit  int64
	Message string
}

// Checkin 执行每日签到。
func Checkin(ctx context.Context, cred *Credential, cfg Config) (*CheckinResult, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if !cfg.supportsCheckin() {
		return nil, fmt.Errorf("trae 国际版无签到活动")
	}

	headers := checkinHeaders(cred)

	status, errStatus := fetchCheckinStatus(ctx, cfg, headers)
	if errStatus != nil {
		return nil, errStatus
	}
	if !status.Enable {
		return nil, fmt.Errorf("签到活动未开启")
	}
	if status.CheckedIn {
		return &CheckinResult{Already: true, Credit: status.Credits, Message: "今日已完成签到"}, nil
	}

	if errClaim := claimCheckinCredits(ctx, cfg, headers); errClaim != nil {
		return nil, errClaim
	}
	return &CheckinResult{Credit: status.Credits, Message: "签到成功"}, nil
}

type checkinStatus struct {
	Enable    bool
	CheckedIn bool
	Credits   int64
}

func fetchCheckinStatus(ctx context.Context, cfg Config, headers map[string]string) (*checkinStatus, error) {
	raw, errPost := postCheckin(ctx, cfg.CheckinHost+EpCheckinStatus, headers)
	if errPost != nil {
		return nil, errPost
	}
	var result struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Enable    bool   `json:"enable"`
		CheckedIn bool   `json:"checked_in"`
		Credits   int64  `json:"credits"`
	}
	if errJSON := json.Unmarshal(raw, &result); errJSON != nil {
		return nil, fmt.Errorf("decode checkin status: %w", errJSON)
	}
	if result.Code != 0 {
		return nil, Classify(http.StatusOK, string(raw))
	}
	return &checkinStatus{Enable: result.Enable, CheckedIn: result.CheckedIn, Credits: result.Credits}, nil
}

func claimCheckinCredits(ctx context.Context, cfg Config, headers map[string]string) error {
	raw, errPost := postCheckin(ctx, cfg.CheckinHost+EpCheckinClaim, headers)
	if errPost != nil {
		return errPost
	}
	var result struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if errJSON := json.Unmarshal(raw, &result); errJSON != nil {
		return fmt.Errorf("decode checkin claim: %w", errJSON)
	}
	if result.Code == 0 {
		return nil
	}
	// 9074 = 设备级限流；其余业务码原样分类上抛。
	return Classify(http.StatusOK, string(raw))
}

func postCheckin(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if errReq != nil {
		return nil, errReq
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("trae checkin request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, Classify(resp.StatusCode, string(body))
	}
	return body, nil
}

// checkinHeaders 装配签到/额度共用的请求头。
func checkinHeaders(cred *Credential) map[string]string {
	return map[string]string{
		"Authorization":  "Cloud-IDE-JWT " + cred.AccessToken,
		"Content-Type":   "application/json",
		"X-Device-Id":    CheckinDeviceID(cred),
		"X-Device-Brand": checkinDeviceBrand,
		"X-Device-Type":  checkinDeviceType,
	}
}

// CheckinDeviceID 派生签到用的 16 位设备号：sha256(账号身份) mod 10^16。
//
// 身份优先取 JWT payload.data.id（与 Trae2api-cn 一致，token 刷新后保持稳定），
// 退回凭证 uid / device_id。
func CheckinDeviceID(cred *Credential) string {
	identity := jwtIdentity(cred.AccessToken)
	if identity == "" {
		identity = cred.UID
	}
	if identity == "" {
		identity = cred.DeviceID
	}
	if identity == "" {
		identity = "default_trae_user"
	}
	sum := sha256.Sum256([]byte(identity))
	n := new(big.Int).SetBytes(sum[:])
	n.Mod(n, big.NewInt(10000000000000000))
	return fmt.Sprintf("%016d", n)
}

// jwtIdentity 从 Cloud-IDE-JWT 解出账号 id（payload.data.id）。
func jwtIdentity(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return ""
	}
	var parsed struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if errJSON := json.Unmarshal(payload, &parsed); errJSON != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Data.ID)
}

// ---- 额度 ----

// FetchQuota 查询账号积分：剩余 / 总额 / 已用。
func FetchQuota(ctx context.Context, cred *Credential, cfg Config) (*pluginapi.QuotaFetchResponse, error) {
	if cred == nil || cred.AccessToken == "" {
		return nil, fmt.Errorf("credential missing")
	}
	if cfg.QuotaHost == "" {
		// 国际版上游没有额度端点（实测 404）。
		return nil, core.NewPluginError("quota_unsupported", "Trae 国际版无额度查询接口", http.StatusNotImplemented)
	}

	payload, errBody := json.Marshal(map[string]any{"require_usage": true, "req_source": 1})
	if errBody != nil {
		return nil, errBody
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.QuotaHost+EpEntUsage, bytes.NewReader(payload))
	if errReq != nil {
		return nil, errReq
	}
	for key, value := range checkinHeaders(cred) {
		req.Header.Set(key, value)
	}

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("trae quota request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				ProductType int `json:"product_type"`
				Quota       struct {
					CreditsLimit any `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount any `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if errJSON := json.Unmarshal(body, &result); errJSON != nil {
		return nil, fmt.Errorf("decode quota response: %w", errJSON)
	}

	var total, used float64
	unlimited := false
	for _, pack := range result.UserEntitlementPackList {
		// product_type=0 是功能开关包，没有积分额度。
		if pack.EntitlementBaseInfo.ProductType != 2 {
			continue
		}
		limit, okLimit := flexFloat(pack.EntitlementBaseInfo.Quota.CreditsLimit)
		if !okLimit {
			continue
		}
		if limit < 0 {
			unlimited = true
			continue
		}
		total += limit
		if amount, okAmount := flexFloat(pack.Usage.CreditsAmount); okAmount && amount > 0 {
			used += amount
		}
	}

	// 键名必须落在 auth.Totals 的词表内（credit_remain / credit_total /
	// credit_used），否则控制台的「剩余 / 总额」会静默显示 0 / 0。
	// 供应商包不能 import auth（分层约定），所以这里按约定手写同名键。
	response := &pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     "Trae 国内版权益",
			TierName: "Standard",
			TierID:   cfg.VendorID,
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: total - used, Unit: "credits"},
			{Key: "credit_total", Label: "总额度", Value: total, Unit: "credits"},
			{Key: "credit_used", Label: "已用积分", Value: used, Unit: "credits"},
		},
	}
	if unlimited {
		response.Summary[0] = pluginapi.QuotaMetric{Key: "credit_remain", Label: "剩余积分", Value: -1, Unit: "unlimited"}
	}
	return response, nil
}

// flexFloat 宽容解析上游数字字段：接受 JSON 数字与数字字符串，
// 键缺失 / 空串 / null 返回 false（调用方按 0 或跳过处理）。
func flexFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, errParse := strconv.ParseFloat(trimmed, 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}
