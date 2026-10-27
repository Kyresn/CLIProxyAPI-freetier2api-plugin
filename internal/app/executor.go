package app

// 本文件实现 executor 与 quota 两类宿主能力：
//   - executor：非流式/流式执行、token 估算、出站 HTTP 声明；
//   - quota：额度查询与「签到式重置」。
//
// 真正的上游协议调用由各供应商的 Execute/Quota/Checkin 实现完成。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/tasks"
)

const (
	// executorRPCTimeout 是非流式执行的内部上限（流式另有空闲控制）。
	executorRPCTimeout = 10 * time.Minute
	// streamEmitTimeout 是单次流分片投递的上限。
	streamEmitTimeout = 30 * time.Second
)

// executorRPCRequest 与宿主的 rpcExecutorRequest 对齐。
//
// 注意：内嵌字段按 Go 字段名（无 json tag）传输，stream_id / host_callback_id 用小写键。
type executorRPCRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse 与宿主的 rpcExecutorStreamResponse 对齐。
//
// 本插件走「同步返回 headers + 异步 emit 分片」，因此只返回 headers。
type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

// preparedExecution 汇集一次执行的同步校验结果。
type preparedExecution struct {
	rpc        executorRPCRequest
	vendor     core.Vendor
	credential *core.Credential
	model      string
	// modelAliased 报告请求命中了别名：出站前需要把请求体里的模型名一并还原。
	modelAliased bool
}

// executeRequest 把宿主的执行请求转成供应商无关的形态。
func (p preparedExecution) executeRequest() *core.ExecuteRequest {
	payload := p.rpc.Payload
	if p.modelAliased {
		// 供应商的对话实现多从请求体读 model（Qoder / Trae 等），别名必须在这里
		// 一并还原成官方 ID，否则上游收到的是别名。
		payload = rewritePayloadModel(payload, p.model)
	}
	return &core.ExecuteRequest{
		Model:    p.model,
		Payload:  payload,
		Headers:  p.rpc.Headers,
		ClientIP: core.ExtractClientIP(p.rpc.Headers),
		Stream:   p.rpc.Stream,
	}
}

// handleExecutorExecute 执行一次非流式对话。
func (a *App) handleExecutorExecute(request []byte) ([]byte, error) {
	prepared, errPrepare := a.prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	ctx, cancel := context.WithTimeout(
		hostbridge.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID), executorRPCTimeout)
	defer cancel()

	response, errExecute := prepared.vendor.Execute(ctx, prepared.credential, prepared.executeRequest())
	if errExecute != nil {
		return nil, core.ToPluginError(errExecute)
	}
	return core.OKEnvelope(response)
}

// handleExecutorExecuteStream 执行一次流式对话。
//
// 架构：**同步返回响应头 + goroutine 异步投递分片**。
// 宿主先拿到 headers 构造下游响应，之后通过 host.stream.emit 接收分片、
// 通过 host.stream.close 收到结束信号。
func (a *App) handleExecutorExecuteStream(request []byte) ([]byte, error) {
	prepared, errPrepare := a.prepareExecution(request)
	if errPrepare != nil {
		return nil, errPrepare
	}
	if strings.TrimSpace(prepared.rpc.StreamID) == "" {
		return nil, core.NewPluginError("invalid_request", "stream_id is required for streaming execution", http.StatusBadRequest)
	}

	// 头部必须同步返回：宿主用它们构造下游响应。
	streamHeaders := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"Cache-Control": []string{"no-cache"},
	}

	streamCtx, cancelStream, finishStream := a.BeginPluginStream()
	go func() {
		defer finishStream()
		a.runStreamExecution(streamCtx, cancelStream, prepared)
	}()

	return core.OKEnvelope(rpcExecutorStreamResponse{Headers: streamHeaders})
}

// rewritePayloadModel 把请求体里的 model 字段改写成官方 ID。
//
// 解析失败时原样返回：宁可不改写，也不要让一次对话因为本地 JSON 解析失败而中断——
// 上游的真实报错比本地解析错误更有诊断价值（与 workbuddy.PrepareBody 同口径）。
func rewritePayloadModel(payload []byte, model string) []byte {
	if len(payload) == 0 || strings.TrimSpace(model) == "" {
		return payload
	}
	var obj map[string]any
	if errUnmarshal := json.Unmarshal(payload, &obj); errUnmarshal != nil || obj == nil {
		return payload
	}
	obj["model"] = model
	encoded, errMarshal := json.Marshal(obj)
	if errMarshal != nil {
		return payload
	}
	return encoded
}

