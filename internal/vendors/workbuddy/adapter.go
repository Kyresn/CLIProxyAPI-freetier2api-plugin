package workbuddy

// 本文件把 WorkBuddy（CodeBuddy）协议适配成 core.Vendor。
//
// 供应商实例 = 协议 × 区域。CN 与 GLOBAL 是两套独立部署：域名、模型清单、
// reasoning 档位表、签到活动都不同，且凭证不通用（用 cn 的 token 打 global
// 域名会得到 401 TOKEN_EXPIRE）。区域在构造期固定，实例方法内不再分支。

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// adapter 是 core.Vendor 在 WorkBuddy 上的实现。
type adapter struct {
	*core.NativeAdapter[*Credential]
	region Region
	rt     *runtime
}

// Instances 构造 WorkBuddy 的两个区域实例。
func Instances(env core.Env) []core.Vendor {
	return []core.Vendor{newInstance(env, RegionCN), newInstance(env, RegionGlobal)}
}

func newInstance(env core.Env, region Region) core.Vendor {
	rt := newRuntime(env)
	inst := &adapter{rt: rt, region: region}
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
			if native.Realm() != region {
				return core.CredentialSeed{}, nil, fmt.Errorf("credential realm %q does not match vendor %q", native.Realm(), VendorIDFor(region))
			}
			token := native.AccessTokenValue()
			authMode := "oauth"
			if token == "" {
				token = native.DeviceTokenValue()
				authMode = "device"
			}
			return core.CredentialSeed{
				VendorID: VendorIDFor(region), Region: string(native.Realm()),
				Label:  core.FirstNonEmpty(native.NicknameValue(), native.UIDValue(), fileName),
				FileID: core.FileIDFromFileName(fileName), UID: native.UIDValue(),
				Token: token, RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAt, AuthMode: authMode,
			}, native, nil
		},
		RefreshNative: func(ctx context.Context, native *Credential) (core.NativeRefresh[*Credential], error) {
			client := NewClient(rt, rt.options(ctx))
			accessBefore, refreshBefore := native.Snapshot()
			if errRefresh := client.RefreshToken(native); errRefresh != nil {
				return core.NativeRefresh[*Credential]{}, errRefresh
			}
			refreshed := !native.UnchangedSince(accessBefore, refreshBefore)
			return core.NativeRefresh[*Credential]{
				Native: native, Token: native.AccessTokenValue(), RefreshToken: native.RefreshTokenValue(),
				ExpiresAt: native.ExpiresAt, Changed: refreshed,
			}, nil
		},
	})
	return inst
}

// Configure 解析 WorkBuddy 专属配置（UA、设备令牌、提示词、机器盐）。
func (v *adapter) Configure(settings core.Settings, raw map[string]string) {
	v.rt.configure(settings, raw)
}

// Match 判断一份凭证是否属于本区域。
func (v *adapter) Match(fileName, provider string, raw map[string]any) bool {
	if !LooksLikeCredential(raw, fileName, provider) {
		return false
	}
	if vendorID, okVendor := raw[core.VendorKey].(string); okVendor {
		return strings.EqualFold(strings.TrimSpace(vendorID), v.ID())
	}
	if region := RegionForCredential(raw); region != "" {
		return region == v.region
	}
	return v.defaultRegionForParse() == v.region
}

func (v *adapter) defaultRegionForParse() Region {
	settings := v.Env.Settings()
	if settings.RealmEnabled(string(RegionCN)) {
		return RegionCN
	}
	return RegionGlobal
}

// ---- 续期 ----

// Refresh 实现 core.Refresher：用 refresh token 换新的 access token。
//
// 「刷新」与「写回」刻意分开：刷新是供应商协议（这里），写回是宿主与磁盘的
// 事务（由 internal/tasks 的保活编排统一做）。这样供应商不必认识宿主，
// 也避免了每个供应商各写一份 merge + host.auth.save + 落盘。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errRefresh := v.client(ctx).RefreshToken(native); errRefresh != nil {
		return nil, false, errRefresh
	}
	return cred, true, nil
}

// ---- 模型 ----

