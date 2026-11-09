package core

import (
	"context"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// Package core 是本插件的供应商无关骨架：共享类型、供应商接口与注册表。
//
// 分层纪律：
//   - core 不 import 任何供应商包（internal/vendors/*），也不 import package main；
//   - 供应商特有逻辑（端点、请求头、指纹、模型目录、签到流程）必须实现在
//     internal/vendors/<name> 里，通过 Vendor 接口被根层调用；
//   - 根层（package main）是 ABI 适配层，负责把宿主 RPC 翻译成 Vendor 调用。
//
// 为什么需要这一层：宿主 ABI 限制一个插件进程只能声明一个 provider key
// （见 ProviderKey 的说明），因此多个供应商必须共存于一个插件内。若没有
// 这层抽象，每加一个供应商都要在根层的能力文件里改一处 switch。

// ProviderKey 是本插件在 CPA 里占用的 provider 键，**所有供应商共用**。
//
// 宿主的 auth.identifier 只返回单个字符串、注册期调一次永久缓存，且与
// auth 文件的 type 字段严格相等才认领该文件（internal/pluginhost/auth_provider.go
// 的 authProviderRecord）。ModelsForAuth 也用同一个值匹配 auth.Provider，
// 不匹配即跳过（该账号不可用）。
//
// 因此供应商的区分落在**凭证文件名前缀**（workbuddycn-<uid>.json）与文件内的
// vendor 字段上，而不是 provider key。
const ProviderKey = "freetier"

// VendorKey 是凭证内标识供应商的字段名。
//
// 与文件名前缀冗余是刻意的：文件名让用户一眼看出归属，vendor 字段让判定
// 不依赖文件命名（用户手工改名后仍能正确归属）。
const VendorKey = "vendor"

// FormatChatCompletions 是本插件唯一声明支持的协议格式。
//
// 所有供应商统一只处理 chat-completions：Claude / Codex / Gemini 客户端由
// 宿主翻译成 chat-completions 再交给插件。插件重复实现协议转换会在两侧
// 产生不一致的分帧行为，且两套代码都要跟着上游协议变化维护。
const FormatChatCompletions = "chat-completions"

// MaxAccountsPerVendor 是单个供应商允许添加的最大账号数。
const MaxAccountsPerVendor = 10

// Vendor 是供应商实例的**必需**契约（= 供应商协议 × 区域）。
//
// 只放每家都必然具备的能力：身份、凭证归属/解析、模型清单、对话执行。
// 其余能力（登录、续期、额度、签到、任务、配置、存储合并、页面模型明细）
// 都是可选小接口（见 capabilities.go），由调用方按需类型断言——
// Go 没有 Java 的 default 方法，不支持的供应商直接不实现，不写空方法。
//
// 约定：实现必须无状态且并发安全——宿主会并发调用这些方法。
type Vendor interface {
	// ID 是供应商实例标识，同时是凭证文件名前缀（workbuddycn 等）。
	ID() string
	// Name 是管理端展示名（如「WorkBuddy 国内版」）。
	Name() string
	// Region 是区域标识：cn / global。
	Region() string
	// ModelPrefix 是注册模型时的 ID 前缀，空表示用裸名。
	ModelPrefix() string

	// Match 判断一份凭证文件是否属于本实例。
	//
	// **必须偏严**：宿主会把所有非内建格式的凭证依次喂给插件，误吞别家的
	// 会让宿主用本插件的结构覆盖对方账号（且是静默的）。
	Match(fileName, provider string, raw map[string]any) bool
	// Parse 把凭证 JSON 解析成归一化凭证。
	Parse(raw []byte, fileName string) (*Credential, error)

	// StaticModels 返回本供应商的全部模型（供 /v1/models 展示）。
	StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error)

	// Execute 执行一次非流式对话。
	Execute(ctx context.Context, cred *Credential, req *ExecuteRequest) (*pluginapi.ExecutorResponse, error)
	// ExecuteStream 执行一次流式对话，分片经 sink 投递给宿主。
	//
	// 实现只产出上游的原生分片内容（裸 SSE data 载荷），不做 `data: ` 包装
	// 与多协议分帧——那些由宿主按声明格式完成。
	ExecuteStream(ctx context.Context, cred *Credential, req *ExecuteRequest, sink StreamSink) error
}

