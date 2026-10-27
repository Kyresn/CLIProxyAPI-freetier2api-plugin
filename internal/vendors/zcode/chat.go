package zcode

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// 对话转发：OpenAI chat-completions ↔ Anthropic Messages 双向转换。
//
// 上游就是 Anthropic Messages 协议（模型名大小写敏感），因此这一层的职责是
// 把宿主交过来的 chat-completions 请求翻译过去、再把响应翻译回来。参考
// zcode2api/app/openai_compat.py 与 app/body_transform.py。
//
// 工具调用是必做项而非增强：插件声明只处理 chat-completions，Claude Code /
// Codex 的工具调用全部经此转换，缺了它就只剩纯文本问答。

// ChatRequest 是一次对话转发的输入。
type ChatRequest struct {
	Credential *Credential
	// Body 是宿主翻译好的 chat-completions 请求体。
	Body   []byte
	Stream bool
}

// Chat 执行一次非流式对话，返回 OpenAI 格式的完整响应。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return nil, errBuild
	}

	resp, errDo := hostbridge.Client(ctx, chatHTTPTimeout).Do(httpReq)
	if errDo != nil {
		return nil, transientErr("zcode chat request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return nil, transientErr("read zcode response: " + errRead.Error())
	}
	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	// 上游有时以 HTTP 200 包装业务错误，不能当成成功响应。
	if errUpstream := classifyEmbeddedError(body); errUpstream != nil {
		return nil, errUpstream
	}
	return anthropicToOpenAICompletion(body, model)
}

// ChatStream 执行一次流式对话，逐条投递 OpenAI chunk。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest, onChunk func([]byte) error) error {
	httpReq, model, errBuild := buildHTTPRequest(ctx, baseURL, req)
	if errBuild != nil {
		return errBuild
	}

	// 流式响应总时长不可预期，超时交给上游空闲控制。
	resp, errDo := hostbridge.StreamClient(ctx, 0).Do(httpReq)
	if errDo != nil {
		return transientErr("zcode stream request failed: " + errDo.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return Classify(resp.StatusCode, string(body))
	}

	// 上游有时忽略 stream 参数回一个完整 JSON 文档。校验错误必须在这里拦下，
	// 否则 SSE 解析器会把它当成空流——客户端收到「零内容」且看不到任何原因。
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "event-stream") {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if errUpstream := classifyEmbeddedError(body); errUpstream != nil {
			return errUpstream
		}
		// 正常响应：按非流式转换，再切成单个内容分片投递。
		completion, errConvert := anthropicToOpenAICompletion(body, model)
		if errConvert != nil {
			return errConvert
		}
		return emitCollapsedCompletion(completion, model, onChunk)
	}

	converter := newStreamConverter(model)
	if errStart := converter.emitStart(onChunk); errStart != nil {
		return errStart
	}
	return parseAnthropicSSE(resp.Body, converter, onChunk)
}

// ---- Start Plan 出站闸门 ----

// planQueueWaitTimeout 是排队等待同账号在途对话出清的上限。覆盖常见流式
// 回答长度；超过后按 SoftRate 交还宿主换路，不把请求挂死。
const planQueueWaitTimeout = 90 * time.Second

// errPlanQueueTimeout 表示排队超过上限，未取得出站资格。
var errPlanQueueTimeout = errors.New("zcode start plan queue timeout")

// chatGate 按账号串行化 Start Plan 通道的对话出站。
//
// 为什么需要它：Start Plan 是单用户交互套餐，上游以 3008/3009/3010 明确
// 声明了并发上限，高频并发还会触发 3012「unusual activity」；官方客户端天然
// 串行（一个用户在打字）。本插件把宿主转发来的并发 agent 请求直灌上游，
// 等价于把几十个会话塞进一个交互套餐——上游按设计拒绝。同一账号同一时刻
// 只放行一个在途对话，排队者等前一个出清后再出站，与官方客户端行为一致。
//
// 这不是绕过风控：上游以业务错误码声明了并发上限，客户端遵守自己收到的
// 限制并排队是标准做法；排队等不到时按 SoftRate 交还宿主换路。
//
// 状态属于 vendor 实例（与 claimGate 同一约定）：多 App 并存时各自独立。
type chatGate struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

// newChatGate 构造实例级闸门。
func newChatGate() *chatGate {
	return &chatGate{slots: map[string]chan struct{}{}}
}

// acquire 取得该账号的出站资格并返回交还函数：无在途对话立即返回；否则
// 有界排队，请求 context 取消（宿主断开客户端）时提前退出、不占队列。
func (g *chatGate) acquire(ctx context.Context, accountKey string) (func(), error) {
	slot := g.slot(accountKey)
	timer := time.NewTimer(planQueueWaitTimeout)
	defer timer.Stop()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errPlanQueueTimeout
	}
}

