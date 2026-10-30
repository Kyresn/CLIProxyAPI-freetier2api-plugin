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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
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

	headers, errHeaders := checkinHeaders(cred)
	if errHeaders != nil {
		return nil, errHeaders
	}

	// 状态预查失败不阻断领取：null 只表示「无法确认」，不是「不能领」。
	// 把瞬时故障当成签到失败会让账号整天漏签。
	status, errStatus := fetchCheckinStatus(ctx, cfg, headers)
	if errStatus == nil && status != nil {
		if !status.Enable {
			return nil, fmt.Errorf("签到活动未开启")
		}
		if status.CheckedIn {
			return &CheckinResult{Already: true, Credit: status.Credits, Message: "今日已完成签到"}, nil
		}
	}

	if errClaim := claimCheckinCredits(ctx, cfg, headers); errClaim != nil {
		return nil, errClaim
	}
	// claim 响应不含积分数；领取成功后补查一次状态，报告真实所得。
	credit := int64(0)
	if status != nil {
		credit = status.Credits
	}
	if fresh, errFresh := fetchCheckinStatus(ctx, cfg, headers); errFresh == nil && fresh != nil {
		credit = fresh.Credits
	}
	return &CheckinResult{Credit: credit, Message: "签到成功"}, nil
}

// flexInt64 宽容解析业务码/积分：接受 JSON 数字与数字字符串。
//
// 上游网关对同一字段可能下发 0 或 "0"；用 int 直接解码会因类型不符整条
// 响应解析失败，把一次成功签到报成 decode 错误。
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(data []byte) error {
	trimmed := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if trimmed == "" || trimmed == "null" {
		*f = 0
		return nil
	}
	parsed, errParse := strconv.ParseFloat(trimmed, 64)
	if errParse != nil {
		return errParse
	}
	*f = flexInt64(parsed)
	return nil
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
		Code      flexInt64 `json:"code"`
		Message   string    `json:"message"`
		Enable    bool      `json:"enable"`
		CheckedIn bool      `json:"checked_in"`
		Credits   flexInt64 `json:"credits"`
	}
	if errJSON := json.Unmarshal(raw, &result); errJSON != nil {
		return nil, fmt.Errorf("decode checkin status: %w", errJSON)
	}
	if result.Code != 0 {
		// 业务码错误的 HTTP 状态是 200，但传 0 才能让下游按业务类型处理
		// （传 200 会让 Failure.HTTPStatus() 返回 200，限流/凭证策略不生效）。
		return nil, Classify(0, string(raw))
	}
	return &checkinStatus{Enable: result.Enable, CheckedIn: result.CheckedIn, Credits: int64(result.Credits)}, nil
}

