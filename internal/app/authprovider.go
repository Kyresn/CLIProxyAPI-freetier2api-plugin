package app

// 本文件实现 auth provider 能力：
//   - 凭证识别/解析/刷新与登录分发；
//   - 续期后的凭证合并写回（宿主热更新 + 磁盘兜底）。
//
// 登录协议与续期链路由各供应商实现；根层只做路由、错误分类与状态码映射。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"path/filepath"
)

const (
	// authRefreshInterval 是凭证校验成功后的下次刷新间隔。
	authRefreshInterval = 30 * time.Minute
	// authRefreshRetryAfter 是遇到抖动错误时的重试间隔。
	authRefreshRetryAfter = 5 * time.Minute
	// authRefreshHTTPTimeout 是刷新校验的请求上限。
	authRefreshHTTPTimeout = 60 * time.Second
	// credentialCacheTTL 是凭证回源查找的缓存时长。
	//
	// 凭证轮换主要由宿主驱动（它会把新 StorageJSON 交给各能力），
	// 这里的缓存主要服务管理页轮询：30 秒既够用又能及时看到换号。
	credentialCacheTTL = 30 * time.Second
)

// authParseRPCRequest 与宿主的 auth.parse 请求对齐。
type authParseRPCRequest struct {
	pluginapi.AuthParseRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// authRefreshRPCRequest 与宿主的 auth.refresh 请求对齐。
type authRefreshRPCRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthParse 识别并解析本插件的凭证文件。
//
// 归属判定交给 core.ResolveVendor（三级：vendor 字段 → 文件名前缀 → 内容嗅探），
// 解析本身交给命中的供应商实例。这样新增供应商不需要改这里。
//
// 既不能漏认自己的文件，也不能把别的 provider 的凭证误吞——误吞会让宿主
// 用本插件的结构覆盖对方账号，且是静默的。
func (a *App) handleAuthParse(request []byte) ([]byte, error) {
	var rpc authParseRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	if len(rpc.RawJSON) == 0 {
		return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	var raw map[string]any
	if errUnmarshal := json.Unmarshal(rpc.RawJSON, &raw); errUnmarshal != nil {
		// 非 JSON 的凭证文件不归本插件处理。
		return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	vendor, okVendor := a.registry.Resolve(rpc.FileName, rpc.Provider, raw)
	if !okVendor {
		return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}

	credential, errCredential := vendor.Parse(rpc.RawJSON, rpc.FileName)
	if errCredential != nil {
		return nil, core.NewPluginError("credential_invalid", errCredential.Error(), http.StatusUnprocessableEntity)
	}

	logger.Info("parsed auth %s (vendor=%s)",
		core.FirstNonEmpty(credential.LabelValue(), rpc.FileName, rpc.Path), vendor.ID())

	auth, errAuth := buildAuthDataFor(vendor, credential, rpc.FileName, rpc.RawJSON)
	if errAuth != nil {
		return nil, errAuth
	}
	return core.OKEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

// buildAuthDataFor 构造宿主契约的凭证记录（供应商无关）。
//
// fileName 由调用方决定：
//   - auth.parse 用宿主传来的原文件名（不重命名用户已有的文件）；
//   - auth.login.poll 用 core.FileNameFor 生成的 <vendor>-<uid>.json。
func buildAuthDataFor(vendor core.Vendor, credential *core.Credential, fileName string, storageJSON []byte) (pluginapi.AuthData, error) {
	id := strings.TrimSuffix(fileName, ".json")
	if id == "" {
		// 兜底用账号身份（文件名）；上游 UID 不是身份标识，不该出现在这里。
		id = credential.FileIDValue()
	}
	storage, errStorage := ensureVendorField(storageJSON, vendor.ID())
	if errStorage != nil {
		return pluginapi.AuthData{}, errStorage
	}
	return pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    fileName,
		Label:       credential.LabelValue(),
		StorageJSON: storage,
		Metadata:    credentialMetadata(credential, vendor.ID()),
		Attributes:  credentialAttributes(credential, vendor.ID()),
		// 让宿主在启动后不久就驱动一次刷新校验。
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	}, nil
}

// ensureVendorField 保证凭证 JSON 里带 vendor 字段。
//
// 这是供应商归属的**最强证据**：文件名可能被用户改掉，而 vendor 字段是
// 插件自己写进去的。缺了它，用户改名后的凭证只能靠内容嗅探归属，
// 对结构相似的供应商（WorkBuddy 与 Qoder 都有 token 字段）不可靠。
func ensureVendorField(storageJSON []byte, vendorID string) ([]byte, error) {
	if len(storageJSON) == 0 {
		return storageJSON, nil
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(storageJSON, &payload); errUnmarshal != nil {
		// 解析不了就原样返回：凭证内容由供应商负责，这里不强改。
		return storageJSON, nil
	}
	if existing, okExisting := payload[core.VendorKey].(string); okExisting &&
		strings.EqualFold(strings.TrimSpace(existing), vendorID) {
		return storageJSON, nil
	}
	payload[core.VendorKey] = vendorID
	merged, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		return nil, fmt.Errorf("encode vendor field: %w", errMarshal)
	}
	return merged, nil
}

// credentialMetadata 是回给宿主的可变元数据。
//
// refresh_token 必须在这里（见文件头注释）；token 本身绝不回传。
func credentialMetadata(credential *core.Credential, vendorID string) map[string]any {
	metadata := map[string]any{
		core.VendorKey: vendorID,
		"realm":        credential.RegionValue(),
	}
	if uid := credential.FileIDValue(); uid != "" {
		metadata["uid"] = uid
	}
	if label := credential.LabelValue(); label != "" {
		metadata["nickname"] = label
	}
	if refreshToken := credential.RefreshTokenValue(); refreshToken != "" {
		metadata["refresh_token"] = refreshToken
		metadata["auth_mode"] = "oauth"
	}
	if expiresAt := credential.ExpiresAtValue(); expiresAt > 0 {
		metadata["expires_at"] = expiresAt
	}
	return metadata
}

// credentialAttributes 是回给宿主的不可变属性（不含任何凭证材料）。
func credentialAttributes(credential *core.Credential, vendorID string) map[string]string {
	attributes := map[string]string{
		core.VendorKey: vendorID,
		"realm":        credential.RegionValue(),
	}
	if uid := credential.FileIDValue(); uid != "" {
		attributes["uid"] = uid
	}
	if label := credential.LabelValue(); label != "" {
		attributes["nickname"] = label
	}
	if credential.RefreshTokenValue() != "" {
		attributes["auth_mode"] = "oauth"
	}
	return attributes
}

// handleAuthRefresh 校验凭证并在必要时刷新。
//
// 刷新逻辑**交给凭证所属的供应商**：各家的续期链路差异很大
// （WorkBuddy 是 X-Refresh-Token 换 token，Qoder 是 jobToken 交换），
// 共性只有「凭证失效要报错、抖动要保留、成功要写回」这三条处置策略。
//
// 错误分三类处置：
//   - 凭证确实失效（401/403）→ 返回错误，让宿主把该账号标记为坏；
//   - 上游抖动/未知错误 → 保留原凭证并给一个较短的下次刷新时间（不误伤账号）；
//   - 成功 → 若上游轮换了 token，把新凭证写回。
func (a *App) handleAuthRefresh(request []byte) ([]byte, error) {
	var rpc authRefreshRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID), authRefreshHTTPTimeout)
	defer cancel()

	credential, errCredential := a.credentialForAuth(ctx, rpc.HostCallbackID, rpc.StorageJSON, rpc.AuthID, rpc.Attributes)
	if errCredential != nil {
		return nil, core.NewPluginError("credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	vendor, okVendor := a.registry.ByID(credential.VendorIDValue())
	if !okVendor {
		return nil, core.NewPluginError("credential_missing",
			"credential has no resolvable vendor", http.StatusUnauthorized)
	}

	updated, refreshed, errRefresh := core.RefreshVendor(vendor, ctx, credential)
	if errRefresh != nil {
		if isCredentialRejected(errRefresh) {
			logger.Error("auth refresh rejected for %s: %v", rpc.AuthID, errRefresh)
			return nil, core.ToPluginError(errRefresh)
		}
		// 抖动或未知错误：保留原凭证，稍后重试。
		logger.Debug("auth refresh deferred for %s: %v", rpc.AuthID, errRefresh)
		auth, errData := a.refreshedAuthData(rpc, rpc.StorageJSON, credential)
		if errData != nil {
			return nil, errData
		}
		return core.OKEnvelope(pluginapi.AuthRefreshResponse{
			Auth:             auth,
			NextRefreshAfter: time.Now().Add(authRefreshRetryAfter),
		})
	}

	// 上游没下发新令牌时不重写文件：否则 mtime 会无意义地变动，
	// 用户无法从文件时间判断账号是否真的续期过。
	storage := rpc.StorageJSON
	if refreshed {
		if merged, errStorage := mergeVendorStorage(rpc.StorageJSON, updated, vendor); errStorage == nil {
			storage = merged
		}
	}
	logger.Info("auth refresh succeeded for %s (vendor=%s refreshed=%t)", rpc.AuthID, vendor.ID(), refreshed)
	auth, errData := a.refreshedAuthData(rpc, storage, updated)
	if errData != nil {
		return nil, errData
	}
	return core.OKEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: time.Now().Add(authRefreshInterval),
	})
}

// mergeVendorStorage 把刷新后的凭证合并回原始 StorageJSON。
//
// 合并逻辑**交给供应商自己**（经 core.StorageMerger 可选接口）：各家的 JSON
// 结构不同（WorkBuddy 是嵌套的 auth/account，Qoder 是扁平字段，Cline 是平铺的
// accessToken/refreshToken），通用合并会把顶层字段写乱。
//
// 供应商没实现该接口时原样返回：宁可不动，也不要用错误的结构覆盖用户文件。
// 合并后补 vendor 字段，保证供应商归属始终可判定。
func mergeVendorStorage(original []byte, credential *core.Credential, vendor core.Vendor) ([]byte, error) {
	merger, okMerger := vendor.(core.StorageMerger)
	if !okMerger {
		return original, nil
	}
	merged, errMerge := merger.MergeStorageJSON(original, credential)
	if errMerge != nil {
		return nil, errMerge
	}
	return ensureVendorField(merged, vendor.ID())
}

// refreshedAuthData 在刷新后重建凭证记录，保留宿主侧的既有字段。
//
// 供应商取自凭证自身的 vendor 字段/区域（刷新场景下凭证已存在，
// 归属必然可判定），因此不需要调用方再传。
func (a *App) refreshedAuthData(rpc authRefreshRPCRequest, storageJSON []byte, credential *core.Credential) (pluginapi.AuthData, error) {
	vendor, okVendor := a.registry.ByID(credential.VendorIDValue())
	if !okVendor {
		return pluginapi.AuthData{}, fmt.Errorf("unknown vendor %q", credential.VendorIDValue())
	}
	data, errData := buildAuthDataFor(vendor, credential, rpc.AuthID, storageJSON)
	if errData != nil {
		return pluginapi.AuthData{}, errData
	}
	// 保留宿主传入的标识字段，避免因插件重建而丢失。
	if strings.TrimSpace(rpc.AuthID) != "" {
		data.ID = rpc.AuthID
	}
	return data, nil
}

// isCredentialRejected 报告错误是否表示凭证真的失效。
//
// 判断依据是供应商分类后的 HTTP 状态码（401/403 凭证失效、402 额度耗尽），
// 因此对全部供应商生效，而不是只认 WorkBuddy 的错误类型。
func isCredentialRejected(err error) bool {
	var upstream core.UpstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.HTTPStatus() {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return true
	}
	return false
}

// credentialCacheEntry 是凭证回源查找的缓存项。
type credentialCacheEntry struct {
	credential *core.Credential
	fetchedAt  time.Time
}

// credentialForAuth 取回某个账号的凭证。
//
// 优先用宿主随请求发来的 StorageJSON（最准），缺失时按 auth_index 回源。
//
// **必须支持回源**：宿主的管理端额度路由只发 AuthIndex/AuthID +
// Metadata/Attributes，**不发 StorageJSON**；而插件设计上又不把 token 放进
// metadata/attributes，因此只认 StorageJSON 会把额度查询误报成「凭证缺失」。
//
// 供应商归属由凭证自身决定（vendor 字段 → 文件名 → 内容嗅探），
// 因此这里不需要调用方指定供应商，新增供应商也不必改本函数。
func (a *App) credentialForAuth(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*core.Credential, error) {
	if len(storageJSON) > 0 {
		if credential, okParse := a.registry.ParseCredential(storageJSON, authID, attributes); okParse {
			return credential, nil
		}
	}
	if trimmed := strings.TrimSpace(authID); trimmed != "" {
		if cached, okCache := a.cachedCredential(trimmed); okCache {
			return cached, nil
		}
		raw, errFetch := hostbridge.FetchAuthJSON(ctx, callbackID, trimmed, a.registry)
		if errFetch == nil && len(raw) > 0 {
			if credential, okParse := a.registry.ParseCredential(raw, trimmed, attributes); okParse {
				a.storeCredential(trimmed, credential)
				return credential, nil
			}
		}
	}
	return nil, fmt.Errorf("credential is not available for auth %s", authID)
}

func (a *App) cachedCredential(authID string) (*core.Credential, bool) {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	entry, okEntry := a.credentialCache[authID]
	if !okEntry || time.Since(entry.fetchedAt) > credentialCacheTTL {
		return nil, false
	}
	return entry.credential, true
}

func (a *App) storeCredential(authID string, credential *core.Credential) {
	a.credentialMu.Lock()
	defer a.credentialMu.Unlock()
	a.credentialCache[authID] = credentialCacheEntry{credential: credential, fetchedAt: time.Now()}
}

// resolveVendorCredential 取回凭证并解析出它所属的供应商。
//
// 把「取凭证」与「定供应商」合成一步：几乎所有 ABI 处理器都需要这一对，
// 分开写会到处重复 VendorByID + 错误处理。
func (a *App) resolveVendorCredential(ctx context.Context, callbackID string, storageJSON []byte, authID string, attributes map[string]string) (*core.Credential, core.Vendor, error) {
	credential, errCredential := a.credentialForAuth(ctx, callbackID, storageJSON, authID, attributes)
	if errCredential != nil {
		return nil, nil, core.NewPluginError("credential_missing", errCredential.Error(), http.StatusUnauthorized)
	}
	vendor, okVendor := a.registry.ByID(credential.VendorIDValue())
	if !okVendor {
		return nil, nil, core.NewPluginError("credential_missing",
			"credential has no resolvable vendor", http.StatusUnauthorized)
	}
	return credential, vendor, nil
}

// ---- 登录分发（原 auth_login.go） ----

const (
	// loginHTTPTimeout 是单次登录相关请求的上限。
	loginHTTPTimeout = 30 * time.Second
)

// loginStartRPCRequest 与宿主的 auth.login.start 请求对齐。
type loginStartRPCRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// loginPollRPCRequest 与宿主的 auth.login.poll 请求对齐。
type loginPollRPCRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleAuthLoginStart 开始一次登录，返回用户需要打开的授权链接。
//
// 登录协议由供应商实现，根层只做供应商路由与区域开关校验。
func (a *App) handleAuthLoginStart(request []byte) ([]byte, error) {
	var rpc loginStartRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	vendor, errVendor := a.vendorForLogin(rpc.Metadata)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	response, errStart := a.startVendorLogin(ctx, vendor, rpc.Metadata)
	if errStart != nil {
		return nil, errStart
	}
	logger.Info("started login (vendor=%s)", vendor.ID())
	return core.OKEnvelope(response)
}

// StartLogin 发起一次供应商登录（控制台页面路径）。
//
// 与宿主 <provider>-auth-url 的调用共用 startVendorLogin，差别只在入口：
// 页面路径要能拿到供应商给出的可读错误（例如「滑块验证过期了，重拖一次」），
// 所以由插件自己的管理路由承接，而不是绕宿主那条只会回 500 的转发路径。
func (a *App) StartLogin(ctx context.Context, vendorID string, metadata map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	vendor, okVendor := a.registry.ByID(strings.TrimSpace(vendorID))
	if !okVendor {
		return nil, core.NewPluginError("vendor_not_found", "vendor not found: "+vendorID, http.StatusNotFound)
	}
	if !a.vendorEnabled(vendor) {
		return nil, core.NewPluginError("vendor_disabled",
			fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
	}
	response, errStart := a.startVendorLogin(ctx, vendor, metadata)
	if errStart != nil {
		return nil, errStart
	}
	logger.Info("started login via console (vendor=%s)", vendor.ID())
	return response, nil
}

// LoginCaptchaConfig 取登录前的人机验证控件参数。
func (a *App) LoginCaptchaConfig(ctx context.Context, vendorID string) (core.CaptchaConfig, error) {
	vendor, okVendor := a.registry.ByID(strings.TrimSpace(vendorID))
	if !okVendor {
		return core.CaptchaConfig{}, core.NewPluginError("vendor_not_found", "vendor not found: "+vendorID, http.StatusNotFound)
	}
	if !a.vendorEnabled(vendor) {
		return core.CaptchaConfig{}, core.NewPluginError("vendor_disabled",
			fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
	}
	provider, okCaptcha := core.As[core.LoginCaptchaProvider](vendor)
	if !okCaptcha {
		return core.CaptchaConfig{}, core.NewPluginError("captcha_unsupported",
			vendor.Name()+" 的登录不需要页面内人机验证", http.StatusBadRequest)
	}
	config, errConfig := provider.LoginCaptchaConfig(ctx)
	if errConfig != nil {
		return core.CaptchaConfig{}, core.ToPluginError(errConfig)
	}
	return config, nil
}

// startVendorLogin 是两条登录入口（宿主 auth-url / 插件管理路由）的共同实现。
//
// 供应商标识由这里统一注入：宿主靠 metadata.vendor 把登录会话绑定回具体供应商，
// 缺了它宿主拿不到授权链接，页面只会看到「获取授权链接失败」。
func (a *App) startVendorLogin(ctx context.Context, vendor core.Vendor, metadata map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	login, okLogin := core.As[core.LoginProvider](vendor)
	if !okLogin {
		return nil, core.LoginUnsupported(vendor)
	}
	if errLimit := a.pool.CheckVendorLimit(ctx, hostbridge.CallbackID(ctx), vendor, ""); errLimit != nil {
		return nil, errLimit
	}
	response, errStart := login.LoginStart(ctx, metadata)
	if errStart != nil {
		return nil, core.ToPluginError(errStart)
	}
	if response != nil {
		if response.Metadata == nil {
			response.Metadata = map[string]any{}
		}
		response.Metadata[core.VendorKey] = vendor.ID()
		response.Metadata["region"] = vendor.Region()
	}
	return response, nil
}

// handleAuthLoginPoll 轮询一次登录状态。
//
// 会话状态由供应商自己持有（各自的 state 结构不同），根层不参与。
func (a *App) handleAuthLoginPoll(request []byte) ([]byte, error) {
	var rpc loginPollRPCRequest
	if errDecode := decodeRequest(request, &rpc); errDecode != nil {
		return nil, core.NewPluginError("invalid_request", errDecode.Error(), http.StatusBadRequest)
	}
	sessionID := strings.TrimSpace(rpc.State)
	if sessionID == "" {
		return nil, core.NewPluginError("invalid_request", "login state is required", http.StatusBadRequest)
	}
	vendor, errVendor := a.vendorForLoginSession(sessionID)
	if errVendor != nil {
		return nil, errVendor
	}

	ctx, cancel := context.WithTimeout(hostbridge.WithCallbackID(context.Background(), rpc.HostCallbackID), loginHTTPTimeout)
	defer cancel()

	login, okLogin := core.As[core.LoginProvider](vendor)
	if !okLogin {
		return nil, core.LoginUnsupported(vendor)
	}
	response, errPoll := login.LoginPoll(ctx, sessionID)
	if errPoll != nil {
		return nil, core.ToPluginError(errPoll)
	}
	if response != nil && response.Status == pluginapi.AuthLoginStatusSuccess {
		if errLimit := a.pool.CheckVendorLimit(ctx, rpc.HostCallbackID, vendor, response.Auth.FileName); errLimit != nil {
			return core.OKEnvelope(&pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: errLimit.Error(),
			})
		}
	}
	return core.OKEnvelope(response)
}

// vendorForLogin 从登录请求的 Metadata 解析目标供应商。
//
// 控制台页把用户选的供应商标识放在 Metadata 的 vendor 字段里；
// 缺失时回落到配置里启用的第一个供应商（兼容旧版页面只传 realm 的情况）。
func (a *App) vendorForLogin(metadata map[string]any) (core.Vendor, error) {
	if metadata != nil {
		if rawVendor, okVendor := metadata[core.VendorKey].(string); okVendor {
			vendor, okLookup := a.registry.ByID(rawVendor)
			if !okLookup {
				return nil, core.NewPluginError("invalid_request",
					fmt.Sprintf("unknown vendor %q", rawVendor), http.StatusBadRequest)
			}
			if !a.vendorEnabled(vendor) {
				return nil, core.NewPluginError("vendor_disabled",
					fmt.Sprintf("vendor %s is disabled by plugin configuration", vendor.ID()), http.StatusBadRequest)
			}
			return vendor, nil
		}
		// 旧版页面只传 realm：按区域找第一个匹配的供应商。
		if rawRegion, okRegion := metadata["region"].(string); okRegion && strings.TrimSpace(rawRegion) != "" {
			if vendor, okFind := a.firstVendorForRegion(rawRegion); okFind {
				return vendor, nil
			}
		}
	}
	if vendor, okDefault := a.firstEnabledVendor(); okDefault {
		return vendor, nil
	}
	return nil, core.NewPluginError("vendor_disabled", "no vendor is enabled by plugin configuration", http.StatusBadRequest)
}

// vendorForLoginSession 按登录会话找供应商。
//
// 两条归属路径：先查公共会话登记表（core.LoginSessions）；查不到再逐个问
// 供应商「这个会话是不是你的」（core.LoginSessionOwner）。
//
// 第二条是 Trae 家族的前提：它们把会话存在自己的 SessionStore（回调监听要
// 求实例隔离），没有登记进公共表。少了这条自证，Trae 的登录轮询一律被判成
// 「会话不存在」，用户在控制台看到的却是「授权失败：登录会话不存在或已过期」。
// 这里仍然不按 session id 前缀猜供应商——前缀猜错会把会话路由给别家。
func (a *App) vendorForLoginSession(sessionID string) (core.Vendor, error) {
	if vendorID, okOwns := a.logins.Owns(sessionID); okOwns {
		if vendor, okVendor := a.registry.ByID(vendorID); okVendor {
			return vendor, nil
		}
	}
	for _, vendor := range a.registry.Vendors() {
		owner, okOwner := core.As[core.LoginSessionOwner](vendor)
		if okOwner && owner.OwnsLoginSession(sessionID) {
			return vendor, nil
		}
	}
	return nil, core.NewPluginError("session_not_found", "登录会话不存在或已过期，请重新发起登录", http.StatusNotFound)
}

// SendLoginSMS 发送短信验证码（vendorID 定位实现 SMSLoginProvider 的实例）。
func (a *App) SendLoginSMS(ctx context.Context, vendorID, phone string) (string, error) {
	vendor, okVendor := a.registry.ByID(vendorID)
	if !okVendor {
		return "", core.NewPluginError("vendor_not_found", "vendor not found: "+vendorID, http.StatusNotFound)
	}
	sender, okSMS := core.As[core.SMSLoginProvider](vendor)
	if !okSMS {
		return "", core.NewPluginError("sms_unsupported", vendor.Name()+" 不支持手机验证码登录", http.StatusBadRequest)
	}
	callbackID := hostbridge.CallbackID(ctx)
	if errLimit := a.pool.CheckVendorLimit(ctx, callbackID, vendor, ""); errLimit != nil {
		return "", errLimit
	}
	sessionKey, errSend := sender.SendLoginCode(ctx, phone)
	if errSend != nil {
		return "", core.ToPluginError(errSend)
	}
	return sessionKey, nil
}

// VerifyLoginSMS 用短信验证码完成登录并返回落盘凭证 JSON。
func (a *App) VerifyLoginSMS(ctx context.Context, vendorID, sessionKey, phone, code string) (string, string, string, error) {
	vendor, okVendor := a.registry.ByID(vendorID)
	if !okVendor {
		return "", "", "", core.NewPluginError("vendor_not_found", "vendor not found: "+vendorID, http.StatusNotFound)
	}
	sender, okSMS := core.As[core.SMSLoginProvider](vendor)
	if !okSMS {
		return "", "", "", core.NewPluginError("sms_unsupported", vendor.Name()+" 不支持手机验证码登录", http.StatusBadRequest)
	}
	storageJSON, label, errVerify := sender.VerifyLoginCode(ctx, sessionKey, phone, code)
	if errVerify != nil {
		return "", "", "", core.ToPluginError(errVerify)
	}
	// 文件名按 vendor 前缀 + label 兜底（与 auth.login.poll 的 FileNameFor 同口径）。
	fileName := core.FileNameFor(vendor.ID(), core.FirstNonEmpty(label, "account"))
	return fileName, label, string(storageJSON), nil
}

// SubmitLoginCallback 处理控制台粘贴回来的 localhost 回调链接。
//
// 只有实现 core.CallbackSubmitter 的供应商支持（Trae 家族与 CodeArts）：上游
// 只认 localhost 回调，插件与浏览器不在同一台机器时那条回调永远到不了，
// 用户把浏览器停住的整条链接粘回来，插件就地解析完成登录。
func (a *App) SubmitLoginCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	sessionID := strings.TrimSpace(state)
	if sessionID == "" {
		return nil, core.NewPluginError("invalid_request", "缺少登录会话 state，请重新发起登录", http.StatusBadRequest)
	}
	vendor, errVendor := a.vendorForLoginSession(sessionID)
	if errVendor != nil {
		return nil, errVendor
	}
	submitter, okSubmit := core.As[core.CallbackSubmitter](vendor)
	if !okSubmit {
		return nil, core.NewPluginError("callback_unsupported",
			vendor.Name()+" 的登录不依赖本机回调，无需粘贴回调链接", http.StatusBadRequest)
	}

	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, core.NewPluginError("invalid_request", "请粘贴浏览器地址栏里的完整回调链接", http.StatusBadRequest)
	}

	callbackID := hostbridge.CallbackID(ctx)
	if errLimit := a.pool.CheckVendorLimit(ctx, callbackID, vendor, ""); errLimit != nil {
		return nil, errLimit
	}
	result, errSubmit := submitter.SubmitCallback(ctx, sessionID, trimmed)
	if errSubmit != nil {
		return nil, core.ToPluginError(errSubmit)
	}
	if result == nil {
		return nil, core.NewPluginError("login_failed", "回调处理失败，请重新发起登录", http.StatusInternalServerError)
	}
	if result.Status == pluginapi.AuthLoginStatusSuccess {
		if errLimit := a.pool.CheckVendorLimit(ctx, callbackID, vendor, result.Auth.FileName); errLimit != nil {
			return nil, errLimit
		}
	}
	return result, nil
}

