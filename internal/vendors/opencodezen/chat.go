package opencodezen

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 本文件实现 OpenCode ZEN 的 chat-completions 转发。
//
// 与 WorkBuddy / Qoder 的差异：ZEN 上游是标准 OpenAI chat-completions。
// 特殊要求（参考 opencode2api 与 OpenCode 官方实现）：
//   1. 会话 ID 必须是 canonical session 格式：`ses_` + 12位十六进制 + 14位 Base62。
//   2. 免费层模型（ID 包含 "free"）要求完整的官方 Agent 请求形态：走 inference 车道、
//      以匿名 public 凭证鉴权、stream=true，并声明官方 OpenCode 工具集
//      （见 assets/free-tier-tools.json）。
//      非流式调用在插件内会自动开启 stream 并在上游返回后通过 CollapseSSEToJSON 合并为标准 JSON 响应。

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	// APIKey 是付费模型的出站凭证；免费层改用匿名 public 凭证。
	APIKey string
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body []byte
	// Stream 报告是否流式。
	Stream bool
	// Catalog 是 models.dev 目录，用于按单价判定免费层。
	// 为空时退回按模型名判定。
	Catalog modelsDevCatalog
}

// Chat 执行一次非流式对话，返回上游的完整响应体。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	modelName := extractModelName(req.Body)
	isFree := isFreeTierModel(modelName, req.Catalog)

	if isFree {
		// 免费层只接流式：强制 stream 打开，再把 SSE 聚合回完整 JSON 文档。
		req.Stream = true
		resp, errStream := ChatStream(ctx, baseURL, req)
		if errStream != nil {
			return nil, errStream
		}
		defer func() { _ = resp.Body.Close() }()

		return CollapseSSEToJSON(resp.Body, modelName)
	}

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
		return nil, Classify(resp.StatusCode, string(body))
	}
	return body, nil
}

// ChatStream 执行一次流式对话，返回上游响应体（调用方负责关闭）。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	httpReq, errReq := newChatRequest(ctx, baseURL, req, req.Catalog)
	if errReq != nil {
		return nil, errReq
	}

	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat stream: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, Classify(resp.StatusCode, string(body))
	}
	return resp, nil
}

// doChat 发起一次非流式对话请求。
func doChat(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	httpReq, errReq := newChatRequest(ctx, baseURL, req, req.Catalog)
	if errReq != nil {
		return nil, errReq
	}

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, &core.Failure{Msg: "chat: " + errDo.Error(), Kind: core.FailureKindTransient}
	}
	return resp, nil
}

// newChatRequest 组装一次 chat-completions 请求（流式与非流式共用）。
//
// 免费与付费走同一条 inference 车道，区别只在凭证：免费用匿名 public，
// 付费用用户 key。是否免费由 models.dev 目录的单价判定，见 isFreeTierModel。
func newChatRequest(ctx context.Context, baseURL string, req ChatRequest, catalog modelsDevCatalog) (*http.Request, error) {
	body := req.Body
	credential := req.APIKey
	if isFreeTierModel(extractModelName(body), catalog) {
		body = prepareFreeTierBody(body)
		credential = freeTierCredential
	} else {
		body = normalizeMaxTokens(body)
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, chatEndpoint(baseURL), bytesReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("build chat request: %w", errReq)
	}
	ApplyChatHeaders(httpReq, credential, sessionIDFor(body))
	return httpReq, nil
}