// StaticModels 返回本区域的模型清单。
//
// 探测由协议层做（按区域的路由最多三路并发并集），这里只做形态转换：
// 上游的 ModelInfo → 中立的 ModelDescriptor → 宿主契约类型。
func (v *adapter) StaticModels(ctx context.Context) ([]pluginapi.ModelInfo, error) {
	client := v.client(ctx)
	models, errModels := client.FetchModels(v.region)
	if errModels != nil {
		return nil, errModels
	}
	v.rt.publishEffortTables(models)
	return modelsToPluginAPI(models), nil
}

// describeModel 拼接模型说明（带积分倍率前缀）。
//
// 倍率是用户最关心的成本信息，放在说明最前面。
func describeModel(model ModelInfo) string {
	description := strings.TrimSpace(model.Description)
	credits := strings.TrimSpace(model.Credits)
	if credits == "" {
		return description
	}
	// 上游的 credits 形如 "x0.05"，去掉尾部 credit 字样避免 "x0.05 credits credit"。
	credits = strings.TrimSuffix(credits, "credits")
	credits = strings.TrimSuffix(credits, "credit")
	return "[" + strings.TrimSpace(credits) + " credit] " + description
}

// modelsToPluginAPI 把协议层模型元数据转成宿主契约类型。
func modelsToPluginAPI(models []ModelInfo) []pluginapi.ModelInfo {
	return core.ConvertModels(models, func(model ModelInfo) (core.ModelDescriptor, bool) {
		if strings.TrimSpace(model.ID) == "" {
			return core.ModelDescriptor{}, false
		}
		return core.ModelDescriptor{
			ID:                 model.ID,
			Name:               model.Name,
			Description:        describeModel(model),
			ContextLength:      model.ContextWindow,
			MaxOutputTokens:    model.MaxTokens,
			Efforts:            model.Efforts,
			DefaultEffort:      model.DefaultEffort,
			SupportsImages:     model.SupportsImages,
			SupportsTools:      model.SupportsToolCall,
			CanDisableThinking: model.CanDisableThinking,
		}, true
	})
}

// ModelsForAuth 返回该凭证可用的模型。
//
// 本供应商的清单按区域探测（不是按账号），因此与 StaticModels 同源。
// 宿主仍会用这份清单把「凭证 ↔ 模型」绑起来，从而淘汰跨区域凭证。
func (v *adapter) ModelsForAuth(ctx context.Context, cred *core.Credential) ([]pluginapi.ModelInfo, error) {
	if cred == nil || cred.RegionValue() != string(v.region) {
		// 区域不符时返回空清单而不是报错：宿主会因此跳过该凭证，
		// 这正是我们想要的结果（避免用错域的凭证打上游）。
		return nil, nil
	}
	return v.StaticModels(ctx)
}

// ---- 执行 ----

// Execute 执行一次非流式对话。
func (v *adapter) Execute(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	client := v.client(ctx)
	result, errChat := client.Chat(ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       ChatMetaFromBody(req.Payload),
	})
	if errChat != nil {
		return nil, errChat
	}
	return &pluginapi.ExecutorResponse{
		Payload: result.Response,
		Headers: map[string][]string{"Content-Type": {core.ContentTypeJSON}},
	}, nil
}

// ExecuteStream 执行一次流式对话。
func (v *adapter) ExecuteStream(ctx context.Context, cred *core.Credential, req *core.ExecuteRequest, sink core.StreamSink) error {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return errNative
	}
	client := v.client(ctx)
	reader, errStream := client.ChatStream(ChatRequest{
		Credential: native,
		Body:       req.Payload,
		Model:      req.Model,
		ClientIP:   req.ClientIP,
		Meta:       ChatMetaFromBody(req.Payload),
	})
	if errStream != nil {
		return errStream
	}
	defer func() { _ = reader.Close() }()

	_, errForward := Stream(reader, StreamOptions{Emit: sink.Emit})
	// 空流不算失败：上游有时返回一条没有任何 delta 的流，
	// 把它当错误会让客户端收到无意义的错误帧。
	if errForward != nil && !IsEmptyStreamError(errForward) && ctx.Err() == nil {
		return errForward
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
	client := v.client(ctx)
	balance, errBalance := client.FetchBalance(native)
	if errBalance != nil {
		return nil, errBalance
	}
	response := BuildQuotaResponse(native, balance)
	return &response, nil
}

