package autoclaw

// 本文件实现 AutoClaw 的账号运营能力（当前为每日签到；POST {base}/autoclaw-proxy/proxy/autoclaw-task-complete）。
//
// 协议依据 Agent2API check.py：body {"task_id":"daily_signin"}，业务信封
// code=0 成功；data.already_completed=true 表示今日已签（幂等），
// data.reward_points 是本次奖励积分。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// checkinTaskID 是每日签到任务的任务 ID。
const checkinTaskID = "daily_signin"

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	Already bool
	// Claimed 报告本次是否真的领到了积分。上游可能以 code=0 返回
	// success:false（窗口未开 / 无资格），此时不能算签到成功。
	Claimed bool
	Points  int64
	TaskID  string
	// Msg 是上游给出的可读说明（失败时用于展示）。
	Msg string
}

// Checkin 执行一次每日签到。
func Checkin(ctx context.Context, baseURL string, cred *Credential) (*CheckinResult, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is not configured")
	}
	body, errMarshal := json.Marshal(map[string]any{"task_id": checkinTaskID})
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal checkin request: %w", errMarshal)
	}
	endpoint := baseURL + CheckinPath
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if errReq != nil {
		return nil, fmt.Errorf("build checkin request: %w", errReq)
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())

	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		return nil, &core.Failure{Msg: "checkin: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read checkin response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	var envelope struct {
		Code *int64 `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Success          bool   `json:"success"`
			AlreadyCompleted bool   `json:"already_completed"`
			RewardPoints     any    `json:"reward_points"`
			TaskID           string `json:"task_id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	points := int64(core.IntFromAny(envelope.Data.RewardPoints))

	// 业务码优先：登录失效按 401 上抛，其余非零码是「本次没领成」而不是
	// 网络故障——当成 transient 会无限重试。
	if envelope.Code != nil && *envelope.Code != 0 {
		if DeadCode(*envelope.Code) {
			return nil, Classify(http.StatusUnauthorized, *envelope.Code, envelope.Msg)
		}
		if resp.StatusCode >= 400 {
			return nil, Classify(resp.StatusCode, *envelope.Code, envelope.Msg)
		}
		return &CheckinResult{
			Already: envelope.Data.AlreadyCompleted,
			Msg:     core.FirstNonEmpty(envelope.Msg, "签到未完成"),
		}, nil
	}

	// code=0 / 缺省：以 data.success + reward_points 判定是否真的领到。
	// 参考实现：claimed = reward_points > 0 || (success && !already_completed)。
	claimed := points > 0 || (envelope.Data.Success && !envelope.Data.AlreadyCompleted)
	if resp.StatusCode >= 400 && !claimed && !envelope.Data.AlreadyCompleted {
		return nil, Classify(resp.StatusCode, 0, string(raw))
	}
	return &CheckinResult{
		Already: envelope.Data.AlreadyCompleted,
		Claimed: claimed,
		Points:  points,
		TaskID:  checkinTaskID,
		Msg:     envelope.Msg,
	}, nil
}

// UserInfoPath 是登录活跃上报端点（签到成功后由服务端发放活动奖励）。
const UserInfoPath = "/userapi/v1/user-info"

// TriggerLoginActive 在签到成功后上报一次登录活跃。
//
// 参考实现（Agent2API checkin.rs）在签到后 POST user-info，服务端据此发放
// 每日活跃 / 一次性登录奖励。失败只记日志，绝不改变签到结果。
func TriggerLoginActive(ctx context.Context, baseURL string, cred *Credential) {
	deviceID := cred.DeviceIDValue()
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(deviceID) == "" {
		return
	}
	body, errMarshal := json.Marshal(map[string]any{"source_id": "autoclaw", "device_id": deviceID})
	if errMarshal != nil {
		logger.Debug("autoclaw login-active encode failed: %v", errMarshal)
		return
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+UserInfoPath, strings.NewReader(string(body)))
	if errReq != nil {
		logger.Debug("autoclaw login-active request build failed: %v", errReq)
		return
	}
	ApplyUserAPIHeaders(req, cred.AccessTokenValue())
	resp, errDo := hostbridge.Client(ctx, 20*time.Second).Do(req)
	if errDo != nil {
		logger.Debug("autoclaw login-active request failed: %v", errDo)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// 排空 body，让宿主连接可复用；上报失败只记日志，不影响签到结果。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		logger.Debug("autoclaw login-active rejected: HTTP %d", resp.StatusCode)
	}
}
