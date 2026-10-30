package trae

// 本文件实现 Trae 家族（Trae 国内版 / Trae 国际版）的对话转发：
// 请求体改写、IDE 请求头装配与 SSE 事件转换。
//
// 协议基线：TraeCode CN 桌面端 llm_raw_chat（与 Trae2api-cn 参考实现对齐），
// 两个实例共享同一套协议实现，差异由 Config/profile 描述：
//   - 国内版（traecn）：raw_chat_function=chat_v3，IDE 版本 3.3.104
//   - 国际版（traeglobal）：与国内版同协议，仅域名与 client_id 不同；
//     国际版无签到/额度活动（上游 404），由 tasks.go 区分处理。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// ---- 端点与实例 profile ----

type Region string

const (
	RegionCN     Region = "cn"
	RegionGlobal Region = "global"

	VendorIDCN       = "traecn"
	VendorNameCN     = "Trae 国内版"
	VendorIDGlobal   = "traeglobal"
	VendorNameGlobal = "Trae 国际版"

	HostChatCN    = "https://trae-api-cn.mchost.guru"
	HostAuthCN    = "https://api.trae.cn"
	HostConsoleCN = "https://www.trae.cn"
	ClientIDCN    = "en1oxy7wnw8j9n"
	// 国内版 IDE 版本号。上游按构建号（X-Ide-Version-Code）下发不同模型目录：
	// 20260806 起才有 deepseek-v4.1-flash，更早的构建号会被降级成旧清单。
	// 这两个值可用 <实例ID>_ide_version / <实例ID>_ide_version_code 覆盖（见 adapter.Configure），
	// 例如 traecn_ide_version_code；两条产品线版本体系不同，必须按实例配置。
	IDEVersionCN = "3.3.104"
	// IDEVersionCodeCN 是国内版 IDE 的构建号，与 X-Ide-Version-Code 一起下发。
	IDEVersionCodeCN = "20260922"

	// 国际版（新加坡）
	HostChatGlobal       = "https://coresg-normal.trae.ai"
	HostAuthGlobal       = "https://growsg-normal.trae.ai"
	HostConsoleGlobal    = "https://www.trae.ai"
	ClientIDGlobal       = "ono9krqynydwx5"
	IDEVersionGlobal     = "3.3.67"
	IDEVersionCodeGlobal = "20260401"

	// DeviceBrandCN 是国内版上报的设备型号。
	DeviceBrandCN = "83DG"

	// 公共端点路径（两个实例共用）
	EpChat          = "/api/ide/v2/llm_raw_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
	// EpEntUsageGlobal 是国际版额度端点：路径版本与国内版不同（v1），
	// 打在鉴权域（growsg-normal）而不是对话域。
	EpEntUsageGlobal = "/trae/api/v1/pay/ide_user_ent_usage"

	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
)

// Config 是一个 Trae 实例的完整 profile。
type Config struct {
	VendorID   string
	VendorName string
	Region     Region

	ChatHost    string
	AuthHost    string
	RefreshHost string
	CheckinHost string
	QuotaHost   string
	// QuotaPath 是额度端点路径：两个区域版本不同（国内 v2 / 国际 v1）。
	QuotaPath   string
	ConsoleHost string

	ClientID       string
	IDEVersion     string
	IDEVersionCode string
	Function       string
	DefaultModel   string
	AppID          string
	// DeviceBrand 只用于模型目录请求（get_detail_param 要求带设备型号）。
	DeviceBrand string
}

// ConfigFor 返回国内/国际版默认配置（保留给协议层测试使用）。
func ConfigFor(r Region) Config {
	if r == RegionGlobal {
		return Config{
			VendorID:       VendorIDGlobal,
			VendorName:     VendorNameGlobal,
			Region:         RegionGlobal,
			ChatHost:       HostChatGlobal,
			AuthHost:       HostAuthGlobal,
			RefreshHost:    HostAuthGlobal,
			CheckinHost:    "", // 国际版无签到活动（上游无此端点）
			QuotaHost:      HostAuthGlobal,
			QuotaPath:      EpEntUsageGlobal,
			ConsoleHost:    HostConsoleGlobal,
			ClientID:       ClientIDGlobal,
			IDEVersion:     IDEVersionGlobal,
			IDEVersionCode: IDEVersionCodeGlobal,
			Function:       "chat_v3",
			DefaultModel:   "glm-5.2",
			AppID:          AppID,
			DeviceBrand:    "PC",
		}
	}
	return Config{
		VendorID:       VendorIDCN,
		VendorName:     VendorNameCN,
		Region:         RegionCN,
		ChatHost:       HostChatCN,
		AuthHost:       HostAuthCN,
		RefreshHost:    HostAuthCN,
		CheckinHost:    HostAuthCN,
		QuotaHost:      HostAuthCN,
		QuotaPath:      EpEntUsage,
		ConsoleHost:    HostConsoleCN,
		ClientID:       ClientIDCN,
		IDEVersion:     IDEVersionCN,
		IDEVersionCode: IDEVersionCodeCN,
		Function:       "chat_v3",
		DefaultModel:   "glm-5.2",
		AppID:          AppID,
		DeviceBrand:    DeviceBrandCN,
	}
}

