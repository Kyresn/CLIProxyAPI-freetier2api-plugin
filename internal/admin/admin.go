// Package admin 实现插件的管理接口：路由分发、响应封装与资源页。
//
// 领域数据通过 Deps 注入：管理接口属于 ABI 层，但不应反向依赖 package main。
// 这样账号/模型/额度/设置的处理逻辑可以与插件入口分离，也便于单测。
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

const (
	// requestTimeout 是管理接口里上游调用的上限。
	requestTimeout = 3 * time.Minute
	// batchConcurrency 是批量操作的默认并发度上限。
	// 额度查询按账号并发，6 路可以避免排在末尾的账号（如 Trae）拖慢整轮。
	batchConcurrency = 6
)

// Identity 是插件的对外身份。
type Identity struct {
	ID          string
	DisplayName string
	ProviderKey string
	Version     string
}

// Deps 是管理接口依赖的根层能力。
type Deps interface {
	Identity() Identity
	Config() PluginConfig
	State() state.State
	MutateState(func(*state.State))
	Model() *Catalog
	RefreshModels(ctx context.Context) (int, error)
	// SendLoginSMS 发送短信验证码（vendorID 定位实现 SMSLoginProvider 的实例）。
	SendLoginSMS(ctx context.Context, vendorID, phone string) (sessionKey string, err error)
	// VerifyLoginSMS 用短信验证码完成登录并返回落盘凭证。
	VerifyLoginSMS(ctx context.Context, vendorID, sessionKey, phone, code string) (fileName, label, storageJSON string, err error)
	// SubmitLoginCallback 处理用户粘贴回来的 localhost 授权回调链接。
	SubmitLoginCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error)
	// StartLogin 发起一次供应商登录（控制台在页面内完成人机验证后调用）。
	StartLogin(ctx context.Context, vendorID string, metadata map[string]any) (*pluginapi.AuthLoginStartResponse, error)
	// LoginCaptchaConfig 取登录前的人机验证控件参数。
	LoginCaptchaConfig(ctx context.Context, vendorID string) (core.CaptchaConfig, error)
	Reconfigure() error
}

// Service 是管理接口的实例。
type Service struct {
	deps     Deps
	registry *core.Registry
	pool     *auth.Pool
	tasks    *tasks.Service

	quotaMu    sync.RWMutex
	quotaCache map[string]quotaResult

	// quotaRunMu 保护 quotaRun：非 nil 表示有一轮额度查询正在后台执行。
	// 页面刷新会取消自己的管理 RPC，但后台这一轮要照常跑完（见 quota.go）。
	quotaRunMu sync.Mutex
	quotaRun   chan struct{}
}

// New 构造管理 Service。
func New(deps Deps, registry *core.Registry, pool *auth.Pool, taskService *tasks.Service) *Service {
	return &Service{
		deps: deps, registry: registry, pool: pool, tasks: taskService,
		quotaCache: map[string]quotaResult{},
	}
}

// Registration 返回管理路由与资源页声明。
func (s *Service) Registration() pluginapi.ManagementRegistrationResponse {
	id := s.deps.Identity()
	prefix := "/plugins/" + id.ID
	return pluginapi.ManagementRegistrationResponse{
		Routes: s.managementRoutes(prefix),
		Resources: []pluginapi.ResourceRoute{{
			Path:        "/console",
			Menu:        id.DisplayName,
			Description: "FreeTier 账号、任务与额度管理页。",
		}},
	}
}

