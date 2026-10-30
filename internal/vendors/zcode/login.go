package zcode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// OAuth CLI 登录流程（headless，服务端中转）与 API Key 兑换。
//
// 流程：
//  1. POST oauth/cli/init（Bearer <本地 poll_token>）→ flow_id + authorize_url
//     + poll_token。**2026-09 起 poll_token 由服务端下发**，后续轮询必须改用
//     该值；未下发时回退本地随机值（旧协议兼容）。
//  2. 用户在浏览器打开 authorize_url 完成授权。
//  3. GET oauth/cli/poll/{flow_id}（Bearer <poll_token>）→ status=ready 时给出
//     token（Coding Plan JWT）与 zai.access_token。
//  4. 用 access_token 兑换业务 API Key（api.z.ai 链路），免人机验证码。
//
// 请求头刻意保持最小集：官方 CLI 只带 Authorization 与 Content-Type，
// 夹带伪造头会让上游对 OAuth 会话产生异常的设备绑定限制。
// 参考 zcode2api/app/oauth.py。

const (
	// loginFlowTTL 是单次登录会话的有效期。
	loginFlowTTL = 15 * time.Minute
	// loginHTTPTimeout 是登录相关单次请求的上限。
	loginHTTPTimeout = 30 * time.Second
	// loginPollInterval 是两次访问上游 poll 之间的最小间隔。
	//
	// 控制台页每 2 秒轮询一次，不节流会让上游看到远超官方客户端的请求密度。
	loginPollInterval = 1500 * time.Millisecond
)

// pendingLogin 是一次进行中的登录会话数据（TTL / 节流由 auth 会话表统一管理）。
type pendingLogin struct {
	FlowID    string
	PollToken string
	// VendorID 是发起登录的实例（zcodecn / zcodeglobal）。
	VendorID string
	// Region 是该实例的区域（cn / global），落盘时写进凭证。
	Region string
	// Provider 是本次登录使用的 OAuth provider（bigmodel / zai）。
	Provider string
}