// ConfigForVendor 返回某个供应商实例的 profile。
func ConfigForVendor(vendorID string) (Config, bool) {
	switch vendorID {
	case VendorIDCN:
		return ConfigFor(RegionCN), true
	case VendorIDGlobal:
		return ConfigFor(RegionGlobal), true
	default:
		return Config{}, false
	}
}

// AllConfigs 返回两个实例的 profile，顺序与历史注册顺序一致。
func AllConfigs() []Config {
	return []Config{ConfigFor(RegionCN), ConfigFor(RegionGlobal)}
}

func (c Config) clientUA() string { return "Trae/" + c.IDEVersion }

func (c Config) supportsCheckin() bool { return c.Region == RegionCN }

func configForCredential(cred *Credential) Config {
	if cred == nil {
		return ConfigFor(RegionCN)
	}
	if cfg, okConfig := ConfigForVendor(cred.Vendor); okConfig {
		return cfg
	}
	return ConfigFor(cred.Region)
}

// ---- 对话转发 ----

type ChatRequest struct {
	Credential *Credential
	Profile    Config
	Body       []byte
	Stream     bool
	// catalog 是实例级模型目录缓存；nil 表示不缓存（测试与一次性调用）。
	catalog *catalogCache
}

type StreamChunkHandler func(chunk []byte) error

// ApplyTraeHeaders 装配 TraeCode IDE 请求头。
//
// 头集合与 Trae2api-cn 的 build_traecode_headers 一致：模型/函数绑定除请求体
// 外还要在 X-App-Function / X-Ide-Function 上双写，缺一个都可能被网关拒绝。
func ApplyTraeHeaders(req *http.Request, cred *Credential, cfg Config, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "keep-alive")

	at := cred.AccessToken
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if cred.UID != "" {
		req.Header.Set("X-Uid", cred.UID)
	}
	req.Header.Set("X-App-Id", cfg.AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-App-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version", cfg.IDEVersion)
	req.Header.Set("X-Ide-Version-Code", cfg.IDEVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-App-Function", cfg.Function)
	req.Header.Set("X-Ide-Function", cfg.Function)
	req.Header.Set("X-Request-Id", newUUID())
	req.Header.Set("X-Custom-Trace-Id", core.RandomHex(16))
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("Request-Traffic-Type", "prod")
	req.Header.Set("User-Agent", cfg.clientUA())
	if cred.MachineID != "" {
		req.Header.Set("X-Machine-Id", cred.MachineID)
	}
	if cred.DeviceID != "" {
		req.Header.Set("X-Device-Id", cred.DeviceID)
	}
}

// requestModelName 从 OpenAI 请求体里取出模型名（缺省用 profile 默认值）。
func requestModelName(src []byte, cfg Config) string {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return cfg.DefaultModel
	}
	if model, ok := obj["model"].(string); ok && strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	return cfg.DefaultModel
}

// PrepareBody 把 OpenAI 风格请求体改写为 TraeCode llm_raw_chat 请求体。
//
// 关键差异（相对旧版 llm_utils_chat 路径）：
//   - model_name 必须是上游 wire id（目录 model_detail_list 里的精确值），
//     config_name 是目录里的配置名，两者都由 catalogCache.resolve 解析；
//   - raw_chat_function 同时进请求体与请求头；
//   - conversation_id / session_id 每次请求新生成。
func PrepareBody(src []byte, cfg Config, resolved ResolvedModel) ([]byte, string, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, resolved.ConfigName, err
	}

	sessionID := newUUID()
	obj["config_name"] = resolved.ConfigName
	obj["model_name"] = resolved.WireName
	obj["conversation_id"] = sessionID
	obj["session_id"] = sessionID
	obj["stream"] = true
	obj["raw_chat_function"] = cfg.Function
	obj["config_source"] = 1
	delete(obj, "model")
	delete(obj, "function")

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			// TraeCode 的 ChatMessageData 用 content 块数组；字符串内容包一层。
			content, present := m["content"]
			if present {
				if s, ok := content.(string); ok {
					m["content"] = []any{map[string]any{"type": "text", "text": s}}
				}
			}
		}
	}

	data, err := json.Marshal(obj)
	return data, resolved.ConfigName, err
}