// runStreamExecution 在后台把上游 SSE 转发给宿主流。
func (a *App) runStreamExecution(streamCtx context.Context, cancelStream context.CancelFunc, prepared preparedExecution) {
	ctx := hostbridge.WithCallbackID(streamCtx, prepared.rpc.HostCallbackID)
	streamID := prepared.rpc.StreamID
	callbackID := prepared.rpc.HostCallbackID

	sink := &hostStreamSink{
		callbackID:   callbackID,
		streamID:     streamID,
		cancelStream: cancelStream,
	}
	errForward := prepared.vendor.ExecuteStream(ctx, prepared.credential, prepared.executeRequest(), sink)
	// 下游主动断开会取消上下文，那不是错误，不该发错误帧给客户端。
	if errForward != nil && streamCtx.Err() == nil {
		closeStream(callbackID, streamID, streamFailureMessage(errForward))
		return
	}
	closeStream(callbackID, streamID, "")
}

// hostStreamSink 把宿主的流式投递能力包装成 core.StreamSink。
//
// 供应商只负责产出分片内容，投递（host.stream.emit）与收尾由根层统一做——
// 这段逻辑宿主契约相关，不该让每个供应商各写一遍。
type hostStreamSink struct {
	callbackID   string
	streamID     string
	cancelStream context.CancelFunc
}

// Emit 只投递裸数据载荷：宿主的 OpenAI handler 负责 data: 前缀、空行与 [DONE]。
func (s *hostStreamSink) Emit(payload []byte) error {
	frame := executorStreamPayload(payload)
	if len(frame) == 0 {
		return nil
	}
	if errEmit := emitStreamChunk(s.callbackID, s.streamID, frame); errEmit != nil {
		// 下游断开：取消上游读取，静默收尾（这不是错误）。
		s.cancelStream()
		return errEmit
	}
	return nil
}

// executorStreamPayload 把单个供应商分片归一为宿主 executor 接受的裸载荷。
// SSE 控制行不能当作 data 投递，否则心跳会变成不可解析的 data: : heartbeat。
func executorStreamPayload(payload []byte) []byte {
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) ||
		bytes.HasPrefix(payload, []byte(":")) ||
		bytes.HasPrefix(payload, []byte("event:")) ||
		bytes.HasPrefix(payload, []byte("id:")) ||
		bytes.HasPrefix(payload, []byte("retry:")) {
		return nil
	}
	return payload
}

// handleResponseNormalizeBefore 仅在跨协议流式转换前补上转换器需要的 SSE 信封。
// 同格式 OpenAI 响应直接交给 handler，不能在 executor 出口提前包装。
//
// 宿主对每一条 SSE 行都调一次，参数里带着完整客户端请求与上游请求（可达数 MB）。
// 因此这里绝不整包解码 ResponseTransformRequest，取值走 scanResponseTransform
// 的单次线性扫描（见 responsegate.go 的说明）。
//
// 返回空 PayloadResponse 表示「未修改」——宿主对此明确按 no-op 处理
// （callResponseNormalizer 在 len(Body)==0 时保留原 body）。
func (a *App) handleResponseNormalizeBefore(request []byte) ([]byte, error) {
	body, okChunk := normalizeResponseChunk(request)
	if !okChunk || !json.Valid(bytes.TrimSpace(body)) {
		return core.OKEnvelope(pluginapi.PayloadResponse{})
	}
	return core.OKEnvelope(pluginapi.PayloadResponse{Body: formatSSEChunk(body)})
}

// formatSSEChunk 只供转换前适配使用；executor 分片不能调用它。
func formatSSEChunk(payload []byte) []byte {
	trimmed := bytes.TrimRight(payload, "\r\n ")
	if len(trimmed) == 0 {
		return nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) ||
		bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.HasPrefix(trimmed, []byte(":")) {
		return append(trimmed, '\n', '\n')
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return []byte("data: [DONE]\n\n")
	}
	out := make([]byte, 0, len(trimmed)+8)
	out = append(out, []byte("data: ")...)
	out = append(out, trimmed...)
	out = append(out, '\n', '\n')
	return out
}

// Close 结束流。错误帧由调用方在拿到 ExecuteStream 的返回值后统一发，
// 这里不重复发——同一条流发两次关闭会让客户端收到重复的结束事件。
func (s *hostStreamSink) Close(errMsg string) error { return nil }

