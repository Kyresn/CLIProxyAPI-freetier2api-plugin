package autoclaw

// 本文件实现 AutoClaw 的协议端点、请求签名/请求头与 chat-completions 转发。
//
// 协议依据：Agent2API（docs/reverse-notes.md，2026-09 对 Web 前端 bundle 与
// 桌面端 1.18.5 的静态逆向结论），Issue #24 指定为本供应商的实现参考。
//
// 通道：region 基址下的 /autoclaw-proxy/proxy/autoclaw/v1/chat/completions
// （Agent2API 的 chat_proxy 通道，模型经 X-Request-Model 头声明）。
// 上游返回 OpenAI 兼容的 JSON / SSE，插件原样透传。
//
// 说明：Agent2API 还有「沙箱 relay」通道（wss 设备上线 + agent/send +
// /api/events），依赖 per-account 云端沙箱与 WebSocket 长连接。本插件按
// 「凭证驻留宿主进程、按请求出站」的形态选择 proxy 通道；relay 通道需要
// 的常驻会话状态与插件模型不符，暂不实现（凭证里保留 sandbox 字段以备扩展）。

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 区域默认基地址（userapi REST 基址，cn/global 双部署）。
const (
	DefaultBaseURLCN     = "https://autoglm-acceleration-api.zhipuai.cn"
	DefaultBaseURLGlobal = "https://autoglm-api.autoglm.ai"
)

// 业务路径（挂在 region 基址下）。
const (
	ChatPath         = "/autoclaw-proxy/proxy/autoclaw/v1/chat/completions"
	ModelsConfigPath = "/autoclaw-proxy/proxy/autoclaw-model-config"
	RefreshPath      = "/userapi/v1/refresh"
	WalletsPath      = "/agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw"
	CheckinPath      = "/autoclaw-proxy/proxy/autoclaw-task-complete"
)

// 签名与客户端常量（逆向自 Web 前端 bundle）。
const (
	authAppID     = "100003"
	authSecret    = "38d2391985e2369a5fb8227d8e6cd5e5"
	clientVer     = "1.18.5"
	errorCodeDead = 410000 // 未登录/会话失效
	// errorCodeAuthExpired 是参考实现列出的另一个登录过期码。
	errorCodeAuthExpired = 400000
)

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// DefaultBaseURLFor 返回区域的默认基地址。
func DefaultBaseURLFor(region Region) string {
	if region.IsGlobal() {
		return DefaultBaseURLGlobal
	}
	return DefaultBaseURLCN
}

// BaseURL 返回上游基地址（末尾无斜杠）：override 优先，否则区域默认值。
func BaseURL(override string, region Region) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed != "" {
		return trimmed
	}
	return DefaultBaseURLFor(region)
}

// AuthSign 计算请求签名：MD5("<appid>&<ts>&<secret>")。
func AuthSign(ts string) string {
	sum := md5.Sum([]byte(authAppID + "&" + ts + "&" + authSecret))
	return hex.EncodeToString(sum[:])
}

// bearer 归一化 token：已带 Bearer 前缀时幂等。
func bearer(token string) string {
	trimmed := strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(trimmed), "bearer ") {
		return trimmed
	}
	return "Bearer " + trimmed
}

// ApplyUserAPIHeaders 写入 userapi REST 请求所需的全部头（业务信封接口）。
//
// 客户端指纹与官方桌面端一致：参考实现（Agent2API signed_auth_headers）
// 对 refresh / balance / 签到 / 模型配置共用这一组头，且明确提示裁剪它们
// 会让上游把请求判成非官方客户端。签名（appid/ts/MD5）保持不变。
func ApplyUserAPIHeaders(req *http.Request, token string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Tm", desktopPlatform())
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Harness-Type", "zcode")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Auth-Appid", authAppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", AuthSign(ts))
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	req.Header.Set("X-Lang", "zh-CN")
	// 不能留空：Go 默认的 "Go-http-client/1.1" 会被阿里云 WAF 以 405 HTML 拦截
	// （实测同一请求只换 UA 就恢复；OAuth 的 405 就是这么来的）。
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
	}
}