// slot 返回账号对应的闸门。账号数受 core.MaxAccountsPerVendor 约束，无需回收。
func (g *chatGate) slot(accountKey string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	slot, ok := g.slots[accountKey]
	if !ok {
		slot = make(chan struct{}, 1)
		g.slots[accountKey] = slot
	}
	return slot
}

// holdStartPlanSlot 为一次对话取得 Start Plan 出站资格，返回值在成功时必为
// 可调用函数，便于 defer。纯 API Key 通道不受该并发上限约束，直接放行。
func (v *adapter) holdStartPlanSlot(ctx context.Context, cred *Credential) (func(), error) {
	if cred == nil || !cred.UsesPlanChannel() {
		return func() {}, nil
	}
	release, errAcquire := v.gate.acquire(ctx, cred.AccountKey())
	if errAcquire == nil {
		return release, nil
	}
	if errors.Is(errAcquire, errPlanQueueTimeout) {
		return nil, &core.Failure{
			Kind:   core.FailureKindSoftRate,
			Status: 0,
			Msg:    "zcode Start Plan 通道并发排队超时（同账号在途对话未出清），已交还宿主换路",
		}
	}
	// 宿主已断开：不重试，也不计入上游故障。
	return nil, &core.Failure{
		Kind:   core.FailureKindClient,
		Status: 0,
		Msg:    "zcode chat cancelled while queued: " + errAcquire.Error(),
	}
}

