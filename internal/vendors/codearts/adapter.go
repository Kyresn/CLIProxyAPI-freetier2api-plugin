package codearts

// 本文件把 CodeArts 适配成 core.Vendor。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// adapter 是 core.Vendor 在 CodeArts 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]

	// credits 缓存最近一轮目录探测的倍率表（官方模型 ID → 展示文案）。
	credits core.CreditTable

	// modelsByLower 是小写模型名 → 注册 ID 的索引，用于把请求里的旧写法
	// 归一成目录里的正式 ID。
	modelsMu      sync.RWMutex
	modelsByLower map[string]string
}

// Instances 构造 CodeArts 实例（单一部署，国内区）。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{&adapter{NativeAdapter: core.NewNativeAdapter(core.NativeSpec[*Credential]{
		Meta: core.Meta{VendorID: VendorID, VendorName: VendorName, VendorRegion: "cn"},
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
			label := native.Label
			if label == "" {
				if native.UserName != "" {
					label = "CodeArts-" + native.UserName
				} else if native.UserID != "" {
					label = "CodeArts-" + native.UserID
				} else {
					label = "CodeArts-Account"
				}
			}
			return core.CredentialSeed{
				VendorID: VendorID, Region: "cn", Label: label,
				FileID: core.FileIDFromFileName(fileName), Token: native.AccessKeyID,
				ExpiresAt: native.ExpiresAt, AuthMode: "oauth",
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			updated, refreshed, errRefresh := RefreshCredential(ctx, native)
			if errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			return core.NativeRefresh[*Credential]{
				Native: updated, Token: updated.AccessKeyID, RefreshToken: updated.RefreshToken,
				ExpiresAt: updated.ExpiresAt, Changed: refreshed,
			}, nil
		},
		MergeNative: func(original []byte, native *Credential) ([]byte, error) {
			return MergeStorageJSON(original, native)
		},
	})}}
}

// Match 判断一份凭证是否属于本供应商。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	return LooksLikeCredential(raw, fileName, provider)
}

// ---- 模型 ----

// StaticModels 返回模型清单（无需凭证）。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	models, errModels := FetchModels(ctx, nil)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// ModelsForAuth 返回该凭证可用的模型。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	native, _ := v.Native(cred)
	models, errModels := FetchModels(ctx, native)
	if errModels != nil {
		return nil, errModels
	}
	return v.modelsToPluginAPI(models), nil
}

// modelsToPluginAPI 把上游模型条目转成宿主契约类型，顺带记下倍率表。
//
// 倍率与清单必须出自同一轮探测：分开取会出现「模型换了一批、倍率还是上一批」
// 的错配。CodeArts 的倍率来自内置目录 credit[].ratio_display，福利通道没有
// 倍率字段，因此福利模型不进表（控制台显示「—」，不臆造成免费）。
func (v *adapter) modelsToPluginAPI(models []Model) []pluginapi.ModelInfo {
	core.FillCredits(&v.credits, models,
		func(model Model) string { return model.ID },
		func(model Model) string { return model.Credits })
	ids := make(map[string]string, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids[strings.ToLower(id)] = id
		}
	}
	v.modelsMu.Lock()
	v.modelsByLower = ids
	v.modelsMu.Unlock()
	return core.ConvertModels(models, func(model Model) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:              model.ID,
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   int64(model.ContextWindow),
			MaxOutputTokens: int64(model.MaxTokens),
			SupportsTools:   model.SupportsTools,
			SupportsImages:  model.SupportsImages,
		}, true
	})
}

// prepareRequestBody 在出站前做两处最小改写：
//
//  1. model 归一成注册时的 ID：宿主按 ID 精确匹配，客户端却可能沿用旧文档/
//     旧别名的大小写写法（例如小写 glm-5.2 对应注册的 GLM-5.2），上游对模型名
//     大小写敏感，原样出站会被拒；
//  2. max_tokens / max_completion_tokens 夹到上游硬顶（见 clampTokenFields）。
//
// 两处共用一次解析与一次序列化：各做一遍会让每个对话请求多两次 JSON 往返，
// 而且第二遍是在第一遍的产物上再解析，纯属浪费。
func (v *adapter) prepareRequestBody(body []byte) []byte {
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return body
	}
	changed := v.canonicalizeModelField(payload)
	if clampTokenFields(payload) {
		changed = true
	}
	if !changed {
		return body
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return body
	}
	return encoded
}