// normalizeMaxTokens 把 max_tokens 归一到 max_completion_tokens。
//
// 官方 2.1.4 对 opencode 家族的模型一律发 max_completion_tokens：目录里
// 只声明了 compatibility.reasoning_field、没有 max_tokens_field，因此
// detectMaxTokensField（packages/ai/src/protocols/openai-chat.ts）落到默认
// 分支。宿主透传的是客户端原始字段名，这里补齐以免上游按未知字段忽略上限。
//
// 两个字段都给时以 max_completion_tokens 为准并丢掉 max_tokens，避免同一
// 请求里出现两个上限字段。解析失败原样返回，交给上游报错。
func normalizeMaxTokens(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	value, hasMax := payload["max_tokens"]
	if !hasMax {
		return body
	}
	if _, hasCompletion := payload["max_completion_tokens"]; !hasCompletion {
		payload["max_completion_tokens"] = value
	}
	delete(payload, "max_tokens")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// chatEndpoint 返回 chat-completions 的完整地址。
//
// 免费与付费共用同一出口（官方 v2 就是这么分流凭证的），因此不再按
// 是否免费分层。
func chatEndpoint(override string) string {
	return BaseURL(override) + ChatPath
}

// extractModelName 从请求体中提取 model 字符串。
func extractModelName(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

// isFreeTierModel 判断是否为免费层模型。
//
// 判定口径对齐官方 2.1.4：看模型单价是否为 0，**不看模型名**。
// 官方在同名维度上用的是 `!model.cost.some(c => c.input > 0)`
// （packages/core/src/plugin/provider/opencode.ts），而目录里存在零价
// 但名字不带 "free" 的模型（如 big-pickle，且它是官方登出状态的默认
// 模型）。按名字判定会把这类模型错误地推上付费车道。
//
// 单价来自 models.dev 目录；目录不可用时只能退回名字判定（见
// catalogIsFree）。这里额外接受显式的 "-free" 后缀作为兜底信号。
func isFreeTierModel(model string, catalog modelsDevCatalog) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if free, known := catalogIsFree(catalog, model); known {
		return free
	}
	return strings.Contains(strings.ToLower(model), "free")
}

// catalogIsFree 用目录里该模型的单价回答「是否免费」。
//
// known=false 表示目录没有这个模型（或没有 cost 块），调用方需自行兜底：
// 缺数据不能当成"免费"，也不能当成"付费"。
func catalogIsFree(catalog modelsDevCatalog, model string) (free bool, known bool) {
	entry, okEntry := catalog[model]
	if !okEntry || entry.Cost == nil {
		return false, false
	}
	return entry.Cost.Input == 0 && entry.Cost.Output == 0, true
}

// prepareFreeTierBody 规范化免费层请求体：强制流式并声明用量统计。
//
// **不注入任何工具定义**。官方客户端只透传当前会话真正启用的工具
// （packages/core/src/session/model-request.ts），全仓库没有"按模型补
// 工具"的分支；凭空塞一份工具集既与官方行为不符，也让每个请求多背
// 十几 KB 载荷。调用方自带什么就发什么。
func prepareFreeTierBody(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	payload["stream"] = true
	if opt, ok := payload["stream_options"].(map[string]any); ok {
		opt["include_usage"] = true
	} else {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// chatCompletionResponse 用于将 SSE 流合并为一个完整的 chat.completion JSON。
type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type chatMessage struct {
	Role             string `json:"role"`
	Content          any    `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
	ToolCalls        []any  `json:"tool_calls,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type sseDeltaChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []any  `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// CollapseSSEToJSON 将上游 SSE 流消费并组装成一个完整的 OpenAI 格式 chat.completion JSON 响应。
func CollapseSSEToJSON(r io.Reader, defaultModel string) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	resp := chatCompletionResponse{
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   defaultModel,
		Choices: []chatChoice{
			{
				Index: 0,
				Message: chatMessage{
					Role: "assistant",
				},
				FinishReason: "stop",
			},
		},
	}

	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder
	var toolCalls []any

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk sseDeltaChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.ID != "" {
			resp.ID = chunk.ID
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Created > 0 {
			resp.Created = chunk.Created
		}
		if chunk.Usage != nil {
			resp.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
			}
			if choice.Delta.Reasoning != "" {
				reasoningBuilder.WriteString(choice.Delta.Reasoning)
			} else if choice.Delta.ReasoningContent != "" {
				reasoningBuilder.WriteString(choice.Delta.ReasoningContent)
			}
			if len(choice.Delta.ToolCalls) > 0 {
				toolCalls = mergeToolCalls(toolCalls, choice.Delta.ToolCalls)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				resp.Choices[0].FinishReason = *choice.FinishReason
			}
		}
	}

	contentStr := contentBuilder.String()
	resp.Choices[0].Message.Content = contentStr
	resp.Choices[0].Message.ReasoningContent = reasoningBuilder.String()

	toolCalls = filterValidToolCalls(toolCalls)
	if len(toolCalls) > 0 {
		resp.Choices[0].Message.ToolCalls = toolCalls
		resp.Choices[0].FinishReason = "tool_calls"
		if contentStr == "" {
			resp.Choices[0].Message.Content = nil
		}
	}

	return json.Marshal(resp)
}

func mergeToolCalls(accumulated []any, incoming []any) []any {
	for _, raw := range incoming {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		idx := core.IntFromAny(call["index"])
		var target map[string]any
		for _, item := range accumulated {
			if existing, okMap := item.(map[string]any); okMap {
				if core.IntFromAny(existing["index"]) == idx {
					target = existing
					break
				}
			}
		}
		if target == nil {
			target = cloneToolCall(call)
			target["index"] = idx
			accumulated = append(accumulated, target)
			continue
		}
		if id, okID := call["id"].(string); okID && id != "" {
			target["id"] = id
		}
		if tp, okTp := call["type"].(string); okTp && tp != "" {
			target["type"] = tp
		}
		if fnIncoming, okFn := call["function"].(map[string]any); okFn {
			fnTarget, _ := target["function"].(map[string]any)
			if fnTarget == nil {
				fnTarget = map[string]any{}
				target["function"] = fnTarget
			}
			if name, okName := fnIncoming["name"].(string); okName && name != "" {
				fnTarget["name"] = name
			}
			if args, okArgs := fnIncoming["arguments"].(string); okArgs && args != "" {
				prevArgs, _ := fnTarget["arguments"].(string)
				fnTarget["arguments"] = prevArgs + args
			}
		}
	}
	return accumulated
}

func filterValidToolCalls(calls []any) []any {
	var valid []any
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, okFn := call["function"].(map[string]any)
		if !okFn {
			continue
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		valid = append(valid, call)
	}
	for i := 1; i < len(valid); i++ {
		for j := i; j > 0; j-- {
			left, _ := valid[j-1].(map[string]any)
			right, _ := valid[j].(map[string]any)
			if core.IntFromAny(left["index"]) > core.IntFromAny(right["index"]) {
				valid[j-1], valid[j] = valid[j], valid[j-1]
			}
		}
	}
	return valid
}

func cloneToolCall(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for k, v := range src {
		if k == "function" {
			if fn, ok := v.(map[string]any); ok {
				fnCopy := make(map[string]any, len(fn))
				for fk, fv := range fn {
					fnCopy[fk] = fv
				}
				dst[k] = fnCopy
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// canonicalSessionPattern 匹配 OpenCode 官方的 session 形态：
// "ses_" + 12 位小写十六进制 + 14 位 Base62。
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// descendingTimePartPattern 匹配官方客户端 Identifier.descending() 铸造的
// 时间部分：48 位值 ~(ts_ms<<12|counter) 的十六进制。按位取反会把低 12 位
// 置成 0xFFF-counter，因此真实时间部分以 f[c-f] 收尾。
//
// **这一段是免费层的硬门槛**：形态对但用随机十六进制填时间部分，上游照样
// 403 FreeTierError——它校验的是编码方式而不只是长度。
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// sessionIDFor 由请求体派生本次请求的 session ID。
//
// 同一段对话（首条 user 消息相同）会得到同一个 session，既通过免费层的
// 编码校验，也保住上游的 prompt cache 亲和性；派生不出信号时退回随机。
func sessionIDFor(body []byte) string {
	return canonicalSessionID(conversationSeed(body))
}

// canonicalSessionID 把任意信号确定性地重编码成官方 session 形态。
// 已经是官方铸造（形态与时间部分编码都对）的信号原样保留，以免破坏亲和性。
func canonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) &&
		descendingTimePartPattern.MatchString(signal[4:16]) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := descendingTimePart(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// conversationSeed 取首条 user 消息的序列化内容作为会话种子。
//
// 用「开头」而不是整段历史：多轮对话越聊越长，用全文做种子会让同一段对话
// 每轮都换一个 session，prompt cache 永远命中不了。
func conversationSeed(body []byte) string {
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return randomSessionSignal()
	}
	for _, message := range payload.Messages {
		if message.Role != "user" || message.Content == nil {
			continue
		}
		encoded, errMarshal := json.Marshal(message.Content)
		if errMarshal == nil && string(encoded) != "null" {
			return string(encoded)
		}
	}
	if trimmed := strings.TrimSpace(payload.PreviousResponseID); trimmed != "" {
		return trimmed
	}
	return randomSessionSignal()
}

// randomSessionSignal 在请求体给不出任何信号时提供一次性种子。
func randomSessionSignal() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败时仍要产出合法 session；此处只影响缓存亲和性。
		return "fallback"
	}
	return hex.EncodeToString(buf)
}

// descendingTimePart 把六个哈希字节渲染成官方 Identifier.descending() 的
// 布局：36 位时间戳顶到 48 位字的高位，OR 上计数器 1，再按位取反，
// 于是低 12 位读作 ffe。
func descendingTimePart(hash []byte) string {
	current := (uint64(hash[0])<<28 | uint64(hash[1])<<20 | uint64(hash[2])<<12 |
		uint64(hash[3])<<4 | uint64(hash[4])>>4) << 12
	current |= 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// base62Alphabet 是 Base62 字符集。
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// bytesReader 把字节切片包成 io.Reader。
func bytesReader(data []byte) io.Reader {
	return bytes.NewReader(data)
}

// 本文件定义 OpenCode ZEN 的上游端点与请求头。
//
// 端点取自官方 2.1.4 客户端的模型目录（models.dev provider "opencode" 的
// api 字段）：付费与免费模型都走 inference 车道，区别只在凭证——免费用
// 匿名 public，付费用用户 key。历史上的 https://opencode.ai/zen 只是网页
// 路径与用量查询端点，已不再是推理基址。
//
// **本插件只处理 chat-completions**（Claude / Codex 客户端由宿主翻译），
// 因此这里只保留 chat 路径。上游还提供 /v1/responses、/v1/messages、
// /v1/systemone，那些由宿主侧的多协议翻译覆盖，插件不重复实现。

const (
	// DefaultBaseURL 是 chat-completions 的上游基地址。
	DefaultBaseURL = "https://opencode.ai/inference/openai"
	// ChatPath 是 chat-completions 的路径。
	ChatPath = "/v1/chat/completions"
	// ModelsBaseURL 是模型清单的基地址，比 chat 少一层 /openai：
	// /inference/openai/v1/models 实测 404，清单在 /inference/v1/models。
	ModelsBaseURL = "https://opencode.ai/inference"
	// ModelsPath 是模型清单的路径。
	ModelsPath = "/v1/models"
	// freeTierCredential 是官方客户端为免费层使用的匿名凭证：
	// 付费 key 在这条车道上会被拒（实测 401 Missing API key）。
	freeTierCredential = "public"

	// clientVersion 是伪装用的 OpenCode 客户端版本，对齐 v2 稳定版。
	//
	// 取值来自官方仓库 packages/cli/package.json（2.1.4），不是推测；
	// 1.x 已被上游拒绝（见 issue #43）。
	clientVersion = "2.1.4"
	// clientChannel 是官方稳定通道名，见官方 packages/script/src/index.ts：
	// 稳定版 CHANNEL 恒为 "latest"，其余值都是预览通道。
	clientChannel = "latest"
	// clientName 是官方 CLI 的客户端标识（x-opencode-client 与 UA 共用）。
	clientName = "cli"
	// userAgentFormat 是 v2 的 UA 形态：opencode/<channel>/<version>/<name>，
	// 见官方 packages/core/src/app.ts 的 App.useragent。1.x 的两段式
	// "opencode/<版本> (<os> <arch>; <go 版本>)" 已不再被识别。
	userAgentFormat = "opencode/%s/%s/%s"
)

// BaseURL 返回 chat-completions 的上游基地址（末尾无斜杠）。
//
// 允许配置覆盖：上游换域名或用户要走自建网关时不必改代码。
func BaseURL(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	return trimmed
}

// ModelsBaseURLFor 返回模型清单的上游基地址（末尾无斜杠）。
//
// 与 chat 出口不同层：chat 在 /inference/openai，清单在 /inference。
// 覆盖值只写了 chat 那层的（含 /openai 后缀）时，清单层退掉这一层，
// 让自建网关仍然只有一个配置项。
func ModelsBaseURLFor(override string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(override), "/")
	if trimmed == "" {
		return ModelsBaseURL
	}
	return strings.TrimSuffix(trimmed, "/openai")
}

// UserAgent 返回伪装成官方 OpenCode 客户端的 UA。
//
// 形态与官方 v2 客户端一致：opencode/<channel>/<version>/<name>。
func UserAgent() string {
	return fmt.Sprintf(userAgentFormat, clientChannel, clientVersion, clientName)
}

// ApplyChatHeaders 写入 chat-completions 请求所需的全部头。
//
// 认证与客户端标识是**硬门槛**：ZEN 上游按 x-opencode-client 与 UA 判定
// 请求来源，缺失会被当成未知客户端拒绝。
func ApplyChatHeaders(req *http.Request, apiKey, sessionID string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", clientName)
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-session-id", sessionID)
	req.Header.Set("x-session-affinity", sessionID)
	req.Header.Set("X-Session-Id", sessionID)
	req.Header.Set("x-opencode-project", "global")
}

// ApplyModelsHeaders 写入模型清单请求的头（只需认证）。
func ApplyModelsHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-client", clientName)
}