func claimCheckinCredits(ctx context.Context, cfg Config, headers map[string]string) error {
	// 只在传输层失败（没有 HTTP 响应）时重试；业务码错误重试无意义。
	var lastErr error
	for attempt := 0; attempt <= 3; attempt++ {
		raw, errPost := postCheckin(ctx, cfg.CheckinHost+EpCheckinClaim, headers)
		if errPost != nil {
			if _, isUpstream := errPost.(*core.Failure); isUpstream {
				return errPost
			}
			lastErr = errPost
			if attempt < 3 && !core.SleepContext(ctx, time.Second) {
				return ctx.Err()
			}
			continue
		}
		var result struct {
			Code    flexInt64 `json:"code"`
			Message string    `json:"message"`
		}
		if errJSON := json.Unmarshal(raw, &result); errJSON != nil {
			return fmt.Errorf("decode checkin claim: %w", errJSON)
		}
		if result.Code == 0 {
			return nil
		}
		// 9074 = 设备级限流；其余业务码原样分类上抛（状态传 0，见 fetchCheckinStatus）。
		return Classify(0, string(raw))
	}
	return lastErr
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

// checkinHeaders 装配签到/额度共用的完整客户端请求头。
//
// 参考实现（trae-mate）实测：签到端点接受的是**约 20 个头的官方客户端形态**，
// 只发 Authorization + 设备头会被上游拒绝或归到不同设备配额。设备身份必须
// 基于账号 uid 确定性派生：同一账号稳定、不同账号互不共享（上游按设备限流）。
func checkinHeaders(cred *Credential) (map[string]string, error) {
	uid := traeUserID(cred)
	if uid == "" {
		return nil, fmt.Errorf("trae: 缺少账号 uid，无法派生签到设备身份")
	}
	return checkinHeadersForUID(cred, uid), nil
}

// quotaHeaders 是额度查询的头：没有 uid 时退回凭证设备号，不让额度查询失败。
//
// 签到必须拒绝共享默认设备身份（上游按设备限流）；额度只读接口没有这个
// 约束，用一个稳定的账号级标识即可。
func quotaHeaders(cred *Credential) map[string]string {
	if uid := traeUserID(cred); uid != "" {
		return checkinHeadersForUID(cred, uid)
	}
	return checkinHeadersForUID(cred, strings.TrimSpace(cred.DeviceID))
}

// checkinHeadersForUID 用给定身份装配官方客户端请求头。
func checkinHeadersForUID(cred *Credential, uid string) map[string]string {
	return map[string]string{
		"Content-Type":       "application/json",
		"Accept":             "*/*",
		"Accept-Language":    "zh-CN",
		"User-Agent":         traeCheckinUserAgent,
		"Authorization":      "Cloud-IDE-JWT " + cred.AccessToken,
		"X-Market-Client-Id": traeCheckinMarketClientID,
		"X-Market-User-Id":   deriveMarketUserID(uid),
		"X-User-Region":      "CN",
		"X-Device-Id":        deriveCheckinDeviceID(uid),
		"X-Lgw-Req-Sdk-Type": "3",
		"Package-Type":       "stable_cn",
		"X-Lscbd-Aid":        "787976",
		"X-Lscbd-Platform":   "windows",
		// 参考实现调用时产品信息为空，实际发送的就是字面量 undefined；
		// 客户端身份由 User-Agent 表达，App-Version 不参与校验。
		"App-Version":      "undefined",
		"X-Tt-Trace-Id":    "00-" + core.RandomHex(8) + "-01",
		"Vscode-Sessionid": deriveCheckinSessionID(uid),
		"X-Request-Id":     newUUID(),
		"Sec-Fetch-Dest":   "empty",
		"Sec-Fetch-Mode":   "no-cors",
		"Sec-Fetch-Site":   "none",
	}
}

const (
	// traeCheckinUserAgent / traeCheckinMarketClientID 是官方 TRAE SOLO CN
	// 客户端在签到请求里使用的固定 UA 与市场客户端标识。
	traeCheckinUserAgent      = "VSCode 1.107.1 (TRAE SOLO CN)"
	traeCheckinMarketClientID = "VSCode 1.107.1"
)

// traeUserID 返回签到设备身份所用的账号 id。
//
// 优先 JWT payload.data.id（token 刷新后稳定），退回凭证 uid。为空时调用方
// 必须拒绝发请求：共用默认设备号会让多账号互相抢「每设备每天一次」的配额。
func traeUserID(cred *Credential) string {
	if identity := jwtIdentity(cred.AccessToken); identity != "" {
		return identity
	}
	return strings.TrimSpace(cred.UID)
}

// CheckinDeviceID 派生签到用的 15 位设备号（基于账号 uid 的确定性 SHA-256 流）。
func CheckinDeviceID(cred *Credential) string {
	uid := traeUserID(cred)
	if uid == "" {
		return ""
	}
	return deriveCheckinDeviceID(uid)
}

func deriveCheckinDeviceID(uid string) string {
	stream := seededStream(uid, "devid", 15)
	out := make([]byte, len(stream))
	for index, value := range stream {
		out[index] = '0' + value%10
	}
	return string(out)
}

// deriveMarketUserID 派生 UUID v4 形态的 X-Market-User-Id。
func deriveMarketUserID(uid string) string {
	bs := seededStream(uid, "market", 16)
	bs[6] = (bs[6] & 0x0F) | 0x40
	bs[8] = (bs[8] & 0x3F) | 0x80
	hex := fmt.Sprintf("%x", bs)
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

// deriveCheckinSessionID 派生 64 位 hex 的 Vscode-Sessionid。
func deriveCheckinSessionID(uid string) string {
	return fmt.Sprintf("%x", seededStream(uid, "sess", 32))
}

// seededStream 是确定性伪随机流：SHA256(salt:seed || counterBE32) 串联。
//
// 与参考实现（trae-mate device_map.rs）逐字节一致，保证同一账号跨进程、
// 跨版本得到同一套设备标识。
func seededStream(seed, salt string, nbytes int) []byte {
	prefix := salt + ":" + seed
	out := make([]byte, 0, nbytes)
	for counter := uint32(0); len(out) < nbytes; counter++ {
		hasher := sha256.New()
		_, _ = hasher.Write([]byte(prefix))
		var counterBytes [4]byte
		binary.BigEndian.PutUint32(counterBytes[:], counter)
		_, _ = hasher.Write(counterBytes[:])
		out = append(out, hasher.Sum(nil)...)
	}
	return out[:nbytes]
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

	payload, errBody := json.Marshal(map[string]any{"require_usage": true, "req_source": 2})
	if errBody != nil {
		return nil, errBody
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.QuotaHost+EpEntUsage, bytes.NewReader(payload))
	if errReq != nil {
		return nil, errReq
	}
	for key, value := range quotaHeaders(cred) {
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
			// 负数额度表示「无限」，与「没有额度」是两种语义，不能当 0。
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
