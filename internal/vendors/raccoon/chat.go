package raccoon

// 本文件定义小浣熊的上游端点与请求头，并实现 chat-completions 转发。
//
// 协议依据：aimod-cc/Agent2API（v1.1.0 桌面客户端抓包与实测），Issue #24 指定为实现参考。
//
// 上游就是 OpenAI 形状：请求体原样转发（model 由适配器保证是目录内规范
// id），返回 OpenAI JSON / SSE，插件原样透传。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 上游基址（可被配置覆盖）。
const (
	DefaultLLMBase    = "https://xiaohuanxiong.com/api/web/llm/v2"
	DefaultAuthBase   = "https://xiaohuanxiong.com/api/web/auth/v1"
	DefaultPointsBase = "https://xiaohuanxiong.com/api/web/points/v1"
	DefaultOrigin     = "https://xiaohuanxiong.com"
)

// 业务路径（分别挂在对应域下）。
const (
	ChatPath        = "/chat/completions"
	CatalogPath     = "/model_catalog"
	RefreshPath     = "/refresh"
	LoginCodePath   = "/login_with_authorization_code"
	BalancePath     = "/balance"
	UserInfoPath    = "/user_info"
	EntitlementPath = "/entitlement_info"
	PointsGrantPath = "/login/points/grant"
	PointsBillsPath = "/bills"
	DesktopBasePath = "/api/web/desktop/v1"
	AuthorizePath   = "/code/authorize"
	// DefaultConfigName 是上游默认对话模型（v1.1.0 抓包：目录 default_model）。
	DefaultConfigName = "raccoon-8c4485"
	// DefaultClientVersion 是出站 x-client-version 头的内置值（抓包实测）。
	DefaultClientVersion = "v1.1.0"
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// BaseURL 归一化基址：override 优先（末尾无斜杠），否则默认值。
func BaseURL(override, fallback string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed != "" {
		return trimmed
	}
	return fallback
}

// ApplyClientHeaders 写入小浣熊桌面客户端识别头（抓包实测：上游按客户端
// 渠道做风控，带上最贴近真实流量）。
func ApplyClientHeaders(req *http.Request) {
	req.Header.Set("x-client-name", "@raccoon/office-desktop")
	req.Header.Set("x-client-platform", "desktop-windows-x64")
	req.Header.Set("x-client-version", DefaultClientVersion)
	req.Header.Set("x-client-channel", "official")
}

// grantClientPlatform 是登录奖励接口要求的平台枚举值。
//
// 参考实现依据桌面主进程 desktopDeviceIdentity.js：win32 → desktop-windows。
// 该端点**要求**这个头，猜成 desktop-windows-x64 会被拒。
const grantClientPlatform = "desktop-windows"

// grantClientVersion 是登录奖励接口使用的客户端版本（带 v 前缀）。
const grantClientVersion = "v1.0.35"

// ApplyGrantHeaders 写入登录奖励接口的客户端识别头。
//
// 与 ApplyClientHeaders 刻意分开：对话域沿用既有抓包值，登录奖励端点按
// 官方桌面客户端口径发送固定枚举与组织码。个人账号的 X-Org-Code 为空串，
// 但该头必须存在（上游总是发送）。
func ApplyGrantHeaders(req *http.Request, token, orgCode, deviceID string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org-Code", strings.TrimSpace(orgCode))
	req.Header.Set("X-Raccoon-Language", "zh")
	req.Header.Set("X-Client-Platform", grantClientPlatform)
	req.Header.Set("X-Client-Version", grantClientVersion)
	if trimmed := strings.TrimSpace(deviceID); trimmed != "" {
		req.Header.Set("X-Client-Device-ID", trimmed)
	}
	if trimmed := strings.TrimSpace(token); trimmed != "" {
		req.Header.Set("Authorization", "Bearer "+trimmed)
	}
}

// ApplyChatHeaders 写入 chat 请求所需的全部头。
func ApplyChatHeaders(req *http.Request, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	ApplyClientHeaders(req)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

// ApplyAPIHeaders 写入鉴权 / 积分域 JSON 请求的头（Accept + 客户端识别 + Bearer）。
func ApplyAPIHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	ApplyClientHeaders(req)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
}

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// Token 是出站凭证（JWT access token）。
	Token string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, llmBase string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, llmBase, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	// 业务信封（{"code","message"}）：code != 0 按业务错误处理。
	if code, message, ok := envelopeError(body); ok {
		return nil, ClassifyBusiness(resp.StatusCode, code, message)
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, llmBase string, req ChatRequest) (*http.Response, error) {
	endpoint := llmBase + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(payload))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, llmBase string, req ChatRequest) (*http.Response, error) {
	endpoint := llmBase + ChatPath
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// envelopeError 检查业务信封 {"code","message"}：code 存在且非 0 时返回 (code, message, true)。
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	message := probe.Message
	if message == "" {
		message = probe.Msg
	}
	return *probe.Code, message, true
}