func (s *Service) managementRoutes(prefix string) []pluginapi.ManagementRoute {
	routes := []pluginapi.ManagementRoute{
		{Method: "GET", Path: prefix + "/status", Description: "账号、额度与任务状态概览。"},
		{Method: "GET", Path: prefix + "/vendors", Description: "列出已启用的供应商（供添加账号下拉与分组展示）。"},
		{Method: "POST", Path: prefix + "/checkin", Description: "对指定或全部账号执行签到。"},
		{Method: "POST", Path: prefix + "/quotas", Description: "批量查询账号额度。"},
		{Method: "POST", Path: prefix + "/accounts/delete", Description: "删除指定账号的凭证文件（宿主监听 auth 目录，会自动注销该账号）。"},
		{Method: "POST", Path: prefix + "/accounts/add", Description: "添加 API Key 类账号。"},
		{Method: "POST", Path: prefix + "/accounts/alias", Description: "给账号设置/清除自定义展示别名（别名优先于上游昵称显示）。"},
		{Method: "GET", Path: prefix + "/models", Description: "读取当前注册的模型清单。"},
		{Method: "POST", Path: prefix + "/models/toggle", Description: "批量禁用/启用模型。"},
		{Method: "POST", Path: prefix + "/models/alias", Description: "给模型设置/清除对外别名（跨供应商同名可合并路由）。"},
		{Method: "POST", Path: prefix + "/models/refresh", Description: "从上游拉取模型清单并缓存。"},
		{Method: "GET", Path: prefix + "/logs", Description: "读取插件日志（增量）。"},
		{Method: "POST", Path: prefix + "/settings", Description: "更新插件运行期设置（自动签到、任务排程、提示词）。"},
		{Method: "POST", Path: prefix + "/tasks/scan", Description: "扫描全部账号的待办任务。"},
		{Method: "POST", Path: prefix + "/tasks/run", Description: "执行任务队列（账号内串行、账号间并发）。"},
		{Method: "GET", Path: prefix + "/tasks/queue", Description: "查询任务队列进度。"},
		{Method: "POST", Path: prefix + "/tasks/auto", Description: "对单个账号执行单个任务或一键完成。"},
		{Method: "POST", Path: prefix + "/tasks/auto_all", Description: "对全部账号依次执行一键完成任务。"},
		{Method: "GET", Path: prefix + "/school/status", Description: "开学季活动状态。"},
		{Method: "POST", Path: prefix + "/keepalive", Description: "手动执行 token 保活。"},
		{Method: "POST", Path: prefix + "/plans/preview", Description: "查询账号当前可领套餐与人机验证控件参数。"},
		{Method: "POST", Path: prefix + "/activities/preview", Description: "只读探测账号当前活动资格，不执行领取。"},
		{Method: "POST", Path: prefix + "/plans/claim", Description: "提交人机验证令牌并领取套餐。"},
		{Method: "POST", Path: prefix + "/login/callback", Description: "提交用户粘贴的 localhost 授权回调链接（远程部署下的登录兜底）。"},
		{Method: "POST", Path: prefix + "/login/start", Description: "发起一次供应商登录（可携带页面内人机验证令牌）。"},
		{Method: "GET", Path: prefix + "/login/captcha-config", Description: "读取登录前的人机验证控件参数（阿里云场景 ID/region/prefix）。"},
		{Method: "POST", Path: prefix + "/login/sms/send", Description: "发送短信验证码（支持手机验证码登录的供应商）。"},
		{Method: "POST", Path: prefix + "/login/sms/verify", Description: "用短信验证码完成登录并保存账号。"},
	}
	for _, spec := range s.tasks.Catalog().ManualSpecs() {
		if spec.Route == "" {
			continue
		}
		routes = append(routes, pluginapi.ManagementRoute{Method: "POST", Path: prefix + spec.Route, Description: spec.Description})
	}
	return routes
}