// canonicalizeModelField 就地改写 model 字段，返回是否真的改了。
// 未注册的名字保持原样，交给上游判定。
func (v *adapter) canonicalizeModelField(payload map[string]json.RawMessage) bool {
	raw, okRaw := payload["model"]
	if !okRaw {
		return false
	}
	var name string
	if errUnmarshal := json.Unmarshal(raw, &name); errUnmarshal != nil {
		return false
	}
	canonical := v.canonicalModelName(name)
	if canonical == name {
		return false
	}
	encoded, errMarshal := json.Marshal(canonical)
	if errMarshal != nil {
		return false
	}
	payload["model"] = encoded
	return true
}

// canonicalModelName 用最新一轮目录把名字映射回注册 ID；未注册返回原名。
func (v *adapter) canonicalModelName(name string) string {
	v.modelsMu.RLock()
	defer v.modelsMu.RUnlock()
	if canonical, ok := v.modelsByLower[strings.ToLower(strings.TrimSpace(name))]; ok {
		return canonical
	}
	return name
}

// ModelCredits 实现 core.CreditsProvider：返回「官方模型 ID → 计费倍率」。
func (v *adapter) ModelCredits() map[string]string {
	return v.credits.Snapshot()
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	payload, errChat := Chat(ctx, "", ChatRequest{
		Credential: native,
		Body:       v.prepareRequestBody(req.Payload),
		Stream:     false,
		Benefit:    true,
	})
	if errChat != nil {
		return nil, core.ToPluginError(errChat)
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
	resp, errStream := ChatStream(ctx, "", ChatRequest{
		Credential: native,
		Body:       v.prepareRequestBody(req.Payload),
		Stream:     true,
		Benefit:    true,
	})
	if errStream != nil {
		return core.ToPluginError(errStream)
	}
	defer func() { _ = resp.Body.Close() }()
	return hostbridge.ForwardRawSSE(resp.Body, sink.Emit)
}

// ---- 额度与签到 ----

// Quota 查询账号额度。
func (v *adapter) Quota(ctx context.Context, cred *core.Credential) (*pluginapi.QuotaFetchResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	return FetchQuota(ctx, native)
}

// SupportsCheckin 报告本供应商是否提供签到。
func (v *adapter) SupportsCheckin() bool { return SupportsCheckin() }

// Checkin 执行一次签到。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	result, errClaim := Checkin(ctx, native)
	if errClaim != nil {
		return nil, core.ToPluginError(errClaim)
	}
	return &core.CheckinResult{
		Already: result.Already,
		Message: result.Message,
	}, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	return LoginStart(ctx, v.Env.Logins)
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	return LoginPoll(ctx, v.Env.Logins, state)
}

// LocalCallbackHint 返回控制台要展示的本机回调地址。
//
// 华为云授权成功后浏览器停在这个地址上：插件与浏览器不在同一台机器时收不到，
// 用户把整条链接粘回控制台即可继续登录。
func (v *adapter) LocalCallbackHint() string { return LocalCallbackURL }

// SubmitCallback 解析用户粘贴的 localhost 回调链接并完成登录。
func (v *adapter) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	return SubmitCallback(ctx, v.Env.Logins, state, rawURL)
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor            = (*adapter)(nil)
	_ core.CallbackSubmitter = (*adapter)(nil)
	_ core.CheckinProvider   = (*adapter)(nil)
	_ core.LoginProvider     = (*adapter)(nil)
	_ core.ModelsProvider    = (*adapter)(nil)
	_ core.QuotaProvider     = (*adapter)(nil)
	_ core.CreditsProvider   = (*adapter)(nil)
)
