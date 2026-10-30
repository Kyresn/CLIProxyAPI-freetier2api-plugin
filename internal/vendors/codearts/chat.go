package codearts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/internal/hostbridge"
)

type ChatRequest struct {
	Credential *Credential
	Body       []byte
	Stream     bool
	// Benefit 走每日免费福利额度通道（请求头带 maas_type: benefit）。
	//
	// 本插件只服务免费福利通道：上游把每日免费额度与订阅积分分成两条通道，
	// 同一个模型名两边都有，区别就是这个头。要接积分通道时在这里分开即可。
	Benefit bool
}

// Chat 执行一次非流式对话。
func Chat(ctx context.Context, baseURL string, req ChatRequest) ([]byte, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read codearts response: %w", errRead)
	}

	if resp.StatusCode >= 400 {
		return nil, Classify(resp.StatusCode, string(body))
	}
	// HTTP 200 也可能是业务错误信封（{"error_code":"4004","error_msg":...}）：
	// 原样透传客户端只会拿到一个不符合 OpenAI 契约的响应体。
	if failure, okFailure := ErrorFromPayload(body); okFailure {
		return nil, failure
	}

	return body, nil
}

// ChatStream 执行一次流式对话。
func ChatStream(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	resp, errDo := doCodeArtsRequest(ctx, baseURL, req)
	if errDo != nil {
		return nil, errDo
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, Classify(resp.StatusCode, string(body))
	}

	return resp, nil
}

// ForwardStream 逐帧转发上游 SSE，并把业务错误帧翻成 OpenAI 错误帧。
//
// 宿主只把 choices 缺失且带 error 对象的帧当成终止错误；上游却在 HTTP 200 的
// `data:` 帧里直接回 {"error_code":"InferHub.4004.200","error_msg":"benefit not found"}。
// 原样转发时宿主认不出它，收尾时用「流提前结束」覆盖真实原因（issue #58）。
// 帧遍历本身复用 hostbridge.ForwardRawSSE，这里只做载荷改写。
func ForwardStream(body io.Reader, emit func([]byte) error) error {
	return hostbridge.ForwardRawSSE(body, func(payload []byte) error {
		failure, okFailure := ErrorFromPayload(payload)
		if !okFailure {
			return emit(payload)
		}
		frame, errFrame := OpenAIErrorFrame(failure)
		if errFrame != nil {
			return errFrame
		}
		return emit(frame)
	})
}

func doCodeArtsRequest(ctx context.Context, baseURL string, req ChatRequest) (*http.Response, error) {
	if req.Credential == nil {
		return nil, fmt.Errorf("codearts credential is nil")
	}

	target := strings.TrimRight(baseURL, "/")
	if target == "" {
		target = SnapEngineApiHost
	}

	endpoint := target + EpChatV2

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Body))
	if errReq != nil {
		return nil, fmt.Errorf("create codearts http request: %w", errReq)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("Agent-Type", "PromptCenter")
	httpReq.Header.Set("User-Agent", "CodeArtsAgent/5.2.0")
	if req.Benefit {
		// 必须放在签名之前：签名覆盖该头，晚设就不生效，
		// 请求会被判成积分通道并按积分计费。
		httpReq.Header.Set(HeaderMaasType, MaasBenefit)
	}

	// 签名
	signHuaweiRequest(httpReq, req.Body, req.Credential)

	timeout := 120 * time.Second
	if req.Stream {
		timeout = 300 * time.Second
	}

	client := hostbridge.Client(ctx, timeout)
	resp, errCall := client.Do(httpReq)
	if errCall != nil {
		return nil, fmt.Errorf("codearts request failed: %w", errCall)
	}

	return resp, nil
}

// signHuaweiRequest 为请求注入华为云 SDK-HMAC-SHA256 签名。
func signHuaweiRequest(req *http.Request, body []byte, cred *Credential) {
	signSDKRequest(req, body, cred, sdkSignOptions{AddAuthToken: true})
}