// vendorEnabled 报告供应商的区域是否被配置启用。
func (a *App) vendorEnabled(vendor core.Vendor) bool {
	return core.RealmEnabled(a.Config().EnabledRealms, vendor.Region())
}

// firstEnabledVendor 返回配置里第一个启用的供应商。
func (a *App) firstEnabledVendor() (core.Vendor, bool) {
	for _, vendor := range a.registry.Vendors() {
		if a.vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// firstVendorForRegion 返回某个区域第一个启用的供应商。
func (a *App) firstVendorForRegion(region string) (core.Vendor, bool) {
	normalized := strings.ToLower(strings.TrimSpace(region))
	for _, vendor := range a.registry.Vendors() {
		if vendor.Region() == normalized && a.vendorEnabled(vendor) {
			return vendor, true
		}
	}
	return nil, false
}

// ---- 凭证写回（原 credentials.go） ----

// saveRefreshedCredential 把续期后的凭证写回宿主与磁盘。
//
// 写两处是刻意的双保险：
//   - host.auth.save 让宿主热更新内存注册表；
//   - 直接写文件兜住 host.auth.save 不可用的场景。
func (a *App) saveRefreshedCredential(ctx context.Context, callbackID string, vendor core.Vendor,
	entry core.HostAuth, original []byte, updated *core.Credential, filePath string) {
	payload, okPayload := refreshedStorageJSON(vendor, original, updated)
	targetPath := filePath
	if targetPath == "" {
		targetPath = entry.Path
	}
	if targetPath == "" {
		return
	}
	if okPayload {
		// uid 与文件名大小写对齐后再写回/上报：宿主在 Windows 上同时按文件名
		// 与按 uid 登记凭据，历史写回曾把 uid 落成全小写，导致同一物理文件
		// 在宿主注册表里出现大小写两条目（账号列表重复卡片）。
		payload = alignUIDWithFileName(payload, filepath.Base(targetPath))
		if _, errSave := hostbridge.Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
			Name: entry.Name,
			JSON: payload,
		}); errSave != nil {
			logger.Debug("keepalive %s: host.auth.save failed: %v", entry.Name, errSave)
		}
	}
	// 临时文件 + rename：凭证文件被宿主 fsnotify 监听，半写状态会让宿主读到一个
	// 截断的 JSON。rename 在同一文件系统内是原子的，宿主只会看到旧版或新版。
	tmp := targetPath + ".tmp"
	if errWrite := os.WriteFile(tmp, payload, 0o600); errWrite != nil {
		logger.Debug("keepalive %s: write credential file failed: %v", entry.Name, errWrite)
		return
	}
	if errRename := os.Rename(tmp, targetPath); errRename != nil {
		_ = os.Remove(tmp)
		logger.Debug("keepalive %s: replace credential file failed: %v", entry.Name, errRename)
	}
}

