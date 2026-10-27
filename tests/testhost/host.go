package testhost

// Package testhost 提供宿主回调的内存实现（Fake Host）与上游桩。
//
// 目的：让 root 与各 vendor 的测试共享同一套宿主 RPC 行为，而不是每个包
// 各写一份「只实现自己需要的方法」的桩。它实现的是 host.* 的全部常用方法：
// 出站 HTTP、流式读取/关闭、凭证列表/读取、流分片投递。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
)

// UpstreamResponse 是假宿主对一次出站请求的应答。
type UpstreamResponse struct {
	Status int
	Header map[string][]string
	Body   string
}

// Stream 是一次被模拟的流式响应。
type Stream struct {
	chunks   []string
	position int
	closed   bool
}

// Host 是宿主回调的内存实现（实现 hostbridge.Caller 的签名）。
type Host struct {
	mu sync.Mutex

	// calls 记录被调用的宿主方法名（供断言）。
	calls []string
	// authFiles 是 host.auth.list 的返回内容。
	authFiles []core.HostAuth
	// authJSON 是 auth_index → 凭证 JSON。
	authJSON map[string]string
	// authPaths 是 auth_index → 宿主返回的凭证文件路径（host.auth.get 的 path）。
	// 真实宿主的 path 不保证绝对：以相对 auth-dir 启动的宿主会返回相对路径。
	authPaths map[string]string
	// upstream 是出站请求的应答函数（按 method + url 匹配）。
	upstream func(method, url, body string) UpstreamResponse
	// upstreamCalls 记录出站请求次数（供 singleflight 类断言）。
	upstreamCalls int

	streams    map[string]*Stream
	nextStream int

	// 流式转发的记录。
	emits     []string
	streamErr []string
	closed    []string
}