// emitCollapsedCompletion 把一次非流式响应折成 OpenAI chunk 序列。
//
// 上游忽略 stream 参数时的兜底路径：客户端发的是流式请求，必须仍然收到
// 完整的 chunk 序列（含结束分片），否则会一直等后续内容。
func emitCollapsedCompletion(completion []byte, model string, onChunk func([]byte) error) error {
	var resp struct {
		Usage   map[string]any `json:"usage"`
		Choices []struct {
			Message      map[string]any `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(completion, &resp); errUnmarshal != nil || len(resp.Choices) == 0 {
		return transientErr("上游返回了非流式响应且无法解析")
	}

	converter := newStreamConverter(model)
	if errStart := converter.emitStart(onChunk); errStart != nil {
		return errStart
	}
	message := resp.Choices[0].Message
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		if errEmit := onChunk(converter.chunk(map[string]any{"reasoning_content": reasoning}, nil)); errEmit != nil {
			return errEmit
		}
	}
	if content, ok := message["content"].(string); ok && content != "" {
		chunk := converter.chunk(map[string]any{"content": content}, nil)
		if errEmit := onChunk(chunk); errEmit != nil {
			return errEmit
		}
	}
	if toolCalls, ok := message["tool_calls"].([]any); ok {
		for index, item := range toolCalls {
			call, _ := item.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": index,
				"id":    core.RawStringField(call, "id"),
				"type":  "function",
				"function": map[string]any{
					"name":      core.RawStringField(fn, "name"),
					"arguments": core.RawStringField(fn, "arguments"),
				},
			}}}
			if errEmit := onChunk(converter.chunk(delta, nil)); errEmit != nil {
				return errEmit
			}
		}
	}
	reason := core.FirstNonEmpty(resp.Choices[0].FinishReason, "stop")
	return onChunk(converter.chunkWithUsage(map[string]any{}, &reason, resp.Usage))
}

// buildHTTPRequest 把 chat-completions 请求体转成上游 Anthropic 请求。
func buildHTTPRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Request, string, error) {
	if req.Credential == nil {
		return nil, "", fmt.Errorf("zcode credential is nil")
	}

	anthropicBody, model, errConvert := openAIToAnthropicBody(req.Body, req.Stream, req.Credential)
	if errConvert != nil {
		return nil, "", errConvert
	}

	target, errTarget := resolveTargetURL(baseURL, req.Credential)
	if errTarget != nil {
		return nil, "", errTarget
	}

	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(anthropicBody))
	if errNew != nil {
		return nil, "", fmt.Errorf("create http request: %w", errNew)
	}
	applyChatHeaders(httpReq, req.Credential)
	return httpReq, model, nil
}

// resolveTargetURL 决定本次对话走哪条通道。
//
// 优先走 Start Plan 通道（zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages，Bearer JWT）；
// 仅持 API Key 的账号走 ZCode 官方网关转发通道（zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages，
// 对齐官方客户端 official-coding-plan-gateway.ts，避免直连 api.z.ai 在国内网络下 DNS 解析失败）。
func resolveTargetURL(baseURL string, cred *Credential) (string, error) {
	if cred == nil {
		return "", fmt.Errorf("zcode credential is nil")
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if cred.UsesPlanChannel() {
		if base == "" {
			base = ZCodeOrigin
		}
		return base + PathPlanMessages, nil
	}
	if strings.TrimSpace(cred.APIKey) != "" {
		if base == "" {
			return ZCodeOrigin + PathUltraZAIMessages, nil
		}
		return base + PathAPIMessages, nil
	}
	return "", fmt.Errorf("zcode: credential missing both jwt_token and api_key")
}

// applyChatHeaders 组装出站请求头：鉴权 + 身份头 + 追踪头。
func applyChatHeaders(req *http.Request, cred *Credential) {
	headers := map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "text/event-stream, application/json",
		"anthropic-version": AnthropicVersion,
	}
	if cred.UsesPlanChannel() {
		headers["Authorization"] = "Bearer " + cred.JWTToken
		headers["x-api-key"] = cred.JWTToken
	} else if strings.TrimSpace(cred.APIKey) != "" {
		headers["x-api-key"] = cred.APIKey
		headers["Authorization"] = "Bearer " + cred.APIKey
	}
	BuildIdentityHeaders(cred.Profile).Apply(headers)
	for key, value := range BuildTraceHeaders() {
		headers[key] = value
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

// openAIToAnthropicBody 把 OpenAI 请求体转换成 Anthropic Messages 格式。
//
// 覆盖：system/developer 提取、多模态 content 分块、tool_calls → tool_use、
// role=tool → tool_result、tools/tool_choice 映射，以及 Plan 通道所需的
// body 变换（system 身份块 / cache_control / metadata.user_id）。
func openAIToAnthropicBody(src []byte, stream bool, cred *Credential) ([]byte, string, error) {
	var in struct {
		Model               string   `json:"model"`
		Messages            []any    `json:"messages"`
		MaxTokens           int      `json:"max_tokens"`
		MaxCompletionTokens int      `json:"max_completion_tokens"`
		Temperature         *float64 `json:"temperature,omitempty"`
		TopP                *float64 `json:"top_p,omitempty"`
		Stream              bool     `json:"stream"`
		Stop                any      `json:"stop"`
		Tools               []any    `json:"tools"`
		ToolChoice          any      `json:"tool_choice"`
	}
	if errUnmarshal := json.Unmarshal(src, &in); errUnmarshal != nil {
		return nil, "", fmt.Errorf("decode chat request: %w", errUnmarshal)
	}

	model := NormalizeModelName(in.Model)
	if model == "" {
		return nil, "", fmt.Errorf("zcode: model is required")
	}

	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = in.MaxCompletionTokens
	}
	if maxTokens <= 0 || maxTokens > MaxTokensLimit {
		// 客户端（如 auto-compact 续传）可能带超限值，上游会报 400/1210，统一钳制。
		maxTokens = DefaultMaxTokens
	}

	systemParts, outMessages := convertMessages(in.Messages)

	out := map[string]any{
		"model":      model,
		"messages":   outMessages,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if stopSequences := normalizeStop(in.Stop); len(stopSequences) > 0 {
		out["stop_sequences"] = stopSequences
	}
	applyTools(out, in.Tools, in.ToolChoice)

	applyPlanBodyTransforms(out, model, cred)

	encoded, errMarshal := json.Marshal(out)
	if errMarshal != nil {
		return nil, "", fmt.Errorf("encode anthropic request: %w", errMarshal)
	}
	return encoded, model, nil
}

// convertMessages 把 OpenAI messages 拆成 (system 文本, Anthropic messages)。
func convertMessages(messages []any) ([]string, []map[string]any) {
	var systemParts []string
	outMessages := make([]map[string]any, 0, len(messages))

	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch strings.TrimSpace(role) {
		case "system", "developer":
			if text := textFromContent(content); strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}

		case "tool":
			// 工具结果在 Anthropic 协议里是 user 消息下的 tool_result 块；连续的 tool/user 消息合并。
			outMessages = appendAnthropicMessage(outMessages, "user", []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": strings.TrimSpace(core.RawStringField(msg, "tool_call_id")),
				"content":     textFromContent(content),
			}})

		case "assistant":
			outMessages = appendAnthropicMessage(outMessages, "assistant", assistantBlocks(content, msg["tool_calls"]))

		default:
			outMessages = appendAnthropicMessage(outMessages, "user", userBlocks(content))
		}
	}
	return systemParts, outMessages
}

// appendAnthropicMessage 追加一条 Anthropic 消息；若与上一条同 role 则合并其 content 块，
// 满足 Anthropic Messages 协议对 user / assistant 严格交替的要求（例如并发工具返回多条 tool 结果）。
func appendAnthropicMessage(out []map[string]any, role string, blocks []any) []map[string]any {
	if len(blocks) == 0 {
		return out
	}
	if n := len(out); n > 0 && core.RawStringField(out[n-1], "role") == role {
		if prev, ok := out[n-1]["content"].([]any); ok {
			out[n-1]["content"] = append(prev, blocks...)
			return out
		}
	}
	return append(out, map[string]any{
		"role":    role,
		"content": blocks,
	})
}

// assistantBlocks 组装 assistant 的 content 块（文本 + tool_use）。
func assistantBlocks(content any, rawToolCalls any) []any {
	blocks := make([]any, 0, 4)
	if text := textFromContent(content); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	toolCalls, _ := rawToolCalls.([]any)
	for _, item := range toolCalls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name := strings.TrimSpace(core.RawStringField(fn, "name"))
		if name == "" {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    core.FirstNonEmpty(core.RawStringField(call, "id"), "toolu_"+NewHexID(12)),
			"name":  name,
			"input": parseToolArguments(fn["arguments"]),
		})
	}
	if len(blocks) == 0 {
		// Anthropic 要求 content 非空。
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	return blocks
}

// userBlocks 组装 user 的 content 块（文本 + 图片）。
func userBlocks(content any) []any {
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		blocks := make([]any, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch core.RawStringField(part, "type") {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": core.RawStringField(part, "text")})
			case "image_url":
				if block := imageBlock(part["image_url"]); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
		return blocks
	default:
		return []any{map[string]any{"type": "text", "text": ""}}
	}
}

// imageBlock 把 OpenAI image_url 转成 Anthropic image 块。
//
// 只支持 data: URL：外链图片需要网关自行下载，上游不接受直接引用。
func imageBlock(raw any) map[string]any {
	url := ""
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		url = core.RawStringField(v, "url")
	}
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	head, data, found := strings.Cut(url, ",")
	if !found || strings.TrimSpace(data) == "" {
		return nil
	}
	mediaType := strings.TrimPrefix(head, "data:")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

// applyTools 把 OpenAI tools / tool_choice 映射到 Anthropic 形态。
func applyTools(out map[string]any, tools []any, toolChoice any) {
	if len(tools) > 0 {
		mapped := make([]any, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tool["function"].(map[string]any)
			if fn == nil {
				fn = tool
			}
			name := strings.TrimSpace(core.RawStringField(fn, "name"))
			if name == "" {
				continue
			}
			entry := map[string]any{"name": name}
			if desc := core.RawStringField(fn, "description"); desc != "" {
				entry["description"] = desc
			}
			if schema, ok := fn["parameters"].(map[string]any); ok && schema != nil {
				entry["input_schema"] = schema
			} else {
				entry["input_schema"] = map[string]any{"type": "object"}
			}
			mapped = append(mapped, entry)
		}
		if len(mapped) > 0 {
			out["tools"] = mapped
		}
	}

	switch choice := toolChoice.(type) {
	case string:
		switch choice {
		case "none":
			delete(out, "tools")
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		}
		// "auto" / 缺省：Anthropic 默认即 auto，无需显式设置。
	case map[string]any:
		fn, _ := choice["function"].(map[string]any)
		if name := strings.TrimSpace(core.RawStringField(fn, "name")); name != "" {
			out["tool_choice"] = map[string]any{"type": "tool", "name": name}
		}
	}
}

// applyPlanBodyTransforms 施加 Plan 通道所需的请求体变换。
//
// 上游网关做内容审查，缺少官方身份块会被拒为 3012；cache_control 与
// metadata.user_id 是官方客户端的固定行为。这些变换只在具备 JWT 时施加——
// API Key 通道不是官方客户端形态，注入反而制造矛盾信号。
func applyPlanBodyTransforms(body map[string]any, model string, cred *Credential) {
	if cred == nil || !cred.UsesPlanChannel() {
		return
	}
	applyStartPlanSystem(body, model)
	applyCacheControl(body)
	if userID := core.FirstNonEmpty(cred.UserID, UserIDFromJWT(cred.JWTToken)); userID != "" {
		metadata, _ := body["metadata"].(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["user_id"] = userID
		body["metadata"] = metadata
	}
}

// anthropicToOpenAICompletion 把 Anthropic 响应转成 OpenAI chat.completion。
func anthropicToOpenAICompletion(raw []byte, model string) ([]byte, error) {
	var resp struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		Usage anthropicUsage `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", errUnmarshal)
	}

	var text strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			reasoning.WriteString(block.Thinking)
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, buildToolCall(block.ID, block.Name, block.Input))
		}
	}

	message := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		message["content"] = text.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	out := map[string]any{
		"id":      core.FirstNonEmpty(resp.ID, "chatcmpl-"+NewHexID(12)),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   core.FirstNonEmpty(resp.Model, model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason(resp.StopReason, len(toolCalls) > 0),
		}},
		"usage": resp.Usage.openAI(),
	}
	return json.Marshal(out)
}