// Chat 执行一次非流式对话（内部仍走上游 SSE，聚合后返回 OpenAI 结构）。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	respBody, model, errDo := doTraeRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = respBody.Close() }()

	var (
		contentSB   strings.Builder
		reasoningSB strings.Builder
		usage       map[string]any
		finish      = "stop"
	)

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		switch currentEvent {
		case "output":
			if r, ok := payload["response"].(string); ok {
				contentSB.WriteString(r)
			}
			if rc, ok := payload["reasoning_content"].(string); ok {
				reasoningSB.WriteString(rc)
			}
		case "token_usage":
			usage = mapUsage(payload)
		case "error":
			code, _ := payload["code"].(float64)
			msg, _ := payload["message"].(string)
			return nil, fmt.Errorf("trae upstream error: code=%.0f msg=%s", code, msg)
		case "done":
			if fr, ok := payload["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
		}
	}

	message := map[string]any{
		"role":              "assistant",
		"content":           contentSB.String(),
		"reasoning_content": reasoningSB.String(),
	}
	out := map[string]any{
		"id":      "chatcmpl-" + core.RandomHex(6),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if usage != nil {
		out["usage"] = usage
	}

	return json.Marshal(out)
}

// ChatStream 执行一次流式对话，把上游 SSE 增量转换成 OpenAI chunk。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk StreamChunkHandler) error {
	respBody, model, errDo := doTraeRequest(ctx, baseURL, req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = respBody.Close() }()

	scanner := bufio.NewScanner(respBody)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var currentEvent string
	chunkID := "chatcmpl-" + core.RandomHex(6)
	created := time.Now().Unix()

	emit := func(delta map[string]any, finishReason any) error {
		chunk := map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		rawChunk, errMarshal := json.Marshal(chunk)
		if errMarshal != nil {
			return errMarshal
		}
		return onChunk(rawChunk)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}

		switch currentEvent {
		case "output":
			delta := map[string]any{}
			resp, _ := payload["response"].(string)
			reasoning, _ := payload["reasoning_content"].(string)
			if resp != "" {
				delta["content"] = resp
			}
			if reasoning != "" {
				delta["reasoning_content"] = reasoning
			}
			if len(delta) > 0 {
				if errEmit := emit(delta, nil); errEmit != nil {
					return errEmit
				}
			}
		case "token_usage":
			// usage 随最后一个 chunk 之后单独投递（OpenAI 流式惯例）。
			chunk := map[string]any{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{},
				"usage":   mapUsage(payload),
			}
			rawChunk, errMarshal := json.Marshal(chunk)
			if errMarshal != nil {
				return errMarshal
			}
			if errEmit := onChunk(rawChunk); errEmit != nil {
				return errEmit
			}
		case "error":
			code, _ := payload["code"].(float64)
			msg, _ := payload["message"].(string)
			return fmt.Errorf("trae stream error: code=%.0f msg=%s", code, msg)
		case "done":
			return emit(map[string]any{}, "stop")
		}
	}

	return scanner.Err()
}

// mapUsage 把上游 token_usage 事件映射成 OpenAI usage 结构。
func mapUsage(payload map[string]any) map[string]any {
	usage := map[string]any{}
	for src, dst := range map[string]string{
		"prompt_tokens":     "prompt_tokens",
		"completion_tokens": "completion_tokens",
		"total_tokens":      "total_tokens",
	} {
		if v, ok := payload[src]; ok {
			usage[dst] = v
		}
	}
	if v, ok := payload["reasoning_tokens"]; ok {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": v}
	}
	return usage
}

// doTraeRequest 发起 llm_raw_chat 请求；v2 端点不可用时回退 v1。
func doTraeRequest(ctx context.Context, baseURL string, req ChatRequest) (io.ReadCloser, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("trae credential is nil")
	}

	cfg := req.Profile
	if cfg.VendorID == "" {
		cfg = configForCredential(req.Credential)
	}

	resolved, errResolve := req.catalog.resolve(ctx, requestModelName(req.Body, cfg), cfg, req.Credential)
	if errResolve != nil {
		return nil, "", errResolve
	}

	body, model, errPrep := PrepareBody(req.Body, cfg, resolved)
	if errPrep != nil {
		return nil, "", errPrep
	}

	targetHost := strings.TrimRight(baseURL, "/")
	if targetHost == "" {
		targetHost = cfg.ChatHost
	}

	resp, errDo := doRawChat(ctx, targetHost+EpChat, body, req.Credential, cfg)
	if errDo != nil {
		return nil, "", fmt.Errorf("trae chat request failed: %w", errDo)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, "", Classify(resp.StatusCode, string(raw))
	}
	return resp.Body, model, nil
}

// doRawChat 发起 llm_raw_chat 请求。
//
// 上游该端点只讲 SSE（请求体 stream 恒为 true），因此统一走宿主流式桥逐事件
// 投递；非流式 Chat 由调用方聚合同一份 SSE。
func doRawChat(ctx context.Context, url string, body []byte, cred *Credential, cfg Config) (*http.Response, error) {
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errReq != nil {
		return nil, errReq
	}
	ApplyTraeHeaders(httpReq, cred, cfg, true)

	return hostbridge.StreamClient(ctx, 0).Do(httpReq)
}

// newUUID 生成随机 UUID v4 字符串（conversation_id / session_id 用）。
func newUUID() string {
	b := make([]byte, 16)
	if _, errHex := hex.Decode(b, []byte(core.RandomHex(16))); errHex != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
