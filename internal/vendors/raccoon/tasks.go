package raccoon

// 本文件实现小浣熊的凭证续期、额度查询、每日签到与账号资料。
//
// 端点口径（Agent2API 抓包实测）：
//   - 续期：POST {auth_base}/refresh body {"refresh_token":…}，响应可能
//     轮换 refresh_token（空 = 未轮换，沿用旧值）；
//   - 积分：GET {points_base}/balance（available_points 及各来源分类）；
//   - 签到：每日发放由服务端 00:00 自动入账；客户端侧的登录领取走
//     POST {origin}/api/web/desktop/v1/login/points/grant（幂等，已领回
//     granted:false）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// apiHTTPTimeout 是 JSON API 请求的上限。
const apiHTTPTimeout = 30 * time.Second

// ErrCheckinNotGranted 表示登录领取时上游返回 granted:false（该账号已领过）。
var ErrCheckinNotGranted = errors.New("该账号已领取过桌面端登录奖励（每号一次）")

// ClaimDailyGrant 触发登录奖励领取（POST {origin}/api/web/desktop/v1/login/points/grant）。
//
// 上游幂等：已领过时返回 granted:false，这里转成 ErrCheckinNotGranted。
// 该奖励是**一次性**的（不是每日签到）：每日积分由服务端 00:00 自动发放，
// 没有客户端可调用的签到端点。
func ClaimDailyGrant(ctx context.Context, origin string, cred *Credential) (granted bool, credit int64, err error) {
	if strings.TrimSpace(origin) == "" {
		return false, 0, fmt.Errorf("upstream origin is not configured")
	}
	endpoint := strings.TrimRight(origin, "/") + DesktopBasePath + PointsGrantPath
	// 请求体必须为空：参考实现（官方桌面端）发 POST 无 body。
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if errReq != nil {
		return false, 0, fmt.Errorf("build grant request: %w", errReq)
	}
	ApplyGrantHeaders(req, cred.AccessTokenValue(), cred.OfficeIdentityValue(), cred.DeviceIDValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return false, 0, &core.Failure{Msg: "grant: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return false, 0, &core.Failure{Status: resp.StatusCode, Msg: "read grant response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, 0, Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		Granted bool `json:"granted"`
		Popup   *struct {
			Points int64 `json:"points"`
		} `json:"popup"`
	}](resp.StatusCode, raw, "grant")
	if errDecode != nil {
		return false, 0, errDecode
	}
	if !data.Granted {
		return false, 0, ErrCheckinNotGranted
	}
	if data.Popup != nil {
		credit = data.Popup.Points
	}
	return true, credit, nil
}
func decodeAPIData[T any](status int, raw []byte, what string) (T, error) {
	var zero T
	var envelope struct {
		Code    *int64          `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Details string          `json:"details"`
		Data    json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return zero, Classify(status, string(raw))
	}
	errMsg := core.FirstNonEmpty(envelope.Message, envelope.Msg, envelope.Details)
	if envelope.Code != nil && *envelope.Code != 0 {
		return zero, ClassifyBusiness(status, *envelope.Code, errMsg)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return zero, nil
	}
	if errUnmarshal := json.Unmarshal(envelope.Data, &zero); errUnmarshal != nil {
		return zero, fmt.Errorf("parse %s data: %w", what, errUnmarshal)
	}
	return zero, nil
}

// RefreshToken 用 refresh token 换新票（POST {auth_base}/refresh）。
//
// 返回新的 access / refresh token；refresh 为空表示上游未轮换。
func RefreshToken(ctx context.Context, authBase string, cred *Credential) (access, refresh string, expiresAt int64, uid string, err error) {
	if strings.TrimSpace(authBase) == "" {
		return "", "", 0, "", fmt.Errorf("upstream auth base URL is not configured")
	}
	refreshToken := cred.RefreshTokenValue()
	if strings.TrimSpace(refreshToken) == "" {
		return "", "", 0, "", &core.Failure{Msg: "该账号没有 refresh_token，无法续期，请重新登录", Kind: core.FailureKindClient}
	}
	body, errMarshal := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if errMarshal != nil {
		return "", "", 0, "", fmt.Errorf("marshal refresh request: %w", errMarshal)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authBase+RefreshPath, strings.NewReader(string(body)))
	if errReq != nil {
		return "", "", 0, "", fmt.Errorf("build refresh request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return "", "", 0, "", &core.Failure{Msg: "refresh: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "read refresh response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", "", 0, "", &core.Failure{Status: http.StatusUnauthorized, Msg: "小浣熊刷新令牌已失效，请重新登录该账号", Kind: core.FailureKindSessionDead}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, "", Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		UserID       string `json:"user_id"`
		UID          string `json:"uid"`
	}](resp.StatusCode, raw, "refresh")
	if errDecode != nil {
		return "", "", 0, "", errDecode
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return "", "", 0, "", &core.Failure{Status: resp.StatusCode, Msg: "小浣熊续期响应缺少令牌，旧凭证未被覆盖", Kind: core.FailureKindServer}
	}
	access = strings.TrimSpace(data.AccessToken)
	refresh = strings.TrimSpace(data.RefreshToken)
	if ms := core.GetUnixSeconds(map[string]any{"expires_at": data.ExpiresAt}, "expires_at"); ms > 0 {
		expiresAt = ms
	}
	uid = core.FirstNonEmpty(data.UserID, data.UID)
	return access, refresh, expiresAt, uid, nil
}

// FetchBalance 查询积分余额（GET {points_base}/balance）。
func FetchBalance(ctx context.Context, pointsBase string, cred *Credential) (available int64, err error) {
	if strings.TrimSpace(pointsBase) == "" {
		return 0, fmt.Errorf("upstream points base URL is not configured")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, pointsBase+BalancePath, nil)
	if errReq != nil {
		return 0, fmt.Errorf("build balance request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return 0, &core.Failure{Msg: "balance: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return 0, &core.Failure{Status: resp.StatusCode, Msg: "read balance response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		AvailablePoints any `json:"available_points"`
	}](resp.StatusCode, raw, "balance")
	if errDecode != nil {
		return 0, errDecode
	}
	return int64(core.IntFromAny(data.AvailablePoints)), nil
}

// UserProfile 是账号资料接口返回的展示与组织信息。
type UserProfile struct {
	UID            string
	Nickname       string
	OfficeIdentity string
}

// FetchUserInfo 查询账号资料（GET {auth_base}/user_info），补 uid / 昵称。
func FetchUserInfo(ctx context.Context, authBase string, cred *Credential) (uid, nickname string, err error) {
	profile, errProfile := FetchUserProfile(ctx, authBase, cred)
	if errProfile != nil {
		return "", "", errProfile
	}
	return profile.UID, profile.Nickname, nil
}

// FetchUserProfile 查询账号资料，同时带回组织码（X-Org-Code 的来源）。
func FetchUserProfile(ctx context.Context, authBase string, cred *Credential) (*UserProfile, error) {
	if strings.TrimSpace(authBase) == "" {
		return nil, fmt.Errorf("upstream auth base URL is not configured")
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, authBase+UserInfoPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build user info request: %w", errReq)
	}
	ApplyAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, apiHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "user info: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read user info response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(raw))
	}
	data, errDecode := decodeAPIData[struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		OfficeIdentity string `json:"office_identity"`
	}](resp.StatusCode, raw, "user info")
	if errDecode != nil {
		return nil, errDecode
	}
	return &UserProfile{
		UID:            strings.TrimSpace(data.ID),
		Nickname:       strings.TrimSpace(data.Name),
		OfficeIdentity: strings.TrimSpace(data.OfficeIdentity),
	}, nil
}