// New 构造一个 Fake Host。
func New() *Host {
	return &Host{
		authJSON:  map[string]string{},
		authPaths: map[string]string{},
		streams:   map[string]*Stream{},
		upstream: func(string, string, string) UpstreamResponse {
			return UpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404,"msg":"not stubbed"}`}
		},
	}
}

// Call 实现 hostbridge.Caller。
func (h *Host) Call(callbackID, method string, payload any) (json.RawMessage, error) {
	h.mu.Lock()
	h.calls = append(h.calls, method)
	h.mu.Unlock()

	switch method {
	case pluginabi.MethodHostHTTPDo:
		return h.handleHTTPDo(payload)
	case pluginabi.MethodHostHTTPDoStream:
		return h.handleHTTPDoStream(payload)
	case pluginabi.MethodHostHTTPStreamRead:
		return h.handleStreamRead(payload)
	case pluginabi.MethodHostHTTPStreamClose:
		return h.handleStreamClose(payload)
	case pluginabi.MethodHostAuthList:
		return marshalJSON(map[string]any{"auths": h.SnapshotAuthFiles()})
	case pluginabi.MethodHostAuthGet:
		return h.handleAuthGet(payload)
	case pluginabi.MethodHostStreamEmit:
		return h.handleStreamEmit(payload)
	case pluginabi.MethodHostStreamClose:
		return h.handleStreamClosePayload(payload)
	}
	return nil, fmt.Errorf("fake host does not implement %s", method)
}

// SnapshotAuthFiles 返回当前注册的凭证（副本）。
func (h *Host) SnapshotAuthFiles() []core.HostAuth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]core.HostAuth, len(h.authFiles))
	copy(out, h.authFiles)
	return out
}

// AddAccount 注册一个假账号。
func (h *Host) AddAccount(authIndex, name, credentialJSON string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authJSON[authIndex] = credentialJSON
	h.authFiles = append(h.authFiles, core.HostAuth{
		ID: name, AuthIndex: authIndex, Name: name, Provider: core.ProviderKey, Label: name,
	})
}

// SetAccountCredential 覆盖某个账号的凭证 JSON，账号身份（文件名）不变。
//
// 用于模拟 token 刷新 / 重登后宿主重写同一个凭证文件：内容变了，身份没变。
func (h *Host) SetAccountCredential(authIndex, credentialJSON string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authJSON[authIndex] = credentialJSON
}

// SetAuthFilePath 让 host.auth.get 返回凭证的物理文件路径。
//
// 真实宿主的 path 不保证绝对：以相对 auth-dir 启动的宿主（EasyCLIProxyAPI
// 默认就是 ../oauth）会把 WalkDir 得到的相对路径原样返回。这里也原样透传，
// 供删除等依赖路径的功能复现该形态。
func (h *Host) SetAuthFilePath(authIndex, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authPaths[authIndex] = path
}

// MarkUnavailable 模拟宿主路由层给凭证打上「临时不可用」（上游限流/超时后的
// 冷却）。它与用户显式禁用不同：凭证仍应参与模型探测与额度查询。
func (h *Host) MarkUnavailable(name string, unavailable bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := range h.authFiles {
		if h.authFiles[index].Name == name || h.authFiles[index].ID == name {
			h.authFiles[index].Unavailable = unavailable
		}
	}
}

// MarkDisabled 模拟用户显式禁用某凭证。
func (h *Host) MarkDisabled(name string, disabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index := range h.authFiles {
		if h.authFiles[index].Name == name || h.authFiles[index].ID == name {
			h.authFiles[index].Disabled = disabled
		}
	}
}

// SetUpstream 设置出站请求的应答。
func (h *Host) SetUpstream(fn func(method, url, body string) UpstreamResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.upstream = fn
}

// UpstreamCalls 返回已发生的出站请求次数。
func (h *Host) UpstreamCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.upstreamCalls
}

// doUpstream 计一次出站请求并取应答。
func (h *Host) doUpstream(method, url, body string) UpstreamResponse {
	h.mu.Lock()
	h.upstreamCalls++
	fn := h.upstream
	h.mu.Unlock()
	return fn(method, url, body)
}

// EmittedChunks 返回已投递的流分片。
func (h *Host) EmittedChunks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.emits...)
}

// StreamErrors 返回流关闭时上报的错误。
func (h *Host) StreamErrors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.streamErr...)
}

// ClosedStreams 返回被关闭的流 ID 列表。
func (h *Host) ClosedStreams() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.closed...)
}

func (h *Host) handleHTTPDo(payload any) (json.RawMessage, error) {
	req := decodeRequest(payload)
	response := h.doUpstream(req.method, req.url, string(req.body))
	// 与真实宿主同形：host.http.do 回的是**无 json tag** 的 pluginapi.HTTPResponse。
	return marshalJSON(map[string]any{
		"StatusCode": response.Status,
		"Headers":    response.Header,
		"Body":       []byte(response.Body),
	})
}

func (h *Host) handleHTTPDoStream(payload any) (json.RawMessage, error) {
	req := decodeRequest(payload)
	response := h.doUpstream(req.method, req.url, string(req.body))

	h.mu.Lock()
	h.nextStream++
	streamID := fmt.Sprintf("stream-%d", h.nextStream)
	h.streams[streamID] = &Stream{chunks: splitChunks(response.Body)}
	h.mu.Unlock()

	return marshalJSON(map[string]any{
		"status_code": response.Status,
		"headers":     response.Header,
		"stream_id":   streamID,
	})
}

func (h *Host) handleStreamRead(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	stream, okStream := h.streams[req.StreamID]
	if !okStream {
		h.mu.Unlock()
		return marshalJSON(map[string]any{"done": true})
	}
	if stream.position >= len(stream.chunks) {
		delete(h.streams, req.StreamID)
		h.mu.Unlock()
		return marshalJSON(map[string]any{"done": true})
	}
	chunk := stream.chunks[stream.position]
	stream.position++
	h.mu.Unlock()
	return marshalJSON(map[string]any{"payload": []byte(chunk)})
}

func (h *Host) handleStreamClose(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	if stream, okStream := h.streams[req.StreamID]; okStream {
		stream.closed = true
	}
	delete(h.streams, req.StreamID)
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

func (h *Host) handleAuthGet(payload any) (json.RawMessage, error) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	raw, okRaw := h.authJSON[req.AuthIndex]
	path := h.authPaths[req.AuthIndex]
	h.mu.Unlock()
	if !okRaw {
		return json.RawMessage(`{"auth_index":"` + req.AuthIndex + `","json":null}`), nil
	}
	// 真实宿主的 HostAuthGetResponse.JSON 是 json.RawMessage：原样内联，
	// 不能走 []byte 序列化（那会变成 base64，插件侧解不出来）。
	response := struct {
		AuthIndex string          `json:"auth_index"`
		Path      string          `json:"path,omitempty"`
		JSON      json.RawMessage `json:"json"`
	}{AuthIndex: req.AuthIndex, Path: path, JSON: json.RawMessage(raw)}
	return marshalJSON(response)
}

func (h *Host) handleStreamEmit(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	h.emits = append(h.emits, string(req.Payload))
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

func (h *Host) handleStreamClosePayload(payload any) (json.RawMessage, error) {
	var req struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error"`
	}
	_ = json.Unmarshal(mustMarshal(payload), &req)

	h.mu.Lock()
	h.closed = append(h.closed, req.StreamID)
	if req.Error != "" {
		h.streamErr = append(h.streamErr, req.Error)
	}
	h.mu.Unlock()
	return marshalJSON(map[string]any{})
}

type request struct {
	method string
	url    string
	body   []byte
}

func decodeRequest(payload any) request {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(mustMarshal(payload), &fields)

	out := request{}
	if raw, okRaw := fields["method"]; okRaw {
		_ = json.Unmarshal(raw, &out.method)
	}
	if raw, okRaw := fields["url"]; okRaw {
		_ = json.Unmarshal(raw, &out.url)
	}
	if raw, okRaw := fields["body"]; okRaw {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}

func marshalJSON(value any) (json.RawMessage, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return raw, nil
}

// splitChunks 把响应体切成小块，模拟宿主 32KB 分片读取的真实路径。
//
// 刻意不一次性喂完整正文：那会掩盖分片边界相关的 bug。
func splitChunks(body string) []string {
	if body == "" {
		return nil
	}
	const size = 17
	chunks := make([]string, 0, len(body)/size+1)
	for len(body) > 0 {
		take := size
		if take > len(body) {
			take = len(body)
		}
		chunks = append(chunks, body[:take])
		body = body[take:]
	}
	return chunks
}