// Checkin 执行一次签到。
//
// CN 调 daily-checkin 端点；global 没有该端点活动，走每日活跃奖励
// （活跃信标点亮成长热力图，Bonus Pack 次日凌晨发放），同样幂等。
func (v *adapter) Checkin(ctx context.Context, cred *core.Credential) (*core.CheckinResult, error) {
	if !v.SupportsCheckin() {
		return nil, nil
	}
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, errNative
	}
	if reason := checkinUnsupportedReason(native); reason != "" {
		// 企业账号的签到端点恒返回 400 code 10001（「企业账号不支持该操作」）。
		// 这不是故障，也不该记成「今日已签到」：如实跳过并给出原因。
		return &core.CheckinResult{NotRecorded: true, Message: reason}, nil
	}
	client := v.client(ctx)
	if v.region.IsGlobal() {
		result, errBonus := client.GlobalDailyBonus(ctx, native)
		if errBonus != nil {
			return nil, errBonus
		}
		if result.NotRecorded {
			return &core.CheckinResult{
				NotRecorded: true,
				Message:     core.FirstNonEmpty(result.Message, "国际版每日奖励尚未完成"),
			}, nil
		}
		if result.Already {
			return &core.CheckinResult{
				Already: true,
				Message: fmt.Sprintf("今日活跃已点亮（已连续点亮 %d 天）", result.Streak),
			}, nil
		}
		return &core.CheckinResult{
			Message: core.FirstNonEmpty(result.Message,
				fmt.Sprintf("活跃信标已上报，今日已点亮（已连续点亮 %d 天），奖励积分次日凌晨发放", result.Streak)),
		}, nil
	}
	result, errCheckin := client.DailyCheckin(native)
	if errCheckin != nil {
		return nil, errCheckin
	}
	message := "今天已经签到过了（幂等，无新增）"
	if result.NotRecorded {
		message = core.FirstNonEmpty(result.Message, "本次未领取到签到奖励")
	} else if !result.Already {
		message = fmt.Sprintf("签到成功：+%d 积分 +%d 能量", result.Credit, result.Energy)
	}
	return &core.CheckinResult{
		Already:     result.Already,
		Credit:      result.Credit,
		Energy:      result.Energy,
		Message:     message,
		NotRecorded: result.NotRecorded,
	}, nil
}

// ---- 登录与续期 ----

// LoginStart 发起一次设备授权登录。
func (v *adapter) LoginStart(ctx context.Context, _ map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	state, authURL, errStart := RequestLoginState(ctx, v.region)
	if errStart != nil {
		return nil, errStart
	}
	sessionID := "workbuddy_" + string(v.region) + "_" + core.RandomHex(16)
	StoreLoginSession(v.Env.Logins, sessionID, &PendingLogin{
		Region: v.region,
		State:  state,
	})
	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       authURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(LoginSessionTTL),
		Metadata: map[string]any{
			core.VendorKey: v.ID(),
			"region":       string(v.region),
		},
	}, nil
}

// LoginPoll 轮询一次登录状态。
func (v *adapter) LoginPoll(ctx context.Context, state string) (*pluginapi.AuthLoginPollResponse, error) {
	session, exists, shouldPoll := CheckLoginSession(v.Env.Logins, state)
	if !exists {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	if !shouldPoll {
		// 节流期间：返回 pending 等待，不打上游也不报错
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中完成授权...",
		}, nil
	}
	accessToken, refreshToken, errToken := ExchangeLoginToken(ctx, session.Region, session.State)
	if errToken != nil {
		if IsLoginPending(errToken) {
			// 用户还没在浏览器里完成授权，继续等待。
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusPending,
				Message: "正在等待用户在浏览器中完成授权...",
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: errToken.Error()}, nil
	}
	account, errAccount := FetchLoginAccount(ctx, session.Region, session.State, accessToken)
	if errAccount != nil {
		// token 已拿到但账号信息失败：用最小信息落盘，让用户至少能用起来
		// （账号信息会在后续刷新时补齐）。
		logger.Error("fetch account info failed after token exchange: %v", errAccount)
	}
	credential := &Credential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UID:          account.UID,
		Nickname:     account.Nickname,
		Domain:       account.Domain,
		EnterpriseID: account.EnterpriseID,
	}
	credential.SetRealm(session.Region)
	if credential.Domain == "" {
		credential.Domain = DefaultDomainFor(credential.Realm())
	}
	storageJSON, errStorage := credential.StorageJSON()
	if errStorage != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "构建凭证失败：" + errStorage.Error(),
		}, nil
	}
	RemoveLoginSession(v.Env.Logins, state)
	logger.Info("completed workbuddy login for %s (vendor=%s)",
		core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()), v.ID())
	return &pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			FileName:    core.FileNameFor(v.ID(), core.FirstNonEmpty(credential.UID, core.RandomHex(16))),
			Label:       core.FirstNonEmpty(credential.NicknameValue(), credential.UIDValue()),
			StorageJSON: storageJSON,
		},
	}, nil
}