// LoginStart 发起 OAuth 授权，返回用户需在浏览器打开的链接。
//
// vendorID / region 决定凭证归属（zcodecn + cn / zcodeglobal + global），
// provider 决定授权页（bigmodel 国内 / zai 国际）。三者都打到 zcode.z.ai
// 的设备授权流。
//
// 不接收上游基地址覆盖参数：OAuth 的 init/poll 固定在 zcode.z.ai，与对话
// 通道（api.z.ai，可经 <实例ID>_base_url 覆盖）是两个不同的域名。早期版本把
// 对话的覆盖值传进来，会让只想改对话地址的用户连带改坏登录链路。
func LoginStart(ctx context.Context, logins core.LoginSessions, vendorID, region, provider string) (*pluginapi.AuthLoginStartResponse, error) {
	rawProvider := provider
	provider = NormalizeProvider(rawProvider)
	if provider == "" {
		// 不回落 zai：国内实例配错时会静默走国际登录、拿到错渠道的凭证。
		return nil, fmt.Errorf("未知的 ZCode 登录 provider：%q", rawProvider)
	}
	region = core.FirstNonEmpty(NormalizeRegion(region), RegionGlobal)
	pollToken := NewHexID(32)

	body, _ := json.Marshal(map[string]string{"provider": provider})
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost,
		ZCodeOrigin+PathOAuthInit, bytes.NewReader(body))
	if errNew != nil {
		return nil, fmt.Errorf("create oauth init request: %w", errNew)
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("oauth init request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("OAuth 初始化失败（HTTP %d）：%s", resp.StatusCode, textPreview(string(raw)))
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID       string `json:"flow_id"`
			AuthorizeURL string `json:"authorize_url"`
			PollToken    string `json:"poll_token"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode oauth init response: %w", errUnmarshal)
	}
	if payload.Code != codeSuccess {
		return nil, fmt.Errorf("OAuth 初始化被上游拒绝（code=%d）：%s", payload.Code, payload.Msg)
	}
	if payload.Data.FlowID == "" || payload.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("OAuth 初始化返回的流程数据不完整")
	}
	// 服务端下发的 poll_token 优先（新协议）；未下发则沿用本地随机值。
	effectiveToken := core.FirstNonEmpty(payload.Data.PollToken, pollToken)

	sessionID := "zcode_" + NewHexID(8)
	logins.Register(vendorID, sessionID, loginFlowTTL, loginPollInterval, &pendingLogin{
		FlowID:    payload.Data.FlowID,
		PollToken: effectiveToken,
		VendorID:  vendorID,
		Region:    region,
		Provider:  provider,
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  core.ProviderKey,
		URL:       payload.Data.AuthorizeURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(loginFlowTTL),
	}, nil
}

// LoginPoll 轮询一次授权状态；完成时兑换出完整凭证。
func LoginPoll(ctx context.Context, logins core.LoginSessions, sessionID string) (*pluginapi.AuthLoginPollResponse, error) {
	session, pollState := logins.Poll(sessionID)
	if pollState == core.PollMissing {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录",
		}, nil
	}
	// 轮询节流：未到间隔就返回 pending，避免面板把上游打满。
	if pollState == core.PollThrottled {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	pending, okPending := session.Data.(*pendingLogin)
	if !okPending {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话数据已损坏，请重新发起登录",
		}, nil
	}

	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet,
		ZCodeOrigin+PathOAuthPoll+"/"+pending.FlowID, nil)
	if errNew != nil {
		return nil, fmt.Errorf("create poll request: %w", errNew)
	}
	req.Header.Set("Authorization", "Bearer "+pending.PollToken)

	resp, errDo := hostbridge.Client(ctx, loginHTTPTimeout).Do(req)
	if errDo != nil {
		// 网络抖动不判死：保留会话，下次轮询再试。
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status string `json:"status"`
			// JWT 的字段名随上游版本变动过（token / zcodejwttoken），两者都收。
			Token         string `json:"token"`
			ZCodeJWTToken string `json:"zcodejwttoken"`
			AccessToken   string `json:"access_token"`
			User          struct {
				UserID string `json:"user_id"`
				ID     string `json:"id"`
				Email  string `json:"email"`
			} `json:"user"`
			// bigmodel 是国内版登录的第三方 token；zai 是国际版。
			// 两者不可互相替代，按本次 provider 优先取对应分支。
			Bigmodel struct {
				AccessToken    string `json:"access_token"`
				AccessTokenAlt string `json:"accessToken"`
			} `json:"bigmodel"`
			Zai struct {
				AccessToken    string `json:"access_token"`
				AccessTokenAlt string `json:"accessToken"`
			} `json:"zai"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	// code=3004 是上游明示的会话过期，其余 4xx 视为终态失败。
	if payload.Code != codeSuccess {
		if payload.Code == codeSessionExpired {
			logins.Delete(sessionID)
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: "授权会话已过期，请重新发起登录",
			}, nil
		}
		if resp.StatusCode >= 400 {
			logins.Delete(sessionID)
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusError,
				Message: fmt.Sprintf("上游拒绝轮询（HTTP %d, code=%d）：%s", resp.StatusCode, payload.Code, payload.Msg),
			}, nil
		}
	}
	if payload.Data.Status == "failed" {
		logins.Delete(sessionID)
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "授权被拒绝或失败",
		}, nil
	}

	// 判定依据是「拿到 JWT」而不是 status 字符串：上游给 token 即代表授权完成，
	// 死等某个特定 status 值会让会话一直停在 pending 直到超时。
	jwt := core.FirstNonEmpty(payload.Data.Token, payload.Data.ZCodeJWTToken)
	if jwt == "" {
		return &pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}, nil
	}
	logins.Delete(sessionID)
	userID := core.FirstNonEmpty(payload.Data.User.UserID, payload.Data.User.ID, UserIDFromJWT(jwt))
	email := core.FirstNonEmpty(strings.ToLower(strings.TrimSpace(payload.Data.User.Email)), EmailFromJWT(jwt))
	cred := &Credential{
		Vendor:   core.FirstNonEmpty(pending.VendorID, VendorIDGlobal),
		JWTToken: jwt,
		AuthMode: "oauth",
		Profile:  NewDeviceProfile(),
		Email:    email,
		UserID:   userID,
		Region:   core.FirstNonEmpty(pending.Region, RegionGlobal),
		Provider: pending.Provider,
	}

	// 兑换 API Key：作为备选凭证一并持久化。
	// 兑换失败不阻断登录——JWT 仍可用于额度查询与套餐领取。
	apiWarning := ""
	tokens := loginTokens{
		TopLevel:    payload.Data.AccessToken,
		Bigmodel:    payload.Data.Bigmodel.AccessToken,
		BigmodelAlt: payload.Data.Bigmodel.AccessTokenAlt,
		Zai:         payload.Data.Zai.AccessToken,
		ZaiAlt:      payload.Data.Zai.AccessTokenAlt,
	}
	if cred.Provider == ProviderBigmodel {
		// 国内版 token 属于智谱开放平台，不能拿去 api.z.ai 兑换，无需取值。
		apiWarning = "（国内版使用 Start Plan JWT 凭证）"
	} else if accessToken := tokens.pick(cred.Provider); accessToken != "" {
		apiKey, errExchange := ExchangeAPIKey(ctx, accessToken)
		if errExchange != nil {
			apiWarning = fmt.Sprintf("（API Key 兑换失败：%v，已使用 Start Plan JWT 凭证）", errExchange)
		} else {
			cred.APIKey = apiKey
		}
	} else {
		apiWarning = "（上游未返回 access_token，已使用 Start Plan JWT 凭证）"
	}

	cred.Label = core.FirstNonEmpty(cred.Email, "ZCode-"+MaskedKey(core.FirstNonEmpty(cred.APIKey, cred.JWTToken)))

	storage, errJSON := cred.StorageJSON()
	if errJSON != nil {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "生成凭证失败：" + errJSON.Error(),
		}, nil
	}

	fileName := zcodeFileName(cred)
	return &pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功" + apiWarning,
		Auth: pluginapi.AuthData{
			Provider:    core.ProviderKey,
			ID:          strings.TrimSuffix(fileName, ".json"),
			FileName:    fileName,
			Label:       cred.Label,
			StorageJSON: storage,
		},
	}, nil
}