// alignUIDWithFileName 把凭据 JSON 里的 uid 归一成与文件名一致的大小写。
//
// 仅当 uid 与文件名（去 .json）大小写不敏感相等、但字面不一致时改写——
// 这是宿主按 uid 派生出第二种拼写、把同一物理文件登记成两条目的场景。
// uid 是完全不同的值（供应商自有的上游 uid）时不碰。JSON 解析失败原样
// 返回：对齐是尽力而为，不能让写回因此失败。
func alignUIDWithFileName(payload []byte, fileName string) []byte {
	fileID := strings.TrimSuffix(fileName, ".json")
	if fileID == "" {
		return payload
	}
	var m map[string]any
	if errUnmarshal := json.Unmarshal(payload, &m); errUnmarshal != nil {
		return payload
	}
	current, _ := m["uid"].(string)
	if current == "" || current == fileID || !strings.EqualFold(current, fileID) {
		return payload
	}
	m["uid"] = fileID
	encoded, errMarshal := json.Marshal(m)
	if errMarshal != nil {
		return payload
	}
	return encoded
}

// refreshedStorageJSON 用供应商自己的合并逻辑更新凭证 JSON。
func refreshedStorageJSON(vendor core.Vendor, original []byte, updated *core.Credential) ([]byte, bool) {
	merger, okMerger := vendor.(core.StorageMerger)
	if !okMerger {
		return original, false
	}
	merged, errMerge := merger.MergeStorageJSON(original, updated)
	if errMerge != nil {
		return original, false
	}
	return merged, true
}