// MergeStorageJSON 实现 core.StorageMerger：把刷新后的令牌合并回原凭证 JSON。
//
// WorkBuddy 的凭证是嵌套的 auth/account 结构，通用合并会把顶层字段写乱，
// 因此必须由本供应商自己做。
func (v *adapter) MergeStorageJSON(original []byte, credential *core.Credential) ([]byte, error) {
	native, errNative := v.Native(credential)
	if errNative != nil {
		return original, errNative
	}
	return MergeStorageJSON(original, native)
}

// ---- 内部工具 ----

func (v *adapter) client(ctx context.Context) *Client {
	return NewClient(v.rt, v.rt.options(ctx))
}

// SupportsCheckin 报告本区域是否提供签到。
//
// CN 走 daily-checkin 端点；global 走每日活跃奖励（见 tasks.go），
// 两个域都有每日收入路径，因此恒为 true。
func (v *adapter) SupportsCheckin() bool { return true }

// SupportsCheckinFor 实现 core.CheckinCapability：按账号判定签到是否可用。
func (v *adapter) SupportsCheckinFor(cred *core.Credential) bool {
	return v.CheckinUnsupportedReason(cred) == ""
}

// CheckinUnsupportedReason 实现 core.CheckinCapability：返回不可签到的可读原因。
func (v *adapter) CheckinUnsupportedReason(cred *core.Credential) string {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return ""
	}
	return checkinUnsupportedReason(native)
}

// checkinUnsupportedReason 判定该凭证是否没有签到活动。
//
// 企业账号请求 daily-checkin 恒得到 400 code 10001「企业账号不支持该操作」；
// 企业额度按月度周期自动回满，本来就没有签到这条收入路径。
// 只看国内版：国际版的每日活跃奖励是另一套链路（信标 + 次日发放），
// 与国内版签到端点无关。
func checkinUnsupportedReason(cred *Credential) string {
	if cred == nil || cred.Realm().IsGlobal() {
		return ""
	}
	if strings.TrimSpace(cred.EnterpriseIDValue()) != "" {
		return "企业账号不支持签到：企业额度按月度周期自动回满"
	}
	return ""
}

// LoadModelCache 实现 core.ModelCacheLoader：只加载本实例的缓存。
func (v *adapter) LoadModelCache(cached []core.CachedModel) {
	filtered := make([]core.CachedModel, 0, len(cached))
	for _, item := range cached {
		vendorID := strings.TrimSpace(item.VendorID)
		if vendorID == "" {
			vendorID = string(VendorIDFor(v.region))
		}
		if strings.EqualFold(vendorID, v.ID()) {
			filtered = append(filtered, item)
		}
	}
	v.rt.loadCachedModelsFromState(filtered)
}

// ConsoleModels 返回控制台模型页需要的展示明细（含积分倍率）。
func (v *adapter) ConsoleModels() []core.ConsoleModel {
	cached := v.rt.cachedModels()[NormalizeRegion(string(v.region))]
	out := make([]core.ConsoleModel, 0, len(cached))
	for _, model := range cached {
		out = append(out, core.ConsoleModel{
			ID:              model.ID,
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   model.ContextWindow,
			MaxOutputTokens: model.MaxTokens,
			Efforts:         model.Efforts,
			DefaultEffort:   model.DefaultEffort,
			SupportsImages:  model.SupportsImages,
			SupportsTools:   model.SupportsToolCall,
			Credits:         core.FormatRate(model.Credits),
		})
	}
	return out
}