// loginTokens 是轮询回包里可能出现的第三方 access_token。
//
// 官方在不同 provider / 版本下把它放在不同位置，因此先原样收齐再按 provider
// 取值；六个字符串参数容易在调用处张冠李戴，收敛成结构体后调用点自带字段名。
type loginTokens struct {
	TopLevel    string
	Bigmodel    string
	BigmodelAlt string
	Zai         string
	ZaiAlt      string
}

// pick 按登录 provider 优先取对应的第三方 access_token。
//
// bigmodel 登录回 data.bigmodel，zai 登录回 data.zai；另一分支只作兼容兜底。
func (t loginTokens) pick(provider string) string {
	if provider == ProviderBigmodel {
		return core.FirstNonEmpty(t.Bigmodel, t.BigmodelAlt, t.Zai, t.ZaiAlt, t.TopLevel)
	}
	return core.FirstNonEmpty(t.Zai, t.ZaiAlt, t.Bigmodel, t.BigmodelAlt, t.TopLevel)
}

// ExchangeAPIKey 把 OAuth access_token 兑换为业务 API Key。
//
// 链路：z/login 换业务 token → getCustomerInfo 取机构与项目 → 查/建
// name="zcode-api-key" 的 Key → copy 接口解密出 secretKey，拼成
// "<apiKey>.<secretKey>"（智谱 API Key 形态）。
func ExchangeAPIKey(ctx context.Context, accessToken string) (string, error) {
	client := hostbridge.Client(ctx, loginHTTPTimeout)

	bizToken, errBiz := exchangeBizToken(ctx, client, accessToken)
	if errBiz != nil {
		return "", errBiz
	}

	orgID, projID, errOrg := lookupOrgProject(ctx, client, bizToken)
	if errOrg != nil {
		return "", errOrg
	}

	keyURL := fmt.Sprintf("%s/api/biz/v1/organization/%s/projects/%s/api_keys", ZAIOrigin, orgID, projID)
	apiKey, errKey := ensureAPIKey(ctx, client, bizToken, keyURL)
	if errKey != nil {
		return "", errKey
	}

	secretKey, errSecret := decryptSecret(ctx, client, bizToken, keyURL, apiKey)
	if errSecret != nil {
		return "", errSecret
	}
	return apiKey + "." + secretKey, nil
}