// sdkSignOptions 是签名管线的差异参数。
type sdkSignOptions struct {
	// HeaderNames 参与签名的头；nil 表示当前请求上的全部头。
	HeaderNames []string
	// AddAuthToken 是否额外发送 X-Auth-Token（旧网关需要，snap 不需要）。
	AddAuthToken bool
	// EnsureJSONContentType 在 POST 缺 Content-Type 时补 application/json。
	EnsureJSONContentType bool
	// Append 是签名后追加、不参与验签的头。
	Append map[string]string
}

// signSDKRequest 是华为云 SDK-HMAC-SHA256 的唯一签名实现。
func signSDKRequest(req *http.Request, body []byte, cred *Credential, opts sdkSignOptions) {
	if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return
	}

	xDate := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Sdk-Date", xDate)
	req.Header.Set("Host", req.URL.Host)
	if cred.SecurityToken != "" {
		req.Header.Set("X-Security-Token", cred.SecurityToken)
		if opts.AddAuthToken {
			req.Header.Set("X-Auth-Token", cred.SecurityToken)
		}
	}
	if opts.EnsureJSONContentType && req.Method != http.MethodGet && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Sdk-Content-Sha256", payloadHash)

	canonicalHeadersStr, signedHeadersStr := "", ""
	if opts.HeaderNames == nil {
		canonicalHeadersStr, signedHeadersStr = canonicalHeaders(req)
	} else {
		keys := make([]string, 0, len(opts.HeaderNames))
		seen := make(map[string]bool, len(opts.HeaderNames))
		for _, name := range opts.HeaderNames {
			if req.Header.Get(name) == "" {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			keys = append(keys, name)
		}
		sort.Strings(keys)
		var canonical strings.Builder
		for _, name := range keys {
			canonical.WriteString(name + ":" + strings.TrimSpace(req.Header.Get(name)) + "\n")
		}
		canonicalHeadersStr = canonical.String()
		signedHeadersStr = strings.Join(keys, ";")
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeadersStr,
		signedHeadersStr,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"SDK-HMAC-SHA256",
		xDate,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hmacHex(cred.SecretAccessKey, stringToSign)
	req.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+cred.AccessKeyID+
		", SignedHeaders="+signedHeadersStr+
		", Signature="+signature)

	for key, value := range opts.Append {
		req.Header.Set(key, value)
	}
}

// snapSignedHeaderNames 是 snap-access 请求参与签名的固定头集合。
//
// 参考实现（逆向 IDE 的 signer.js）只签这些头；把 Agent-Type / User-Agent
// 等业务头也塞进 SignedHeaders 会被网关以 APIG.0301 拒绝（实测）。
var snapSignedHeaderNames = []string{
	"content-type",
	"host",
	"x-sdk-content-sha256",
	"x-sdk-date",
	"x-security-token",
	"maas_type",
}

// signSnapRequest 为 snap-access ops/statistics 请求注入 SDK-HMAC-SHA256 签名。
//
// 与 signHuaweiRequest 的区别：只签固定头集合，且 Agent-Type / X-Language
// 在签名**之后**追加。这两个头仍会被发送（服务端路由需要），但不参与验签。
func signSnapRequest(req *http.Request, body []byte, cred *Credential) {
	signSDKRequest(req, body, cred, sdkSignOptions{
		HeaderNames:           snapSignedHeaderNames,
		EnsureJSONContentType: true,
		Append: map[string]string{
			"Agent-Type": "PromptCenter",
			"X-Language": "zh-cn",
		},
	})
}

func canonicalHeaders(req *http.Request) (headers, signed string) {
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		v := req.Header.Get(k)
		sb.WriteString(k + ":" + strings.TrimSpace(v) + "\n")
	}
	return sb.String(), strings.Join(keys, ";")
}