// ModelCredits 实现 core.CreditsProvider：把上游的积分倍率规范化后交给控制台。
func (v *adapter) ModelCredits() map[string]string {
	cached := v.rt.cachedModels()[NormalizeRegion(string(v.region))]
	out := make(map[string]string, len(cached))
	for _, model := range cached {
		if rate := core.FormatRate(model.Credits); rate != "" {
			out[model.ID] = rate
		}
	}
	return out
}

// 本文件解析 WorkBuddy 的供应商专属配置，并保存出站请求需要的运行时快照。
//
// 归属边界：UA/设备令牌/提示词/机器盐都只影响 WorkBuddy 的上游协议，
// 由本包自己解释；根层只把全局设置与原始键交进来（core.Configurable）。

const maxPromptFileSize = 1 << 20

// runtimeConfig 是单个适配器实例的配置快照。
type runtimeConfig struct {
	enabledRealms        []string
	sanitizeFingerprints bool
	passthroughIP        bool
	stateDir             string

	promptMode string
	promptText string

	userAgent     string
	clientVersion string
	cliVersion    string
	clientName    string

	deviceToken     string
	deviceTokenFile string
}

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		sanitizeFingerprints: true,
		clientVersion:        DefaultClientVersion,
		cliVersion:           DefaultCLIVersion,
		clientName:           DefaultClientName,
		promptMode:           PromptModePassThrough,
		promptText:           BuiltinPrompt(),
	}
}

// configure 解析 WorkBuddy 专属键并更新当前实例的运行时快照。
func (r *runtime) configure(settings core.Settings, raw map[string]string) {
	cfg := defaultRuntimeConfig()
	cfg.enabledRealms = append([]string(nil), settings.EnabledRealms...)
	cfg.sanitizeFingerprints = ParseBool(raw["workbuddy_sanitize_fingerprints"], true)
	cfg.passthroughIP = ParseBool(raw["workbuddy_passthrough_ip"], false)
	cfg.stateDir = settings.StateDir

	if value := strings.TrimSpace(raw["workbuddy_user_agent"]); value != "" {
		cfg.userAgent = value
	}
	if value := strings.TrimSpace(raw["workbuddy_client_version"]); value != "" {
		cfg.clientVersion = value
	}
	if value := strings.TrimSpace(raw["workbuddy_cli_version"]); value != "" {
		cfg.cliVersion = value
	}
	if value := strings.TrimSpace(raw["workbuddy_client_name"]); value != "" {
		cfg.clientName = value
	}
	cfg.deviceToken = strings.TrimSpace(raw["workbuddy_device_token"])
	cfg.deviceTokenFile = strings.TrimSpace(raw["workbuddy_device_token_file"])

	// 提示词过滤是全局设置（prompt_mode / prompt_file），WorkBuddy 只是实现方。
	if value := strings.ToLower(strings.TrimSpace(raw["prompt_mode"])); value != "" {
		cfg.promptMode = value
	}
	if path := strings.TrimSpace(raw["prompt_file"]); path != "" {
		text, errRead := readPromptFile(path)
		if errRead != nil {
			logger.Error("load prompt file failed, falling back to built-in prompt: %v", errRead)
		} else if text != "" {
			cfg.promptText = text
		}
	}
	r.cfg.Store(&cfg)
	r.prompt.Store(&promptState{Mode: cfg.promptMode, Text: cfg.promptText})
	salt := strings.TrimSpace(settings.MachineSalt)
	r.salt.Store(&salt)
}

func readPromptFile(path string) (string, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", errRead
	}
	if len(raw) > maxPromptFileSize {
		return "", fmt.Errorf("prompt file %s exceeds %d bytes", path, maxPromptFileSize)
	}
	return strings.TrimSpace(string(raw)), nil
}