// desktopPlatform 返回官方桌面端使用的 X-Tm 平台标识。
func desktopPlatform() string {
	if runtime.GOOS == "windows" {
		return "win"
	}
	return "linux"
}

// ApplyOAuthHeaders 写入国际版 OAuth 两跳（oauth-url / oauth-login）的请求头。
//
// 与 ApplyUserAPIHeaders 的差别（别合并）：OAuth 走的是官方**桌面端**链路，
// 客户端指纹是 pc / zcode / linux / zh-CN（Agent2API signed_auth_headers 同口径）。
// User-Agent 尤其不能留空：浏览器里生成的阿里云验证码令牌由服务端原样转发，
// 而 Go 默认 UA 会被阿里云 WAF 判成机器人并回 405 HTML（现象是「Google 能过、
// Zai 一直失败」——WAF 规则按路径命中，不是端点形状问题）。
func ApplyOAuthHeaders(req *http.Request, token string) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Harness-Type", "zcode")
	// 与参考实现一致：非 Windows 构建发 linux。WAF 与业务层都不按它区分
	// 浏览器所在的桌面系统，改 mac 没有任何收益（agent2api 在 macOS 上也发 linux）。
	req.Header.Set("X-Tm", "linux")
	req.Header.Set("X-Lang", "zh-CN")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Auth-Appid", authAppID)
	req.Header.Set("X-Auth-TimeStamp", ts)
	req.Header.Set("X-Auth-Sign", AuthSign(ts))
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
	}
}

// ApplyChatHeaders 写入 proxy chat 请求所需的全部头。
//
// 与 userapi 头的差异：走桌面端形态（X-Tm: mac / X-Client-Type: pc），
// 模型经 X-Request-Model 与 X-Authorization 双通道声明。
func ApplyChatHeaders(req *http.Request, token, model string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "AutoClaw/"+clientVer)
	req.Header.Set("X-Tm", "mac")
	req.Header.Set("X-Version", clientVer)
	req.Header.Set("X-Product", "autoclaw")
	req.Header.Set("X-Channel", "official")
	req.Header.Set("X-Lang", "zh-CN")
	req.Header.Set("X-Client-Type", "pc")
	req.Header.Set("X-Trace-Id", core.RandomHex(16))
	req.Header.Set("x_trace_id", "freetier2api")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", bearer(token))
		req.Header.Set("X-Authorization", bearer(token))
	}
	if strings.TrimSpace(model) != "" {
		req.Header.Set("X-Request-Model", model)
	}
}

// ApplyModelsHeaders 写入模型配置请求的头（userapi 信封接口）。
func ApplyModelsHeaders(req *http.Request, token string) {
	ApplyUserAPIHeaders(req, token)
	req.Header.Set("Accept", "application/json")
}

// DeadCode 报告业务码是否为「会话失效」（410000 / 400000）。
func DeadCode(code int64) bool { return code == errorCodeDead || code == errorCodeAuthExpired }

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// Token 是出站凭证（access token）。
	Token string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Model 是剥掉前缀后的裸模型名。
	Model string
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doChat(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, &core.Failure{Status: resp.StatusCode, Msg: "read chat response: " + errRead.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, Classify(resp.StatusCode, 0, string(body))
	}
	// 业务信封形态（{"code","msg","data"}）：非 0 业务码按错误处理。
	if code, msg, ok := envelopeError(body); ok {
		return nil, Classify(resp.StatusCode, code, msg)
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat stream request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, 0, string(payload))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	endpoint := baseURL + ChatPath
	body := prepareBody(req.Body, req.Model)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, req.Token, req.Model)

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// prepareBody 是请求体出站前的钩子：把 model 改写为上游路由名、去掉 user 字段
// （与 Agent2API 的 _prepare_proxy_body 一致；user 字段会被上游拒绝），再补两道
// 上游预校验适配（system 白名单前缀与 max_tokens 下限，见 normalizeSystemPrompt /
// normalizeMaxTokens）。
func prepareBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if strings.TrimSpace(model) != "" {
		payload["model"] = model
	}
	delete(payload, "user")
	normalizeSystemPrompt(payload)
	normalizeMaxTokens(payload)
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// ---- 上游预校验适配（Issue #41）----
//
// 上游给 proxy chat 加了两道预校验闸门，AND 关系，命中即 406 空响应体
// （实测口径见 Agent2API 的 providers/autoclaw/prompt.rs 与 adapter.rs）：
//
//  1. system 白名单（2026-09-22 起）：首条消息必须是 system，正文以 identityLine
//     开头且带「## Tooling」段，且任何 system/developer 正文都不得含外来 harness
//     身份句（Claude Code / Codex / ZCode / DeepSeek Harness）。
//  2. max_tokens 下限（2026-09-29 起）：小正整数（30~8192）一律拒收；不传或
//     ≥ maxTokensFloor 都放行。
//
// 普通 OpenAI 客户端（system 是中性句或没有、max_tokens 常见 1024/4096）两条都
// 命中，所以必须在出站前归一，而不是让用户改客户端。