// prepareExecution 完成同步校验：请求解码、凭证解析、模型与域解析。
func (a *App) prepareExecution(request []byte) (preparedExecution, error) {
	var prepared preparedExecution
	if errDecode := decodeRequest(request, &prepared.rpc); errDecode != nil {
		return prepared, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}

	// 本插件只声明 chat-completions。宿主若送来其它格式，说明双方声明不一致，
	// 明确报错比静默按错误格式处理更容易定位。
	if format := strings.TrimSpace(prepared.rpc.Format); format != "" && !isChatFormat(format) {
		return prepared, core.NewPluginError("unsupported_format",
			fmt.Sprintf("unsupported protocol format %q (this plugin only handles %s)", format, formatChatCompletions),
			http.StatusBadRequest)
	}
	if format := strings.TrimSpace(prepared.rpc.SourceFormat); format != "" && !isChatFormat(format) {
		return prepared, core.NewPluginError("unsupported_format",
			fmt.Sprintf("unsupported source format %q (this plugin only handles %s)", format, formatChatCompletions),
			http.StatusBadRequest)
	}

	ctx := hostbridge.WithCallbackID(context.Background(), prepared.rpc.HostCallbackID)
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, prepared.rpc.HostCallbackID,
		prepared.rpc.StorageJSON, prepared.rpc.AuthID, prepared.rpc.AuthAttributes)
	if errResolve != nil {
		return prepared, errResolve
	}
	prepared.vendor = vendor
	prepared.credential = credential

	// 模型带的供应商前缀优先；无前缀时就用裸名（宿主按 EqualFold 合并同名模型）。
	_, bareModel := a.registry.SplitModelID(prepared.rpc.Model)
	if strings.TrimSpace(bareModel) == "" {
		return prepared, core.NewPluginError("invalid_request", "model is required", http.StatusBadRequest)
	}

	// 客户端请求的是别名时还原成上游官方 ID：禁用校验与出站都用官方 ID，
	// 因此还原必须发生在两者之前。
	prepared.model = a.resolveOutboundModel(vendor.ID(), bareModel)
	prepared.modelAliased = prepared.model != bareModel

	// 检查模型是否已被插件配置禁用（禁用键是 vendor:model，跨供应商互不影响）。
	// 用官方 ID 判定：管理页展示与写入的禁用键都是官方 ID，别名只是对外名字。
	if a.isModelDisabled(vendor.ID(), prepared.model) {
		return prepared, core.NewPluginError("model_disabled",
			fmt.Sprintf("model %q is disabled by plugin configuration", prepared.rpc.Model),
			http.StatusBadRequest)
	}

	// 供应商所在的区域被配置关闭时直接拒绝（而不是让请求打到错误的上游域名）。
	if !core.RealmEnabled(a.Config().EnabledRealms, vendor.Region()) {
		return prepared, core.NewPluginError("vendor_disabled",
			fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
	}
	return prepared, nil
}

// handleExecutorCountTokens 估算 token 数。
//
// 上游没有 token 计数接口，因此这是**保守估算**：宁可高估也不能低估，
// 因为客户端用它做上下文管理——低估会导致客户端把超长上下文发上来被上游拒绝。
// 响应里显式标注 estimated，避免被当成上游精确值。
func (a *App) handleExecutorCountTokens(request []byte) ([]byte, error) {
	var rpc executorRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	estimate := core.EstimateTokens(append(append([]byte(nil), rpc.OriginalRequest...), rpc.Payload...))
	payload, errMarshal := json.Marshal(map[string]any{
		"total_tokens":  estimate,
		"input_tokens":  estimate,
		"output_tokens": 0,
		"estimated":     true,
	})
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal token estimate: %w", errMarshal)
	}
	return core.OKEnvelope(pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{jsonContentType}},
	})
}

// handleExecutorHTTPRequest 让宿主把一次出站 HTTP 交给插件执行。
//
// 本插件不需要这个能力（所有出站都由插件主动发起并经宿主的 HTTP 桥），
// 但既然声明了 executor 就必须实现。返回"不支持"比返回错误的成功更诚实。
func (a *App) handleExecutorHTTPRequest(request []byte) ([]byte, error) {
	return nil, core.NewPluginError("unsupported_operation",
		"this plugin does not proxy executor HTTP requests; outbound calls go through the host HTTP bridge",
		http.StatusNotImplemented)
}