// Handle 是管理接口的总分发。
func (s *Service) Handle(request []byte) ([]byte, error) {
	if s.deps == nil {
		return nil, core.NewPluginError("admin_not_configured", "management deps are not configured", http.StatusInternalServerError)
	}
	var rpc pluginapi.ManagementRequest
	if len(request) > 0 {
		if errDecode := json.Unmarshal(request, &rpc); errDecode != nil {
			return nil, core.NewPluginError("invalid_request", "decode request: "+errDecode.Error(), http.StatusBadRequest)
		}
	}
	method := strings.ToUpper(strings.TrimSpace(rpc.Method))
	if method == "" {
		method = http.MethodGet
	}
	id := s.deps.Identity()

	switch {
	case method == http.MethodGet && matchesResourcePath(rpc.Path, id.ID, "/console"):
		return core.OKEnvelope(HTMLResponse(http.StatusOK, []byte(Page("/plugins/"+id.ID))))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/status"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, s.buildStatusPayload(rpc)))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/vendors"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, s.buildVendorsPayload()))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/logs"):
		return core.OKEnvelope(JSONResponse(http.StatusOK, s.buildLogsPayload(rpc)))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/checkin"):
		return core.OKEnvelope(s.handleCheckinRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/quotas"):
		return core.OKEnvelope(s.handleQuotasRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/delete"):
		return core.OKEnvelope(s.handleAccountDeleteRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/add"):
		return core.OKEnvelope(s.handleAccountAddRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/accounts/alias"):
		return core.OKEnvelope(s.handleAccountAliasRequest(rpc))

	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/models"):
		return core.OKEnvelope(s.handleModelsList(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/toggle"):
		return core.OKEnvelope(s.modelToggleHandler(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/alias"):
		return core.OKEnvelope(s.modelAliasHandler(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/models/refresh"):
		ctx, cancel := hostbridge.ManagementContext(rpc, requestTimeout)
		defer cancel()
		count, errRefresh := s.deps.RefreshModels(ctx)
		if errRefresh != nil {
			return core.OKEnvelope(ErrorResponse(errRefresh))
		}
		return core.OKEnvelope(JSONResponse(http.StatusOK, map[string]any{"ok": true, "models": count}))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/settings"):
		return core.OKEnvelope(s.handleSettingsRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/keepalive"):
		return core.OKEnvelope(s.handleKeepaliveRequest(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/tasks/scan"):
		return core.OKEnvelope(s.handleTasksScan(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/tasks/run"):
		return core.OKEnvelope(s.handleTasksRun(rpc))
	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/tasks/queue"):
		return core.OKEnvelope(s.handleTasksQueue(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/tasks/auto"):
		return core.OKEnvelope(s.handleTasksAuto(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/tasks/auto_all"):
		return core.OKEnvelope(s.handleTasksAutoAll(rpc))
	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/school/status"):
		return core.OKEnvelope(s.handleSchoolStatus(rpc))

	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/plans/preview"):
		return core.OKEnvelope(s.handlePlansPreview(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/activities/preview"):
		return core.OKEnvelope(s.handleActivitiesPreview(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/plans/claim"):
		return core.OKEnvelope(s.handlePlansClaim(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/callback"):
		return core.OKEnvelope(s.handleLoginCallbackRequest(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/start"):
		return core.OKEnvelope(s.handleLoginStartRequest(rpc))
	case method == http.MethodGet && matchesManagementPath(rpc.Path, id.ID, "/login/captcha-config"):
		return core.OKEnvelope(s.handleLoginCaptchaConfigRequest(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/sms/send"):
		return core.OKEnvelope(s.handleLoginSMSSend(rpc))
	case method == http.MethodPost && matchesManagementPath(rpc.Path, id.ID, "/login/sms/verify"):
		return core.OKEnvelope(s.handleLoginSMSVerify(rpc))
	}
	if method == http.MethodPost {
		if suffix, okSuffix := managementSuffix(rpc.Path, id.ID); okSuffix {
			for _, spec := range s.tasks.Catalog().ManualSpecs() {
				if spec.Route != "" && suffix == spec.Route {
					return core.OKEnvelope(s.handleScheduledRun(rpc, spec.Kind))
				}
			}
		}
	}

	return core.OKEnvelope(JSONResponse(http.StatusNotFound, map[string]any{
		"error": "not found", "path": rpc.Path, "method": method,
	}))
}

// matchesManagementPath 判断路径是否命中某个管理子路径。
func matchesManagementPath(path, pluginID, suffix string) bool {
	normalized := normalizeRequestPath(path)
	prefix := "/plugins/" + pluginID
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == prefix+suffix || normalized == "/v0/management"+prefix+suffix
}

func matchesResourcePath(path, pluginID, suffix string) bool {
	normalized := normalizeRequestPath(path)
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return normalized == "/v0/resource/plugins/"+pluginID+suffix
}

// managementSuffix 返回去掉前缀后的子路径。
func managementSuffix(path, pluginID string) (string, bool) {
	normalized := normalizeRequestPath(path)
	for _, prefix := range []string{"/plugins/" + pluginID, "/v0/management/plugins/" + pluginID} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimPrefix(normalized, prefix), true
		}
	}
	return "", false
}

func normalizeRequestPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// HTMLResponse 构造 HTML 管理响应。
func HTMLResponse(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"text/html; charset=utf-8"},
			// 页面与后端字段名一起演进：不允许浏览器拿旧页面去提交已改名的键。
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}

// decodeRequest 解码管理请求体。空请求体保持兼容并视为未提供参数（返回零值）；
// 非空请求体只要不是合法 JSON 就返回错误，由调用方统一回 400，
// 避免畸形请求继续按默认值执行。
func decodeRequest[T any](raw []byte) (T, error) {
	var body T
	return decodeRequestInto(raw, body)
}

// decodeRequestInto 与 decodeRequest 相同，但以一个非零默认值起步：
// 只出现在请求体里的字段才覆盖默认值，缺省字段保留 base。
func decodeRequestInto[T any](raw []byte, base T) (T, error) {
	if len(raw) == 0 {
		return base, nil
	}
	if errUnmarshal := json.Unmarshal(raw, &base); errUnmarshal != nil {
		var zero T
		return zero, errUnmarshal
	}
	return base, nil
}

// BadJSONRequestResponse 构造统一的畸形 JSON 请求响应。
//
// login/callback 是唯一例外：页面按 AuthLoginStatus 契约读 status / message，
// 那边自己构造同样形状的 400，不套用这里。
func BadJSONRequestResponse() pluginapi.ManagementResponse {
	return JSONResponse(http.StatusBadRequest, map[string]any{
		"ok": false, "error": "请求体解析失败，请检查 JSON 格式",
	})
}

// JSONResponse 构造 JSON 管理响应。
func JSONResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		raw = []byte(`{"error":"marshal failed"}`)
		status = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

// ErrorResponse 把内部错误转成带状态码的 JSON 响应。
func ErrorResponse(err error) pluginapi.ManagementResponse {
	status := http.StatusInternalServerError
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	if pluginErr, okPlugin := err.(*core.PluginError); okPlugin {
		message = pluginErr.Message
		if pluginErr.HTTPStatus > 0 {
			status = pluginErr.HTTPStatus
		}
	}
	return JSONResponse(status, map[string]any{"error": message, "ok": false})
}

// queryString 取第一个非空查询参数（缺失返回空串）。
func queryString(query map[string][]string, key string) string {
	if query == nil {
		return ""
	}
	for _, value := range query[key] {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func queryInt64(query map[string][]string, key string, fallback int64) int64 {
	if query == nil {
		return fallback
	}
	values, okValues := query[key]
	if !okValues || len(values) == 0 {
		return fallback
	}
	parsed, errParse := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
	if errParse != nil {
		return fallback
	}
	return parsed
}

// 供子文件使用的共享助手。

// hostActiveVendors 返回有凭证的供应商集合。
func (s *Service) hostActiveVendors(ctx context.Context, callbackID string) map[string]bool {
	return hostbridge.ActiveVendorSet(ctx, callbackID, s.registry)
}