// identityLine 是上游闸门要求「以它开头」的身份句（幂等判定只认这一条）。
const identityLine = "You are a personal assistant running inside OpenClaw."

// identityPrefix 是出站身份前缀：身份句 + 「## Tooling」段，逐字取自官方客户端
// system prompt 的开头两行（末尾换行与 Agent2API 一致，正文另起一段）。
const identityPrefix = identityLine + "\n\n" +
	"## Tooling\n" +
	"Available tools are policy-filtered. Names are case-sensitive; call exactly as listed.\n"

// maxTokensFloor 是实测过的安全下限：8192（拒）与 10000（过）之间取后者。
const maxTokensFloor = 10000

// foreignIdentities 是上游按字面拦截的外来身份句 → 中性说法（长的在前：短串先
// 命中会把长串切碎）。替换串刻意不含产品名——闸门拦的就是产品名本身。
var foreignIdentities = [][2]string{
	{"You are ZCode, an interactive coding agent", "You are an interactive coding agent"},
	{"You are a coding agent running in the Codex CLI tool", "You are a coding agent running in a terminal CLI tool"},
	{"You are a coding agent running in the Codex CLI", "You are a coding agent running in a terminal CLI"},
	{"You are running as a coding agent in the Codex CLI", "You are running as a coding agent in a terminal CLI"},
	{"You are Claude Code", "You are a coding assistant"},
	{"You are ZCode", "You are an interactive coding agent"},
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent powered by a local coding harness"},
	{"You are Codex", "You are a coding agent"},
}

// normalizeSystemPrompt 让出站 system 提示符合上游白名单：先改写所有
// system/developer 正文里的外来身份句，再保证首条消息是 system 且正文以身份句
// 开头（已以身份句开头时只改写、不再前置，重复出站不会重复拼接）。首条不是
// system 时在头部插一条只带前缀的 system——不重排、不删除其余消息，也不动
// user/assistant/tool 的内容（上游闸门只看 system/developer）。
func normalizeSystemPrompt(payload map[string]any) {
	messages, okMessages := payload["messages"].([]any)
	if !okMessages {
		return
	}
	for _, item := range messages {
		message, okMessage := item.(map[string]any)
		if okMessage && isSystemRole(message) {
			rewriteIdentities(message)
		}
	}
	if len(messages) > 0 {
		if first, okFirst := messages[0].(map[string]any); okFirst && isSystemRole(first) {
			prependIdentity(first)
			return
		}
	}
	payload["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": identityPrefix,
	}}, messages...)
}

// isSystemRole 报告消息角色是否为 system/developer。
func isSystemRole(message map[string]any) bool {
	role, okRole := message["role"].(string)
	if !okRole {
		return false
	}
	trimmed := strings.TrimSpace(role)
	return strings.EqualFold(trimmed, "system") || strings.EqualFold(trimmed, "developer")
}