// emitStreamChunk 把一段载荷推给宿主流。
func emitStreamChunk(callbackID, streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("stream id is missing")
	}
	_, errCall := hostbridge.Call(callbackID, pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	if errCall != nil {
		logger.Debug("stream emit failed: %v", errCall)
	}
	return errCall
}

// closeStream 关闭宿主流；failure 非空时由宿主转为错误帧。
func closeStream(callbackID, streamID, failure string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	payload := map[string]any{"stream_id": streamID}
	if failure != "" {
		payload["error"] = failure
	}
	if _, errCall := hostbridge.Call(callbackID, pluginabi.MethodHostStreamClose, payload); errCall != nil {
		logger.Debug("stream close failed: %v", errCall)
	}
}

// streamFailureMessage 把错误转成给下游看的消息。
//
// 上游的原始错误信息比我们重写的更有诊断价值，因此优先透传。
func streamFailureMessage(err error) string {
	if err == nil {
		return ""
	}
	var detail core.UpstreamDetail
	if errors.As(err, &detail) {
		if message := strings.TrimSpace(detail.UpstreamMessage()); message != "" {
			return message
		}
	}
	return err.Error()
}

// handleQuotaDescribe 描述本插件的额度能力。
func (a *App) handleQuotaDescribe(request []byte) ([]byte, error) {
	return core.OKEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerKey},
		DisplayName:        pluginDisplayName,
		// 本插件的「重置」是执行一次签到（签到会恢复额度），因此支持。
		SupportsReset: true,
	})
}

// handleQuotaFetch 查询某个账号的额度。
//
// 取数交给凭证所属的供应商：各家的额度接口与归一方式完全不同
// （WorkBuddy 是积分余额，Qoder 是套餐用量桶）。
func (a *App) handleQuotaFetch(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaFetchRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), "")
	// 宿主的额度路由**不发 StorageJSON**（只发 AuthIndex/AuthID + 属性），
	// 因此这里必须能按 index 回源，否则会误报「凭证缺失」。
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, "", rpc.StorageJSON,
		core.FirstNonEmpty(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errResolve != nil {
		return nil, errResolve
	}

	provider, okQuota := core.As[core.QuotaProvider](vendor)
	if !okQuota {
		return nil, core.QuotaUnsupported(vendor)
	}
	response, errQuota := provider.Quota(ctx, credential)
	if errQuota != nil {
		return nil, core.ToPluginError(errQuota)
	}
	return core.OKEnvelope(response)
}

// handleQuotaReset 执行一次签到以恢复额度。
//
// 语义由供应商决定：WorkBuddy 的「重置」是签到（CN 签到 / 国际版每日
// 活跃奖励）；不支持签到的供应商返回明确的不可用说明，而不是假装成功。
func (a *App) handleQuotaReset(request []byte) ([]byte, error) {
	var rpc pluginapi.QuotaResetRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx := hostbridge.WithCallbackID(context.Background(), "")
	credential, vendor, errResolve := a.resolveVendorCredential(ctx, "", rpc.StorageJSON,
		core.FirstNonEmpty(rpc.AuthIndex, rpc.AuthID), rpc.Attributes)
	if errResolve != nil {
		return nil, errResolve
	}
	if !core.SupportsCheckinFor(vendor, credential) {
		// 供应商级能力为真但账号级为假时（WorkBuddy 企业账号），
		// 用供应商给出的原因说明，避免把「不支持签到」显示成失败。
		message := core.FirstNonEmpty(core.CheckinUnsupportedReason(vendor, credential),
			"本供应商不提供签到；额度按上游周期自动重置")
		return core.OKEnvelope(pluginapi.QuotaResetResponse{
			Success: false,
			Message: message,
		})
	}
	result, errCheckin := a.tasks.Checkin(ctx, vendor, credential, tasks.CheckinTriggerManual)
	if errCheckin != nil {
		return nil, core.ToPluginError(errCheckin)
	}
	// NotRecorded 表示这次签到没有真正完成（上游窗口未开启、领取被拒、
	// 未确认发放）。此时不能报成功——否则宿主与控制台都会显示「已重置」
	// 而额度并没有因为签到而恢复。
	if result.NotRecorded {
		return core.OKEnvelope(pluginapi.QuotaResetResponse{Success: false, Message: result.Message})
	}
	return core.OKEnvelope(pluginapi.QuotaResetResponse{Success: true, Message: result.Message})
}