// buildToolCall 组装 OpenAI 形态的工具调用。
func buildToolCall(id, name string, rawInput json.RawMessage) map[string]any {
	input := "{}"
	if len(rawInput) > 0 {
		input = string(rawInput)
	}
	return map[string]any{
		"id":   core.FirstNonEmpty(id, "toolu_"+NewHexID(12)),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": input,
		},
	}
}

// finishReason 把 Anthropic stop_reason 映射成 OpenAI finish_reason。
func finishReason(stopReason string, hasToolCalls bool) string {
	switch stopReason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

// streamConverter 是有状态的 Anthropic SSE → OpenAI chunk 转换器。
//
// 工具调用在流式下分两段到达：content_block_start 给 id/name，
// 随后的 input_json_delta 分批给 arguments 片段，因此必须维护索引映射。
type streamConverter struct {
	model     string
	chunkID   string
	created   int64
	toolIndex int
	usage     anthropicUsage
}

// Anthropic input_tokens excludes cache reads/writes. OpenAI prompt_tokens
// includes both; detail counters are subsets, not additional prompt tokens.
// Pointers distinguish an omitted cumulative counter from an explicit zero.
type anthropicUsage struct {
	InputTokens         *int64 `json:"input_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
}

func (u *anthropicUsage) merge(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	var update anthropicUsage
	if json.Unmarshal(data, &update) != nil {
		return
	}
	// message_delta counters are cumulative snapshots, never increments.
	if update.InputTokens != nil {
		u.InputTokens = update.InputTokens
	}
	if update.OutputTokens != nil {
		u.OutputTokens = update.OutputTokens
	}
	if update.CacheReadTokens != nil {
		u.CacheReadTokens = update.CacheReadTokens
	}
	if update.CacheCreationTokens != nil {
		u.CacheCreationTokens = update.CacheCreationTokens
	}
}

func (u anthropicUsage) openAI() map[string]any {
	if u.InputTokens == nil && u.OutputTokens == nil && u.CacheReadTokens == nil && u.CacheCreationTokens == nil {
		return nil
	}
	value := func(p *int64) int64 {
		if p != nil {
			return *p
		}
		return 0
	}
	read, write := value(u.CacheReadTokens), value(u.CacheCreationTokens)
	input, output := value(u.InputTokens)+read+write, value(u.OutputTokens)
	out := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if u.CacheReadTokens != nil || u.CacheCreationTokens != nil {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": read, "cache_creation_tokens": write}
	}
	return out
}

func newStreamConverter(model string) *streamConverter {
	return &streamConverter{
		model:   model,
		chunkID: "chatcmpl-" + NewHexID(12),
		created: time.Now().Unix(),
	}
}

// emitStart 投递首个 chunk（携带 role）。
func (c *streamConverter) emitStart(onChunk func([]byte) error) error {
	return onChunk(c.chunk(map[string]any{"role": "assistant"}, nil))
}

// feed 处理一条上游事件，产出零到多条 chunk。
func (c *streamConverter) feed(event map[string]any, onChunk func([]byte) error) error {
	switch core.RawStringField(event, "type") {
	case "message_start":
		if msg, ok := event["message"].(map[string]any); ok {
			if id := core.RawStringField(msg, "id"); id != "" {
				c.chunkID = id
			}
			if model := core.RawStringField(msg, "model"); model != "" {
				c.model = model
			}
			c.usage.merge(msg["usage"])
		}

	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		if block != nil && core.RawStringField(block, "type") == "tool_use" {
			index := c.toolIndex
			c.toolIndex++
			return onChunk(c.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": index,
					"id":    core.RawStringField(block, "id"),
					"type":  "function",
					"function": map[string]any{
						"name":      core.RawStringField(block, "name"),
						"arguments": "",
					},
				}},
			}, nil))
		}

	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch core.RawStringField(delta, "type") {
		case "thinking_delta":
			if thinking := core.RawStringField(delta, "thinking"); thinking != "" {
				return onChunk(c.chunk(map[string]any{"reasoning_content": thinking}, nil))
			}
		case "text_delta":
			if text := core.RawStringField(delta, "text"); text != "" {
				return onChunk(c.chunk(map[string]any{"content": text}, nil))
			}
		case "input_json_delta":
			if partial := core.RawStringField(delta, "partial_json"); partial != "" {
				return onChunk(c.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    core.MaxInt(c.toolIndex-1, 0),
						"function": map[string]any{"arguments": partial},
					}},
				}, nil))
			}
		}

	case "message_delta":
		c.usage.merge(event["usage"])
		delta, _ := event["delta"].(map[string]any)
		stop := core.RawStringField(delta, "stop_reason")
		if stop == "" {
			return nil
		}
		reason := finishReason(stop, c.toolIndex > 0)
		// This is the internal CPA executor payload: the host observes usage
		// before protocol translation, independently of client include_usage.
		// Keep it on the terminal choice so cross-protocol translators receive
		// the final counters together with finish_reason. Never emit interim
		// choices:[] snapshots, which clients may interpret as end-of-stream.
		return onChunk(c.chunkWithUsage(map[string]any{}, &reason, c.usage.openAI()))

	case "error":
		errObj, _ := event["error"].(map[string]any)
		return fmt.Errorf("上游流式错误: %s", core.FirstNonEmpty(core.RawStringField(errObj, "message"), "unknown"))
	}
	return nil
}

// chunk 组装一条 OpenAI chat.completion.chunk。
func (c *streamConverter) chunk(delta map[string]any, finishReason *string) []byte {
	return c.chunkWithUsage(delta, finishReason, nil)
}

func (c *streamConverter) chunkWithUsage(delta map[string]any, finishReason *string, usage map[string]any) []byte {
	var finish any
	if finishReason != nil {
		finish = *finishReason
	}
	payload := map[string]any{
		"id":      c.chunkID,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if len(usage) > 0 {
		payload["usage"] = usage
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// parseAnthropicSSE 逐行解析上游 SSE 并交给转换器。
func parseAnthropicSSE(r io.Reader, converter *streamConverter, onChunk func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	// 单条事件可能很大（大段 text_delta / base64 图片），放宽到 8MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if errUnmarshal := json.Unmarshal([]byte(data), &event); errUnmarshal != nil {
			continue
		}
		if errFeed := converter.feed(event, onChunk); errFeed != nil {
			return errFeed
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return transientErr("read zcode stream: " + errScan.Error())
	}
	return nil
}

// classifyEmbeddedError 识别 HTTP 200 包装的业务错误。
//
// 上游对部分错误仍回 200 + {"code":N,...}，当成成功响应会把错误正文
// 当作模型输出吐给客户端。
func classifyEmbeddedError(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var envelope struct {
		Type  string `json:"type"`
		Code  int    `json:"code"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(trimmed, &envelope); errUnmarshal != nil {
		return nil
	}
	// Anthropic 错误信封：{"type":"error","error":{...}}
	if envelope.Type == "error" && envelope.Error.Type != "" {
		return Classify(http.StatusBadRequest, string(trimmed))
	}
	// 业务码信封：有 code 且非 0，且不含 Anthropic 响应标志（content）。
	if envelope.Code != codeSuccess && envelope.Code != 0 && !bytes.Contains(trimmed, []byte(`"content"`)) {
		return Classify(http.StatusOK, string(trimmed))
	}
	return nil
}

// parseToolArguments 把工具参数解析成对象；非法 JSON 包成 _raw 保留原文。
func parseToolArguments(raw any) any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return map[string]any{}
		}
		var parsed map[string]any
		if errUnmarshal := json.Unmarshal([]byte(trimmed), &parsed); errUnmarshal == nil {
			return parsed
		}
		return map[string]any{"_raw": v}
	default:
		return map[string]any{}
	}
}