// options 返回一次请求要用的 Options（配置快照 + 当前提示词降级态）。
func (r *runtime) options(ctx context.Context) Options {
	cfg := *r.cfg.Load()
	prompt := *r.prompt.Load()
	promptText := prompt.Text
	if prompt.Degraded {
		promptText = degradedPrompt
	}
	return Options{
		Context:              ctx,
		EnabledRealms:        append([]string(nil), cfg.enabledRealms...),
		PromptMode:           prompt.Mode,
		PromptText:           promptText,
		OnContentBlocked:     r.markPromptDegraded,
		SanitizeFingerprints: cfg.sanitizeFingerprints,
		PassthroughIP:        cfg.passthroughIP,
		UserAgent:            cfg.userAgent,
		ClientVersion:        cfg.clientVersion,
		CLIVersion:           cfg.cliVersion,
		ClientName:           cfg.clientName,
		DeviceToken:          cfg.deviceToken,
		DeviceTokenFile:      cfg.deviceTokenFile,
		StateDir:             cfg.stateDir,
	}
}

// ConfigFields 声明 WorkBuddy 专属 YAML 配置。
func (v *adapter) ConfigFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "workbuddy_sanitize_fingerprints", Type: pluginapi.ConfigFieldTypeBoolean, Description: "出站请求体指纹脱敏，默认开启。"},
		{Name: "workbuddy_passthrough_ip", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否透传客户端 IP，默认关闭。"},
		{Name: "workbuddy_user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "出站 User-Agent 覆盖。"},
		{Name: "workbuddy_client_version", Type: pluginapi.ConfigFieldTypeString, Description: "UA 中 WorkBuddy 版本段。"},
		{Name: "workbuddy_cli_version", Type: pluginapi.ConfigFieldTypeString, Description: "UA 中 CLI 版本段。"},
		{Name: "workbuddy_client_name", Type: pluginapi.ConfigFieldTypeString, Description: "归属头客户端名。"},
		{Name: "workbuddy_device_token", Type: pluginapi.ConfigFieldTypeString, Description: "X-Device-Token 全局兜底。"},
		{Name: "workbuddy_device_token_file", Type: pluginapi.ConfigFieldTypeString, Description: "设备令牌文件路径。"},
	}
}

func ParseBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

//go:embed assets/prompt_default.md
var defaultPromptRaw []byte

const degradedPrompt = "You are a helpful assistant. Respond in the user's language, " +
	"follow the user's instructions, and be direct and concise."

const (
	PromptModePassThrough = "passthrough"
	PromptModeCustom      = "custom"
	PromptModeAppend      = "append"
)

// BuiltinPrompt 返回内置的默认提示词文本。
func BuiltinPrompt() string {
	return strings.TrimSpace(string(defaultPromptRaw))
}

// 本文件是 WorkBuddy 每个适配器实例独占的运行时状态。
//
// 配置、提示词降级态、档位表、设备令牌缓存、机器盐与模型探测宿主面
// 都放在 runtime 里，不再使用包级变量；多个 App 实例互不影响。

type promptState struct {
	Mode     string
	Text     string
	Degraded bool
}

type effortState struct {
	Defaults  map[string]string
	Supported map[string][]string
}

type runtime struct {
	env core.Env

	cfg    atomic.Pointer[runtimeConfig]
	prompt atomic.Pointer[promptState]
	effort atomic.Pointer[effortState]
	salt   atomic.Pointer[string]

	deviceMu    sync.Mutex
	devicePath  string
	deviceToken string
	deviceAt    time.Time

	modelMu    sync.Mutex
	modelCache map[Region]modelCacheEntry
}

func newRuntime(env core.Env) *runtime {
	cfg := defaultRuntimeConfig()
	rt := &runtime{env: env, modelCache: map[Region]modelCacheEntry{}}
	rt.cfg.Store(&cfg)
	rt.prompt.Store(&promptState{Mode: PromptModePassThrough, Text: BuiltinPrompt()})
	rt.effort.Store(&effortState{Defaults: map[string]string{}, Supported: map[string][]string{}})
	salt := ""
	rt.salt.Store(&salt)
	return rt
}

func (r *runtime) markPromptDegraded() {
	current := *r.prompt.Load()
	current.Degraded = true
	r.prompt.Store(&current)
}

