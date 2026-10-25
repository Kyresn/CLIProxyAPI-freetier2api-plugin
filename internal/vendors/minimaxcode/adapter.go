package minimaxcode

// 本文件把 MiniMax Code（国内版/国际版）适配成 core.Vendor。

import (
	"context"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// adapter 是 core.Vendor 在 MiniMax Code 上的实现（每个区域一个实例）。
type adapter struct {
	*core.NativeAdapter[*Credential]
	region  Region
	baseURL string
	// workspace 是账号工作区模型目录缓存。挂在实例上而不是包级变量：
	// 同进程的多个 App 实例各自独立，缓存生命周期跟着实例走。
	workspace workspaceCache
}

// Instances 构造两个区域实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	inst := &adapter{region: region}
	inst.NativeAdapter = core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorIDFor(region), VendorName: VendorNameFor(region), VendorRegion: string(region)},
		Env:  env,
		Extract: func(c *core.Credential) (*Credential, bool) {
			native, ok := c.Native.(*Credential)
			return native, ok
		},
		ParseSeed: func(raw []byte, fileName string) (core.CredentialSeed, *Credential, error) {
			native, errParse := ParseCredential(raw, region)
			if errParse != nil {
				return core.CredentialSeed{}, nil, errParse
			}
			label := native.Label
			if label == "" {
				if native.Email != "" {
					label = native.Email
				} else if native.UserID != "" {
					label = VendorNameFor(region) + "-" + native.UserID
				} else {
					label = fileName
				}
			}
			uid := native.UserID
			if uid == "" {
				uid = core.FirstNonEmpty(native.Email, fileName)
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(region), Label: label,
				FileID: core.FileIDFromFileName(fileName), UID: uid,
				Token: native.AccessToken, RefreshToken: native.RefreshToken,
				ExpiresAt: native.ExpiresAt, AuthMode: native.AuthMode,
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			updated, refreshed, errRefresh := RefreshCredential(ctx, native, inst.baseURL)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			return core.NativeRefresh[*Credential]{
				Native: updated, Token: updated.AccessToken, RefreshToken: updated.RefreshToken,
				ExpiresAt: updated.ExpiresAt, Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})
	return inst
}

// Configure 解析供应商专属配置。
func (v *adapter) Configure(_ core.Settings, raw map[string]string) {
	if v.region == RegionGlobal {
		v.baseURL = strings.TrimSpace(raw["minimaxcode_global_base_url"])
		return
	}
	v.baseURL = strings.TrimSpace(raw["minimaxcode_cn_base_url"])
}

// Match 判断一份凭证是否属于本区域。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider, v.region)
}

// ---- 模型 ----

// ModelsForAuth 返回本区域「内置目录 ∪ 账号工作区」的模型清单。
//
// 内置目录是注册表骨架：它带完整的上下文/输出/工具元数据，以及插件侧
// 构造的 -thinking 变体 ID。工作区 /config 是上游当前可用模型的权威来源，
// 但只用来补充内置目录里没有的新模型——整体替换会让既有 ID（尤其
// -thinking）与元数据消失，造成兼容性回归。
//
// 拉取失败时只用内置目录：宁可给旧 ID 也不给空清单（空清单会让宿主
// 淘汰凭证，而旧 ID 至多在对话时报错）。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	static, errStatic := v.StaticModels(ctx)
	if errStatic != nil {
		return nil, errStatic
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return static, nil
	}
	return mergeWorkspaceModels(static, v.workspace.models(ctx, native, v.baseURL)), nil
}

// StaticModels 返回本区域的模型清单。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, v.region)
	if errModels != nil {
		return nil, errModels
	}
	return modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型。
func modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        model.Description,
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxOutput,
			SupportsTools:      model.SupportsTools,
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
	payload, errChat := chat(ctx, native, req, v.baseURL, &v.workspace)
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
	errStream := chatStream(ctx, native, req, v.baseURL, func(chunk []byte) error {
		return sink.Emit(chunk)
	}, &v.workspace)
	if errStream != nil {
		return errStream
	}
	return nil
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return FetchQuota(ctx, native, v.baseURL)
}

// Checkin 执行一次签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return Checkin(ctx, native, v.baseURL)
}

// ---- 登录与续期 ----

// LoginStart 发起一次登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins, v.region)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state, v.baseURL)
}

func (v *adapter) SupportsCheckin() bool { return v.region == RegionGlobal }

// ConfigFields 声明 MiniMax Code 专属配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "minimaxcode_cn_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "国内版 Agent 域基地址覆盖（Token Plan 窗口查询固定直连开放平台域）。"},
		{Name: "minimaxcode_global_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "国际版 Agent 域基地址覆盖（Token Plan 窗口查询固定直连开放平台域）。"},
	}
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.CheckinProvider      = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
)