// CredentialOwnershipValidator checks host provider identity before registry
// shortcuts can bypass Vendor.Match.
type CredentialOwnershipValidator interface {
	ValidateCredentialOwnership(provider string, raw map[string]any) bool
}

// ExecuteRequest 是一次对话执行的输入。
type ExecuteRequest struct {
	// Model 是剥掉供应商前缀后的裸模型名。
	Model string
	// Payload 是宿主翻译好的 chat-completions 请求体。
	Payload []byte
	// Headers 是客户端请求头。
	Headers map[string][]string
	// ClientIP 是客户端 IP（未开启透传时为空）。
	ClientIP string
	// Stream 报告是否流式。
	Stream bool
}

// StreamSink 是插件向宿主投递流式分片的出口（由根层实现）。
type StreamSink interface {
	// Emit 投递一个分片（裸 SSE 数据载荷，不含 `data: ` 前缀）。
	Emit(payload []byte) error
	// Close 结束流。errMsg 非空时作为错误帧投递给客户端。
	Close(errMsg string) error
}

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Already 报告本次是否为重复签到（幂等命中）。
	Already bool
	// Credit / Energy 是本次所得。
	Credit int64
	Energy int64
	// Message 是可读说明。
	Message string
	// NotRecorded 报告本次结果**不应**被记入签到历史。
	//
	// 它是 opt-out 字段（零值 false = 正常记录），因为 CheckinResult 普遍以
	// 零值结构体构造：若用「Recorded bool」正向声明，所有未显式赋值的现有
	// 供应商都会变成「不记录」，那是一次静默的行为回归。
	//
	// 供应商只在「本次什么都没做成」时置位——例如上游领取窗口尚未开启、
	// 或领取后未能确认发放。把这类情况也记成「今日已签到」会让页面显示
	// 假性成功，而上游窗口错过不补领，积分就此永久丢失。
	NotRecorded bool
}

// 本文件定义供应商运行时环境与组合根工厂。
//
// Env 是供应商唯一允许依赖的宿主面：host 端口、注册表、登录会话表。
// 供应商不再读取包级全局注册表或全局 host port。

// VendorFactory 负责构造一个供应商家族的实例（区域变体在 family 内展开）。
type VendorFactory struct {
	Instances func(Env) []Vendor
}

// Env 是供应商实例的运行时依赖。
type Env struct {
	Host     Host
	Registry *Registry
	Logins   LoginSessions
}

// Settings 返回当前全局设置快照。
func (e Env) Settings() Settings {
	if e.Host == nil {
		return Settings{}
	}
	return e.Host.Settings()
}

// PollState 是登录轮询的准入结果。
type PollState int

const (
	PollMissing PollState = iota
	PollThrottled
	PollReady
)

// LoginSessions 是登录会话登记表接口；实现由 internal/auth.SessionRegistry 提供。
type LoginSession struct {
	VendorID string
	Data     any
}

type LoginSessions interface {
	Register(vendorID, sessionID string, ttl, interval time.Duration, data any)
	Poll(sessionID string) (*LoginSession, PollState)
	Owns(sessionID string) (vendorID string, ok bool)
	Delete(sessionID string)
}

// ProbeNativeCredential 从宿主凭证中挑第一个属于 vendorID 的可用原生凭证。
func ProbeNativeCredential[T any](env Env, vendorID string, extract NativeExtractor[T]) (*T, bool) {
	if env.Host == nil || env.Registry == nil {
		return nil, false
	}
	ctx := context.Background()
	entries, errList := env.Host.ListAuths(ctx)
	if errList != nil {
		return nil, false
	}
	for _, entry := range entries {
		if !entry.Usable() {
			continue
		}
		raw, okRaw := env.Host.AuthJSON(ctx, entry.AuthIndex)
		if !okRaw {
			continue
		}
		credential, okParse := env.Registry.ParseCredential(raw, entry.Name, nil)
		if !okParse || credential.VendorIDValue() != vendorID {
			continue
		}
		if native, okNative := extract(credential); okNative && native != nil {
			return native, true
		}
	}
	return nil, false
}