func canonicalURI(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	out := strings.Join(segments, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := vals[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// 常量与端点定义：华为云 CodeArts Agent 配置。
// 参考：/Users/jiandan/Workspaces/codearts2api/internal/upstream/const.go

const (
	VendorID   = "codearts"
	VendorName = "CodeArts"

	// 华为云 CodeArts API 网关
	SnapEngineApiHost = "https://snap-access.cn-north-4.myhuaweicloud.com"
	SnapManagerHost   = SnapEngineApiHost + "/snap-manager"
	PortalHost        = "https://codearts.huaweicloud.com/portal"
	// STSHost 临时 AK/SK 与令牌服务。**不是** snap-manager：把 /v1/oauth2/tokens
	// 打到 snap-manager 会得到 404 APIG.0101（实测），续期与授权码换 token 都在这里。
	STSHost     = "https://sts.cn-north-4.myhuaweicloud.com"
	BenefitHost = "https://opengw.developer.huaweicloud.com"

	ClientID = "codearts-agent"

	// 授权链接里的 plugin-name / plugin-version：门户按这两个头识别调用方，
	// 与官方 CodeArts Agent 插件保持一致（逆向自 huaweicloud.authentication）。
	PluginName    = "snap_AIIDE"
	PluginVersion = "5.2.0"

	// 本机回调：门户按授权链接里的 port 拼出 http://127.0.0.1:<port>/oauth/callback。
	//
	// 插件与浏览器不在同一台机器时收不到，只能让用户把这条链接粘回控制台。
	LocalCallbackPort = 7866
	LocalCallbackPath = "/oauth/callback"
	LocalCallbackURL  = "http://127.0.0.1:7866" + LocalCallbackPath

	// 端点路径
	EpChatV2        = "/api/v2/chat/completions"
	EpModelBuiltin  = "/v1/model/builtin"
	EpBenefitConfig = "/api/v1/gateway/config"
	EpBenefitClaim  = "/api/v1/benefit/claim"
	// EpBenefitGateway 报告福利通道是否开放（注意在 snap 主机上，不是 benefit 主机）。
	EpBenefitGateway = "/v1/benefit-gateway-config"
	// EpBenefitBalance 查询每日 / 每月福利 Token 额度。
	EpBenefitBalance = "/api/v1/user/tokens/balance"

	// 每日签到（积分活动）走 IDE 直连的 snap-access ops 接口，与福利 Token
	// 的 opengw 通道是两套协议：
	//   账户/套餐  GET  {snap}/snap-manager/v1/statistics/plugin
	//   活动列表   GET  {snap}/v1/ops/delivery?channel=IDE
	//   领取       POST {snap}/v1/ops/claim   {campaignId, channel:"IDE"}
	//   领取确认   POST {snap}/v1/ops/confirm {campaignId}
	EpPackageInfo = "/snap-manager/v1/statistics/plugin"
	EpOpsDelivery = "/v1/ops/delivery"
	EpOpsClaim    = "/v1/ops/claim"
	EpOpsConfirm  = "/v1/ops/confirm"
	// OpsChannel 声明请求来自 IDE 形态（对齐官方客户端的 channel=IDE）。
	OpsChannel = "IDE"
	// OpsDailyLoginType 是「每日登录领取」在活动列表里的 type。
	OpsDailyLoginType = "USER_LOGIN"

	// HeaderMaasType 区分推理通道：带 benefit 走每日免费额度，不带则按订阅积分计费。
	HeaderMaasType = "maas_type"
	MaasBenefit    = "benefit"
	EpLoginTicket  = "/v1/login/ticket"
	EpOAuthTokens  = "/v1/oauth2/tokens"
)

// clampTokenFields 把请求体里超限的输出上限夹到上游硬顶，返回是否改过。
//
// 目录报出的上限可以夹，但客户端（如 auto-compact 续传）会自己带更大的值，
// 上游对 max_tokens 的取值范围做平台级预校验，超限整条请求回
// InferHub.001001005.400。这里只做最小改写：能解析成数字且确实超限才回写，
// 其余情况原样放行，不碰请求体的其它字段。
func clampTokenFields(payload map[string]json.RawMessage) bool {
	changed := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		raw, okRaw := payload[key]
		if !okRaw {
			continue
		}
		value, okNumber := numericValue(raw)
		if !okNumber || value <= maxOutputTokenCap {
			continue
		}
		payload[key] = json.RawMessage(strconv.Itoa(maxOutputTokenCap))
		changed = true
	}
	return changed
}

// numericValue 读取 JSON 数字（含 131072.0）或数字字符串（常见于客户端配置）。
// 布尔、对象、非数字字符串一律视为「不碰」。
func numericValue(raw json.RawMessage) (int64, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, false
	}
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return int64(number), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}