// normalizeStop 归一化 stop 参数为 Anthropic 的 stop_sequences。
func normalizeStop(stop any) []string {
	switch v := stop.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// textFromContent 从 OpenAI content（字符串或分块数组）提取纯文本。
func textFromContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if core.RawStringField(part, "type") == "text" {
				if text := core.RawStringField(part, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// chatHTTPTimeout 是非流式对话的上限。
const chatHTTPTimeout = 10 * time.Minute

// transientErr 把本地/传输层失败包装成共享的上游错误类型。
//
// 与 Classify 产出的业务错误的区别：Status 为 0，HTTPStatus() 因此回落到
// KindServer 的 502——宿主会换号重试，但不会因为一次半截响应把凭证标记为坏。
// 复用 core.Failure 是刻意的：core.ToPluginError 通过 core.UpstreamError
// 识别这套分类词汇表，
// 自建类型会被降级成通用 502 并丢失全部语义（见 errors.go 的说明）。
func transientErr(msg string) *core.Failure {
	return &core.Failure{Kind: core.FailureKindServer, Status: 0, Msg: msg}
}

// Plan 通道的请求体变换（对齐官方客户端行为）。
//
// 三项变换的作用与必要性：
//  1. system 身份块：上游网关做内容审查，请求缺少官方身份块会被拒为
//     3012「unusual activity」；官客户端固定前置这三个块。
//  2. cache_control：最后一条非 system 消息的末块追加 ephemeral 标记。
//     低于缓存门槛时上游静默忽略，因此无条件追加是安全的。
//  3. metadata.user_id：官方客户端注入了登录用户标识。
//
// 所有变换对畸形输入保持 no-op：变换失败绝不能放大成请求失败。
// 参考 zcode2api/app/body_transform.py。

//go:embed assets/zcode_system.json
var zcodeSystemJSON []byte

type systemBlockTemplate struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

var (
	zcodeSystemOnce   sync.Once
	zcodeSystemBlocks []systemBlockTemplate
)

// systemBlocks 返回官方身份块模板（解析失败时为空，此时跳过注入）。
func systemBlocks() []systemBlockTemplate {
	zcodeSystemOnce.Do(func() {
		var blocks []systemBlockTemplate
		if errUnmarshal := json.Unmarshal(zcodeSystemJSON, &blocks); errUnmarshal != nil {
			blocks = nil
		}
		zcodeSystemBlocks = blocks
	})
	return zcodeSystemBlocks
}

// applyStartPlanSystem 按 ZCode ContextBuilder 规范组装三段式 system（cli_prefix / stable / dynamic）。
//
// 上游网关严格校验这三段结构：追加第 4 个 system 块会触发 HTTP 405 code=3012 拦截。
// 因此客户端传入的 system 提示词按官方 Output Style 槽位内嵌到第 3 段动态块中。
// 幂等：首块已是官方标识时直接返回，重复调用不会重复拼接。
func applyStartPlanSystem(body map[string]any, model string) {
	blocks := systemBlocks()
	if len(blocks) == 0 {
		return
	}

	existing := body["system"]
	if list, ok := existing.([]any); ok && len(list) > 0 {
		if first, ok := list[0].(map[string]any); ok {
			if core.RawStringField(first, "text") == blocks[0].Text {
				return
			}
		}
	}

	modelLine := ""
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		modelLine = "\n- You are powered by the model named account:zai-start-plan/" + trimmed + "."
	}
	outputStyle := ""
	if userSys := extractUserSystemText(existing); userSys != "" {
		outputStyle = "\n\n# Output Style: custom\n" + userSys
	}

	official := make([]any, 0, len(blocks))
	for _, tmpl := range blocks {
		text := strings.ReplaceAll(tmpl.Text, "{{MODEL_LINE}}", modelLine)
		text = strings.ReplaceAll(text, "{{OUTPUT_STYLE}}", outputStyle)
		entry := map[string]any{
			"type": tmpl.Type,
			"text": text,
		}
		if len(tmpl.CacheControl) > 0 {
			cc := make(map[string]any, len(tmpl.CacheControl))
			for k, v := range tmpl.CacheControl {
				cc[k] = v
			}
			entry["cache_control"] = cc
		}
		official = append(official, entry)
	}
	body["system"] = official
}

// extractUserSystemText 提取客户端原有 system 文本。
func extractUserSystemText(system any) string {
	switch v := system.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch block := item.(type) {
			case string:
				if s := strings.TrimSpace(block); s != "" {
					parts = append(parts, s)
				}
			case map[string]any:
				if core.RawStringField(block, "type") == "text" {
					if s := strings.TrimSpace(core.RawStringField(block, "text")); s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// applyCacheControl 给最后一条非 system 消息的末块加 ephemeral 缓存标记。
//
// 幂等：已有 cache_control 时不动。
func applyCacheControl(body map[string]any) {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok || core.RawStringField(msg, "role") == "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			msg["content"] = []any{map[string]any{
				"type":          "text",
				"text":          content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		case []any:
			if len(content) == 0 {
				return
			}
			last, ok := content[len(content)-1].(map[string]any)
			if !ok || last["cache_control"] != nil {
				return
			}
			last["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		return
	}
}

// 上游常量收口：URL、端点、客户端指纹与模型名。
//
// 约定与其它供应商一致：**禁止在别处硬编码上游常量**，一律引用本文件。

const (
	// VendorID 是本供应商在 freetier2api 插件内的唯一标识。
	VendorID = "zcode"
	// VendorName 是管理端与列表的展示名。
	VendorName = "ZCode"

	// ── 上游 origin ─────────────────────────────────────────────────────────

	// ZCodeOrigin 承载 Start Plan 对话通道、Ultra 网关、计费族与 OAuth CLI。
	ZCodeOrigin = "https://zcode.z.ai"
	// ZAIOrigin 承载 API Key 兑换链。
	ZAIOrigin = "https://api.z.ai"

	// ── 对话端点 ────────────────────────────────────────────────────────────

	// PathPlanMessages 是 Start Plan 通道（Bearer JWT）。
	PathPlanMessages = "/api/v1/zcode-plan/anthropic/v1/messages"
	// PathUltraZAIMessages 是 ZCode 官方网关对 api.z.ai 的代理路径（对齐 official-coding-plan-gateway.ts）。
	PathUltraZAIMessages = "/api/v1/ultra-zai/anthropic/v1/messages"
	// PathAPIMessages 是自定义 baseURL 时的直连 Anthropic Messages 路径。
	PathAPIMessages = "/api/anthropic/v1/messages"

	// ── OAuth CLI（headless，服务端中转）─────────────────────────────────────
	//
	// 从 origin 起算的完整路径（与 ZCodeOrigin 拼接后使用）。

	PathOAuthInit = "/api/v1/oauth/cli/init"
	PathOAuthPoll = "/api/v1/oauth/cli/poll" // + /{flow_id}

	// ── API Key 兑换链（api.z.ai）───────────────────────────────────────────

	PathZLogin       = "/api/auth/z/login"
	PathCustomerInfo = "/api/biz/customer/getCustomerInfo"

	// ── 计费 / 额度（zcode.z.ai）────────────────────────────────────────────

	// BillingBase 是计费族基准（balance/preview/claim）。
	//
	// billing/current 已废弃（官方源码 codingPlanProviderAvailability.ts 与
	// bigmodelUsageQuotaProvider.ts 均标注），持有套餐改由 billing/balance 的
	// data.plans 判定，见 tasks.go 的 fetchPlanSnapshot。
	BillingBase = ZCodeOrigin + "/api/v1/zcode-plan"

	// PathBillingBalance 同时返回 data.plans（持有套餐）与 data.balances（余量）。
	PathBillingBalance = "/billing/balance"
	// PathBillingPreview 列出上游当前投放的套餐（未过滤已持有）。
	PathBillingPreview = "/billing/preview"
	// PathBillingClaim 领取套餐，需要人机验证令牌。
	PathBillingClaim  = "/billing/claim"
	PathUsage         = "/usage"
	PathClientConfigs = "/api/v1/client/configs"

	// PathEventReport 是客户端埋点上报端点。
	//
	// **不在 zcode-plan 之下**（拼 BillingBase 会 404），也不带 Authorization。
	// 上游把它当作活动套餐的投放资格信号：官方桌面端每次启动发 app_launch、
	// 每天发一次 app_daily_active，缺了这两条 billing/preview 就恒空。
	PathEventReport = "/api/v1/event/report"

	// ── 客户端指纹 ──────────────────────────────────────────────────────────

	// ClientAppVersion 是官方桌面端现行版本。
	//
	// 上游按客户端版本判定兼容性（版本过旧会被拒），因此跟进官方升版时要
	// 同步改这里。
	ClientAppVersion = "3.16.2"

	// UserAgent 与 ClientAppVersion 同源，避免两者漂移。
	UserAgent = "ZCode/" + ClientAppVersion

	AnthropicVersion = "2023-06-01"
	ClientAgent      = "glm"
	HTTPReferer      = ZCodeOrigin + "/"
	ClientTitle      = "Z Code@electron"
	ReleaseChannel   = "stable"

	// DefaultPlatform/DefaultOSVersion/DefaultLanguage/DefaultTimezone 是
	// **无账号上下文**时的伪装值（模型探测、测试）。有账号时一律取该账号的
	// 设备档案——每账号一台独立设备，避免多账号聚成「同一台机器」。
	DefaultPlatform  = "darwin-arm64"
	DefaultOSVersion = "25.5.0"
	DefaultLanguage  = "zh-CN"
	DefaultTimezone  = "Asia/Shanghai"

	// ── 限制 ────────────────────────────────────────────────────────────────

	// MaxTokensLimit 是上游 max_tokens 的合法上限（超限报 400 code 1210）。
	MaxTokensLimit = 131072

	// DefaultMaxTokens 是请求未指定 max_tokens 时的取值。
	DefaultMaxTokens = 8192

	// ── 验证码头 ────────────────────────────────────────────────────────────

	// CaptchaHeader 是 Plan 通道的验证令牌；REGION 与它成对下发，缺失易 3007。
	CaptchaHeader       = "X-Aliyun-Captcha-Verify-Param"
	CaptchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)

// client/configs 拉不到验证码配置时的兜底值（prefix / region / scene_id）。
//
// 与参考实现 zcode2api/app/constants.py 的 CAPTCHA_DEFAULTS 同源：这三个值是
// 官方桌面端现行配置，configs 端点不可用时用它们仍能渲染滑块。
const (
	captchaFallbackPrefix = "no8xfe"
	captchaFallbackRegion = "cn"
	captchaFallbackScene  = "11xygtvd"
)

// 本文件解析 ZCode 的供应商专属配置。
