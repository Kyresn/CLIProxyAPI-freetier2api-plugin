package zcode

import (
	"context"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// 本文件把 ZCode 适配成 core.Vendor。
//
// 凭证形态是 oauth：账号由控制台发起 OAuth 登录产生（一次拿到 Coding Plan
// JWT 与兑换出的 API Key）。之所以声明为 oauth 而不是 apikey，是因为页面按
// auth_mode 决定「添加账号」的动作——apikey 会直接弹输入框、不发起登录。
//
// 通道选择：具备 JWT 时优先走 Start Plan 通道（zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages）；
// 纯 API Key 账号走 ZCode 官方网关转发通道（zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages）。

// adapter 是 core.Vendor 在 ZCode 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	// vendorID 是实例标识（国内版 zcodecn / 国际版 zcodeglobal）。
	vendorID string
	// region / provider 固定本实例的登录来源。
	region   string
	provider string
	baseURL  string
	// claims 是本实例的领取节流器；不共享，多 App 并存时互不影响。
	claims *claimGate
	// activations 是本实例的活跃上报去重表（每账号每日一次）。
	activations *activationState
	// gate 是本实例的 Start Plan 出站闸门；不共享，多 App 并存时互不影响。
	gate *chatGate
}

// Instances 构造 ZCode 国内 / 国际两个实例。
//
// 两者协议与上游通道相同，区别只在登录 provider 与凭证归属：
//   - 国内版 zcodecn：bigmodel（智谱开放平台）
//   - 国际版 zcodeglobal：zai（z.ai）
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{
		newInstance(env, VendorIDCN, VendorNameCN, RegionCN, ProviderBigmodel),
		newInstance(env, VendorIDGlobal, VendorNameGlobal, RegionGlobal, ProviderZai),
	}
}

func newInstance(env core.Env, vendorID, vendorName, region, provider string) *adapter {
	instance := &adapter{
		vendorID:    vendorID,
		region:      region,
		provider:    provider,
		claims:      newClaimGate(),
		activations: newActivationState(),
		gate:        newChatGate(),
	}
	instance.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: vendorID, VendorName: vendorName, VendorRegion: region},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			// 旧凭证（vendor=zcode、无 region）按本实例补齐并迁移到新 ID。
			native.Region = core.FirstNonEmpty(native.Region, region)
			native.Provider = core.FirstNonEmpty(native.Provider, provider)
			native.Vendor = vendorID
			label := native.Label
			if label == "" {
				label = core.FirstNonEmpty(native.Email, "ZCode-"+MaskedKey(native.OutboundToken()))
			}
			return core.CredentialSeed{
				VendorID: vendorID, Region: native.Region, Label: label,
				FileID: core.FileIDFromFileName(fileName), UID: core.FirstNonEmpty(native.UserID, native.Email),
				Token: native.APIKey, AuthMode: native.AuthMode,
			}, native, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})
	return instance
}

// AuthMode 返回凭证形态：oauth。
func (v *adapter) AuthMode() string { return "oauth" }

// Configure 解析供应商专属配置。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	// 实例级键优先；族级键保留为旧配置的兼容回退。
	v.baseURL = core.FirstNonEmpty(
		strings.TrimSpace(raw[v.vendorID+"_base_url"]),
		strings.TrimSpace(raw["zcode_base_url"]),
	)
	intervalText := core.FirstNonEmpty(
		strings.TrimSpace(raw[v.vendorID+"_claim_min_interval"]),
		strings.TrimSpace(raw["zcode_claim_min_interval"]),
	)
	if interval, errParse := time.ParseDuration(intervalText); errParse == nil && interval >= 0 {
		v.claims.setInterval(interval)
	}
}

// Match 判断一份凭证是否属于本实例。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider, v.region)
}

// ---- 模型 ----

// StaticModels 返回模型清单。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      int64(model.ContextWindow),
			MaxOutputTokens:    int64(model.MaxTokens),
			SupportsTools:      model.SupportsTools,
			SupportsImages:     model.SupportsImages,
			CanDisableThinking: !model.IsReasoning,
		}, true
	})
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	release, errGate := v.holdStartPlanSlot(ctx, native)
	if errGate != nil {
		return nil, errGate
	}
	defer release()

	payload, errChat := Chat(ctx, v.baseURL, ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     false,
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	release, errGate := v.holdStartPlanSlot(ctx, native)
	if errGate != nil {
		return errGate
	}
	defer release()

	return ChatStream(ctx, v.baseURL, ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Stream:     true,
	}, func(chunk []byte) error {
		return sink.Emit(chunk)
	})
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return v.fetchQuota(ctx, native)
}

// ---- 登录与续期 ----

// LoginStart 发起一次登录（本实例固定的国内 / 国际 provider）。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins, v.vendorID, v.provider)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state)
}

// LegacyVendorIDs 让旧 `zcode:*` 配置继续命中两个新实例。
func (v *adapter) LegacyVendorIDs() []string { return []string{VendorID} }

// ConfigFields 声明 zcode 专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: v.vendorID + "_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "本实例上游基地址覆盖（旧键 zcode_base_url 仍兼容）。"},
		{Name: v.vendorID + "_claim_min_interval", Type: pluginapi.ConfigFieldTypeString, Description: "本实例两次套餐领取之间的最小间隔（Go 时长，如 15s；0 表示不节流）。"},
	}
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.AuthModeReporter     = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.VendorAliasProvider  = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.ActivityProbe        = (*adapter)(nil)
	_ core.PlanClaimer          = (*adapter)(nil)
)