// rewriteIdentities 把一条 system 消息正文里的外来身份句换成中性说法。
//
// 正文有两种承载形态：字符串，或 text part 数组；其它形态无文本可改，交给
// prependIdentity 统一处置。
func rewriteIdentities(message map[string]any) {
	switch content := message["content"].(type) {
	case string:
		message["content"] = replaceForeignIdentities(content)
	case []any:
		for _, item := range content {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				part["text"] = replaceForeignIdentities(text)
			}
		}
	}
}

// prependIdentity 给一条 system 消息的正文前置身份前缀。
//
// content 的三种形态：字符串直接拼；数组拼进第一个 text part（一个都没有时在
// 头部插一个）；其它（缺失 / null / 对象）换成前缀本身——这种 content 当 system
// 提示词本来就不成立，原样发出去必然 406。
func prependIdentity(message map[string]any) {
	switch content := message["content"].(type) {
	case string:
		if !strings.HasPrefix(content, identityLine) {
			message["content"] = joinIdentity(content)
		}
	case []any:
		for _, item := range content {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			text, okText := part["text"].(string)
			if !okText {
				continue
			}
			if !strings.HasPrefix(text, identityLine) {
				part["text"] = joinIdentity(text)
			}
			return
		}
		message["content"] = append([]any{map[string]any{"type": "text", "text": identityPrefix}}, content...)
	default:
		message["content"] = identityPrefix
	}
}

// joinIdentity 返回前缀 + 一个空行 + 原文（原文为空时不补空行）。
func joinIdentity(original string) string {
	if original == "" {
		return identityPrefix
	}
	return identityPrefix + "\n" + original
}

// replaceForeignIdentities 逐条套用 foreignIdentities（全量替换，忽略 ASCII 大小写）。
func replaceForeignIdentities(text string) string {
	for _, rule := range foreignIdentities {
		text = replaceAllFold(text, rule[0], rule[1])
	}
	return text
}

// replaceAllFold 把 text 里的 from 全量替换为 to（忽略 ASCII 大小写）。
func replaceAllFold(text, from, to string) string {
	index := indexFold(text, from)
	if index < 0 {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for index >= 0 {
		out.WriteString(text[:index])
		out.WriteString(to)
		text = text[index+len(from):]
		index = indexFold(text, from)
	}
	out.WriteString(text)
	return out.String()
}

// indexFold 返回 from 在 text 里首次出现的字节下标（忽略 ASCII 大小写），未命中返回 -1。
//
// 按字节滑窗配 strings.EqualFold：from 全是 ASCII，非 ASCII 字节 >= 0x80 折不成
// ASCII，命中区间只可能由 ASCII 字节组成，切片一定是字符边界。
func indexFold(text, from string) int {
	if from == "" {
		return -1
	}
	for start := 0; start+len(from) <= len(text); start++ {
		if strings.EqualFold(text[start:start+len(from)], from) {
			return start
		}
	}
	return -1
}

// normalizeMaxTokens 把出站体里会被上游拒收的小正整数 max_tokens 抬到下限。
//
// 只归一实测过被拒的形态（存在 + 正整数 + < maxTokensFloor）：缺省不注入（上游
// 对缺省是 200，凭空注入反而改变行为），其余形态（≤0 / 非整数 / ≥ 下限 / 非数字）
// 原样交给上游报错。max_completion_tokens 不参与该校验，一字不动。
func normalizeMaxTokens(payload map[string]any) {
	value, okValue := payload["max_tokens"].(float64)
	if !okValue || value <= 0 || value >= maxTokensFloor || value != float64(int64(value)) {
		return
	}
	payload["max_tokens"] = float64(maxTokensFloor)
}

// envelopeError 检查业务信封 {"code","msg"}：code 存在且非 0 时返回 (code, msg, true)。
func envelopeError(body []byte) (int64, string, bool) {
	var probe struct {
		Code    *int64 `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, "", false
	}
	if probe.Code == nil || *probe.Code == 0 {
		return 0, "", false
	}
	msg := probe.Msg
	if msg == "" {
		msg = probe.Message
	}
	return *probe.Code, msg, true
}