// exchangeBizToken 用 OAuth access_token 换取业务 token。
func exchangeBizToken(ctx context.Context, client *http.Client, accessToken string) (string, error) {
	body, _ := json.Marshal(map[string]string{"token": accessToken})
	req, errNew := http.NewRequestWithContext(ctx, http.MethodPost, ZAIOrigin+PathZLogin, bytes.NewReader(body))
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("Content-Type", "application/json")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("z/login 请求失败：%w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("z/login 失败（HTTP %d）：%s", resp.StatusCode, textPreview(string(raw)))
	}
	var payload struct {
		Data struct {
			AccessToken    string `json:"access_token"`
			AccessTokenAlt string `json:"accessToken"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", fmt.Errorf("解析 z/login 响应失败：%w", errUnmarshal)
	}
	token := core.FirstNonEmpty(payload.Data.AccessToken, payload.Data.AccessTokenAlt)
	if token == "" {
		return "", fmt.Errorf("z/login 响应不含业务凭证")
	}
	return token, nil
}

// lookupOrgProject 取「默认机构」与「默认项目」的 ID。
func lookupOrgProject(ctx context.Context, client *http.Client, bizToken string) (string, string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, ZAIOrigin+PathCustomerInfo, nil)
	if errNew != nil {
		return "", "", errNew
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", fmt.Errorf("getCustomerInfo 请求失败：%w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("getCustomerInfo 失败（HTTP %d）", resp.StatusCode)
	}

	var payload struct {
		Data struct {
			Organizations []struct {
				OrganizationID   string `json:"organizationId"`
				OrganizationName string `json:"organizationName"`
				Projects         []struct {
					ProjectID   string `json:"projectId"`
					ProjectName string `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", "", fmt.Errorf("解析客户信息失败：%w", errUnmarshal)
	}
	orgs := payload.Data.Organizations
	if len(orgs) == 0 {
		return "", "", fmt.Errorf("账号下没有可用机构")
	}

	// 优先「默认机构」/「默认项目」，否则退回首个。
	org := orgs[0]
	for _, item := range orgs {
		if strings.Contains(item.OrganizationName, "默认机构") {
			org = item
			break
		}
	}
	if len(org.Projects) == 0 {
		return "", "", fmt.Errorf("机构下没有可用项目")
	}
	proj := org.Projects[0]
	for _, item := range org.Projects {
		if strings.Contains(item.ProjectName, "默认项目") {
			proj = item
			break
		}
	}
	return org.OrganizationID, proj.ProjectID, nil
}

// ensureAPIKey 查找已有的 zcode-api-key，没有则创建一个。
func ensureAPIKey(ctx context.Context, client *http.Client, bizToken, keyURL string) (string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, keyURL, nil)
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)

	if resp, errDo := client.Do(req); errDo == nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var payload struct {
			Data []struct {
				Name   string `json:"name"`
				APIKey string `json:"apiKey"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			for _, item := range payload.Data {
				if item.Name == "zcode-api-key" && strings.TrimSpace(item.APIKey) != "" {
					return item.APIKey, nil
				}
			}
		}
	}

	body, _ := json.Marshal(map[string]string{"name": "zcode-api-key"})
	createReq, errCreate := http.NewRequestWithContext(ctx, http.MethodPost, keyURL, bytes.NewReader(body))
	if errCreate != nil {
		return "", errCreate
	}
	createReq.Header.Set("Authorization", "Bearer "+bizToken)
	createReq.Header.Set("Content-Type", "application/json")

	resp, errDo := client.Do(createReq)
	if errDo != nil {
		return "", fmt.Errorf("创建 API Key 请求失败：%w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Data struct {
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil || payload.Data.APIKey == "" {
		return "", fmt.Errorf("创建 API Key 失败：%s", textPreview(string(raw)))
	}
	return payload.Data.APIKey, nil
}

// decryptSecret 调 copy 接口取回 secretKey。
//
// 列表接口只给 apiKey 明文，secretKey 需经此接口解密，两者拼起来才是完整凭证。
func decryptSecret(ctx context.Context, client *http.Client, bizToken, keyURL, apiKey string) (string, error) {
	req, errNew := http.NewRequestWithContext(ctx, http.MethodGet, keyURL+"/copy/"+apiKey, nil)
	if errNew != nil {
		return "", errNew
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("解密 Secret Key 请求失败：%w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		Data struct {
			SecretKey string `json:"secretKey"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", fmt.Errorf("解析 Secret Key 响应失败：%w", errUnmarshal)
	}
	if payload.Data.SecretKey == "" {
		return "", fmt.Errorf("未能解密 Secret Key")
	}
	return payload.Data.SecretKey, nil
}

// zcodeFileName 生成凭证文件名（邮箱优先，便于辨识账号）。
func zcodeFileName(cred *Credential) string {
	seed := core.FirstNonEmpty(cred.Email, MaskedKey(core.FirstNonEmpty(cred.APIKey, cred.JWTToken)))
	return fileNameFor(core.FirstNonEmpty(cred.Vendor, VendorID), seed)
}

// base64URLDecode 解 base64url（容忍缺省填充）。
func base64URLDecode(segment string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	padded := segment + strings.Repeat("=", (4-len(segment)%4)%4)
	return base64.URLEncoding.DecodeString(padded)
}

// Refresh 校验凭证。
//
// ZCode 没有 refresh_token 流程（JWT 与 API Key 都不支持续期），因此这里只做
// 本地校验并返回 refreshed=false——不触发无意义的写盘。
func (v *adapter) Refresh(ctx context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	native, errNative := v.Native(cred)
	if errNative != nil {
		return nil, false, errNative
	}
	if errValidate := ValidateCredential(ctx, native); errValidate != nil {
		return nil, false, errValidate
	}
	return cred, false, nil
}

// ValidateCredential 校验凭证形态与上游可用性。
//
// 只做本地形态校验，不产生上游流量：宿主会在每次刷新时调用本方法，
// 让它去打上游等于把额度查询变成了周期性的风控信号。
func ValidateCredential(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return transientErr("zcode credential is nil")
	}
	if strings.TrimSpace(cred.APIKey) == "" && strings.TrimSpace(cred.JWTToken) == "" {
		return transientErr("zcode credential missing both api_key and jwt_token")
	}
	return nil
}

// MaskedKey 对凭证脱敏：保留前 4 位与后 4 位。
func MaskedKey(val string) string {
	s := strings.TrimSpace(val)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "***" + s[len(s)-4:]
}