func (r *runtime) effortTables() (map[string]string, map[string][]string) {
	state := r.effort.Load()
	return state.Defaults, state.Supported
}

func (r *runtime) setEffortTables(defaults map[string]string, supported map[string][]string) {
	old := r.effort.Load()
	next := &effortState{
		Defaults:  make(map[string]string, len(old.Defaults)+len(defaults)),
		Supported: make(map[string][]string, len(old.Supported)+len(supported)),
	}
	for key, value := range old.Defaults {
		next.Defaults[key] = value
	}
	for key, value := range old.Supported {
		next.Supported[key] = value
	}
	for key, value := range defaults {
		next.Defaults[key] = value
	}
	for key, value := range supported {
		next.Supported[key] = value
	}
	r.effort.Store(next)
}

func (r *runtime) publishEffortTables(models []ModelInfo) {
	defaults := make(map[string]string, len(models))
	supported := make(map[string][]string, len(models))
	for _, model := range models {
		if model.DefaultEffort != "" {
			defaults[model.ID] = model.DefaultEffort
		}
		if len(model.Efforts) > 0 {
			supported[model.ID] = model.Efforts
		}
	}
	r.setEffortTables(defaults, supported)
}

func (r *runtime) deriveID(uid, purpose string) string {
	return deriveIDWithSalt(uid, purpose, *r.salt.Load())
}

func (r *runtime) fingerprint(credential *Credential, purpose string) string {
	return r.deriveID(credential.UIDValue(), purpose)
}

func (r *runtime) readDeviceTokenFile(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	r.deviceMu.Lock()
	defer r.deviceMu.Unlock()
	if r.devicePath == trimmed && time.Since(r.deviceAt) < deviceTokenFileTTL {
		return r.deviceToken
	}
	r.devicePath = trimmed
	r.deviceAt = time.Now()
	r.deviceToken = ""
	info, errStat := os.Stat(trimmed)
	if errStat != nil || info.Size() > deviceTokenFileMaxLen {
		return ""
	}
	raw, errRead := os.ReadFile(trimmed)
	if errRead != nil {
		return ""
	}
	r.deviceToken = strings.TrimSpace(string(raw))
	return r.deviceToken
}

func (r *runtime) hostProbeCredential(region Region) *Credential {
	if r.env.Host == nil || r.env.Registry == nil {
		return nil
	}
	ctx := context.Background()
	entries, errList := r.env.Host.ListAuths(ctx)
	if errList != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.Usable() {
			continue
		}
		stored, okStored := r.env.Host.AuthJSON(ctx, entry.AuthIndex)
		if !okStored {
			continue
		}
		credential, okCredential := r.env.Registry.ParseCredential(stored, entry.Name, nil)
		if !okCredential || credential.RegionValue() != string(region) {
			continue
		}
		if native, okNative := credential.Native.(*Credential); okNative && native != nil {
			return native
		}
	}
	return nil
}

// 编译期断言：适配器实际具备的供应商能力。
var (
	_ core.Vendor               = (*adapter)(nil)
	_ core.CheckinProvider      = (*adapter)(nil)
	_ core.Configurable         = (*adapter)(nil)
	_ core.ConfigFieldsProvider = (*adapter)(nil)
	_ core.ConsoleModelProvider = (*adapter)(nil)
	_ core.CreditsProvider      = (*adapter)(nil)
	_ core.LoginProvider        = (*adapter)(nil)
	_ core.ModelCacheLoader     = (*adapter)(nil)
	_ core.ModelsProvider       = (*adapter)(nil)
	_ core.QuotaProvider        = (*adapter)(nil)
	_ core.Refresher            = (*adapter)(nil)
	_ core.StorageMerger        = (*adapter)(nil)
	_ core.TaskCatalogProvider  = (*adapter)(nil)
	_ core.TaskInspector        = (*adapter)(nil)
	_ core.TaskPreparer         = (*adapter)(nil)
	_ core.TaskRunner           = (*adapter)(nil)
	_ core.TaskStatusProvider   = (*adapter)(nil)
	_ core.ScheduledRunner      = (*adapter)(nil)
)
