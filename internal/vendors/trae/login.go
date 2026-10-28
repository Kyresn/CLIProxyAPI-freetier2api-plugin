package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// 本文件实现 Trae 家族授权登录链接生成与轮询状态检查。

// BuildLoginURL 构造某个 profile 的授权登录地址。
func BuildLoginURL(cfg Config, machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", "2.3.62834")
	v.Set("auth_type", "local")
	v.Set("client_id", cfg.ClientID)
	v.Set("redirect", "0")

	traceID := MachineTraceID(machineID, deviceID)

	return cfg.ConsoleHost + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(traceID) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(cfg.IDEVersion) +
		"&x_app_type=stable"
}

// LoginStart 启动登录流程。
func LoginStart(ctx context.Context, cfg Config, store *SessionStore) (*pluginapi.AuthLoginStartResponse, error) {
	if errListen := store.EnsureCallbackServer(); errListen != nil {
		// 监听起不来时不要继续：用户点完授权，回调没有进程接收。
		return nil, core.NewPluginError("trae_callback_unavailable",
			"无法在本机启动授权回调监听: "+errListen.Error(), http.StatusInternalServerError)
	}

	machineID := core.RandomHex(16)
	deviceID := core.RandomHex(16)
	traceID := MachineTraceID(machineID, deviceID)
	callbackURL := store.callbackURL()

	loginURL := BuildLoginURL(cfg, machineID, deviceID, callbackURL)
	sessionID := cfg.VendorID + "_" + core.RandomHex(8)

	store.register(&PendingSession{
		SessionID: sessionID,
		VendorID:  cfg.VendorID,
		Region:    cfg.Region,
		MachineID: machineID,
		DeviceID:  deviceID,
		TraceID:   traceID,
		State:     "pending",
		CreatedAt: time.Now(),
	})

	return &pluginapi.AuthLoginStartResponse{
		Provider:  "freetier",
		URL:       loginURL,
		State:     sessionID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}, nil
}

// LoginPoll 轮询状态。
func LoginPoll(ctx context.Context, state string, store *SessionStore) (*pluginapi.AuthLoginPollResponse, error) {
	session, ok := store.get(state)
	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "登录会话不存在或已过期，请重新发起登录并在浏览器授权后立即粘贴回调链接",
		}, nil
	}

	switch session.State {
	case "success":
		store.remove(state)
		if session.AuthData != nil {
			return &pluginapi.AuthLoginPollResponse{
				Status:  pluginapi.AuthLoginStatusSuccess,
				Message: "登录成功",
				Auth:    *session.AuthData,
			}, nil
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "登录成功",
		}, nil
	case "failed":
		store.remove(state)
		errMsg := session.ErrMsg
		if errMsg == "" {
			errMsg = "授权登录失败"
		}
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errMsg,
		}, nil
	default:
		return &pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "正在等待用户在浏览器中授权...",
		}, nil
	}
}

// PendingSession 记录一次进行中的 Trae 登录。
type PendingSession struct {
	SessionID string
	VendorID  string // traecn / traeglobal
	Region    Region
	MachineID string
	DeviceID  string
	TraceID   string
	State     string // "pending", "success", "failed"
	AuthData  *pluginapi.AuthData
	ErrMsg    string
	CreatedAt time.Time
}

// 一次登录的表单与监听全部属于 SessionStore（= 一个 App 实例），
// 不放在包级变量里：同进程多个 App 并存时，一个实例发起登录不能
// 影响另一个实例的会话，也不能让它们的回调互相认领。

// SessionStore 是单个 App 实例的 Trae 登录会话表与回调监听。
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*PendingSession

	// serverMu 保护本实例的监听生命周期（懒启动，失败后允许重试）。
	serverMu   sync.Mutex
	listening  bool
	serverAddr string
}

// NewSessionStore 构造会话表。
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]*PendingSession{}}
}

func (s *SessionStore) register(p *PendingSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.sessions[p.SessionID] = p
}

func (s *SessionStore) get(sessionID string) (*PendingSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if time.Since(session.CreatedAt) > 15*time.Minute {
		delete(s.sessions, sessionID)
		return nil, false
	}
	return session, true
}

func (s *SessionStore) remove(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *SessionStore) owns(sessionID, vendorID string) bool {
	norm := strings.ToLower(strings.TrimSpace(sessionID))
	switch strings.ToLower(strings.TrimSpace(vendorID)) {
	case "traecn", "trae-cn":
		if strings.HasPrefix(norm, "traecn_") || strings.HasPrefix(norm, "trae_cn_") {
			return true
		}
	case "traeglobal", "trae-global":
		if strings.HasPrefix(norm, "traeglobal_") || strings.HasPrefix(norm, "trae_global_") {
			return true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return false
	}
	if time.Since(session.CreatedAt) > 15*time.Minute {
		delete(s.sessions, sessionID)
		return false
	}
	return strings.EqualFold(session.VendorID, vendorID)
}

// matchExact 只按 trace/machine 精确匹配，找不到时返回 nil。
func (s *SessionStore) matchExact(info *CallbackInfo) *PendingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		if session.State != "pending" {
			continue
		}
		if (info.TraceID != "" && session.TraceID == info.TraceID) ||
			(info.MachineID != "" && session.MachineID == info.MachineID) {
			return session
		}
	}
	return nil
}

// latestPending 返回最近创建的待处理会话（回调里没带可匹配标识时的兜底）。
func (s *SessionStore) latestPending() *PendingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *PendingSession
	for _, session := range s.sessions {
		if session.State == "pending" && time.Since(session.CreatedAt) <= 15*time.Minute {
			if latest == nil || session.CreatedAt.After(latest.CreatedAt) {
				latest = session
			}
		}
	}
	return latest
}

func (s *SessionStore) gcLocked() {
	now := time.Now()
	for id, session := range s.sessions {
		if now.Sub(session.CreatedAt) > 15*time.Minute {
			delete(s.sessions, id)
		}
	}
}

// matchCallback 先按 trace/machine 精确匹配，再退回归属最新的待处理会话。
//
// 只在**本实例**的会话表里查找：另一个 App 的登录会话不属于这里。
func (s *SessionStore) matchCallback(info *CallbackInfo) *PendingSession {
	if matched := s.matchExact(info); matched != nil {
		return matched
	}
	return s.latestPending()
}

// EnsureCallbackServer 保证本实例的授权回调监听已启动。
//
// 端口优先用 127.0.0.1:18080（与 Trae 桌面端回调地址一致）；被占用时
// 退回系统分配的空闲端口，并把实际端口写进登录链接——否则回调永远
// 不会到达本进程。监听生命周期随实例，不与其他 App 共享。
func (s *SessionStore) EnsureCallbackServer() error {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if s.listening {
		return nil
	}

	listener, errListen := net.Listen("tcp", "127.0.0.1:18080")
	if errListen != nil {
		logger.Debug("trae: 18080 已被占用，改用随机端口: %v", errListen)
		listener, errListen = net.Listen("tcp", "127.0.0.1:0")
		if errListen != nil {
			return fmt.Errorf("listen callback: %w", errListen)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", s.handleAuthorizeCallback)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.serverAddr = "http://" + listener.Addr().String()
	s.listening = true
	go func() {
		logger.Info("trae: 本地授权回调监听已启动: %s/authorize", s.serverAddr)
		if errServe := srv.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			logger.Error("trae: 回调服务退出: %v", errServe)
		}
	}()
	return nil
}

// callbackURL 返回本实例的回调地址（EnsureCallbackServer 之后有效）。
func (s *SessionStore) callbackURL() string {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	return s.serverAddr + "/authorize"
}

// callbackHint 返回页面应当展示的回调地址：监听没起来时给默认端口，
// 起来了就给真实端口（18080 被占用时是随机端口，提示必须跟着变）。
func (s *SessionStore) callbackHint() string {
	if addr := s.callbackURL(); addr != "" {
		return addr
	}
	return "http://127.0.0.1:18080/authorize"
}

// MachineTraceID 从 machineID+deviceID 派生 16 位 trace ID。
func MachineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= 16 {
		return h[len(h)-16:]
	}
	return strings.Repeat("0", 16-len(h)) + h
}

// CallbackInfo 回调解析结果。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    int64
	TraceID      string
	MachineID    string
	DeviceID     string
}

// ParseCallback 解析 /authorize 接收到的链接。
func ParseCallback(rawURL string) (*CallbackInfo, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("empty callback url")
	}
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = "http://127.0.0.1" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse callback url: %w", err)
	}
	q := u.Query()
	info := &CallbackInfo{
		RefreshToken: q.Get("refreshToken"),
		TraceID:      q.Get("loginTraceID"),
		MachineID:    q.Get("machine_id"),
		DeviceID:     q.Get("device_id"),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	info.UID = core.GetString(userInfo, "UserID")
	info.Nickname = core.GetString(userInfo, "ScreenName")
	info.EnterpriseID = core.GetString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := core.GetString(userJwt, "Token")
	jwtRefresh := core.GetString(userJwt, "RefreshToken")

	if info.RefreshToken == "" {
		info.RefreshToken = jwtRefresh
	}
	if info.RefreshToken == "" {
		info.AccessToken = jwtToken
		if exp := getInt64Param(userJwt, "TokenExpireAt"); exp > 0 {
			if exp > 1e12 {
				exp /= 1000
			}
			info.ExpiresAt = exp
		}
	}

	if info.RefreshToken == "" && info.AccessToken == "" {
		return nil, fmt.Errorf("callback missing refreshToken and userJwt.Token")
	}

	return info, nil
}

func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getInt64Param(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func (s *SessionStore) handleAuthorizeCallback(w http.ResponseWriter, r *http.Request) {
	info, errParse := ParseCallback(r.URL.String())
	if errParse != nil {
		renderCallbackHTML(w, http.StatusBadRequest, "授权回调解析失败", errParse.Error())
		return
	}

	matched := s.matchCallback(info)

	if matched == nil {
		renderCallbackHTML(w, http.StatusNotFound, "未找到对应的登录会话", "登录会话可能已过期或已被处理，请返回控制台重新发起登录。")
		return
	}

	if _, errComplete := s.completeLogin(r.Context(), matched, info); errComplete != nil {
		renderCallbackHTML(w, http.StatusInternalServerError, "授权回调处理失败", errComplete.Error())
		return
	}

	renderCallbackHTML(w, http.StatusOK, "授权成功", "账号已成功授权！现在可以关闭此窗口返回管理面板。")
}

// completeLogin 用回调里带出的凭证完成登录：换 access token、拼凭证、置会话成功。
//
// HTTP 回调与控制台「粘贴回调链接」共用这一段——两条路径只差「怎么拿到回调
// 链接」，完成逻辑必须一致，否则同一次登录会因为入口不同产出不同凭证。
func (s *SessionStore) completeLogin(ctx context.Context, matched *PendingSession, info *CallbackInfo) (*pluginapi.AuthData, error) {
	// 执行 Token 换取：host/clientID 一律按实例 profile 取（国内版为
	// api.trae.cn + en1oxy7wnw8j9n，国际版为 growsg-normal.trae.ai + ono9krqynydwx5）。
	cfg, okConfig := ConfigForVendor(matched.VendorID)
	if !okConfig {
		cfg = ConfigFor(matched.Region)
	}
	authHost := cfg.AuthHost
	clientID := cfg.ClientID

	accessToken := info.AccessToken
	refreshToken := info.RefreshToken
	expiresAt := info.ExpiresAt

	// uid 要在换 token 之前算好：上游的换 token 接口要求带 UserID。
	uid := info.UID
	if uid == "" {
		uid = "user_" + matched.MachineID[:min(8, len(matched.MachineID))]
	}

	if refreshToken != "" {
		newAccess, newRefresh, exp, errEx := exchangeTraeToken(ctx, authHost, clientID, uid, refreshToken)
		if errEx != nil {
			logger.Error("trae: callback exchange token warning: %v, falling back to original", errEx)
		} else {
			accessToken = newAccess
			if newRefresh != "" {
				refreshToken = newRefresh
			}
			expiresAt = exp
		}
	}

	nickname := info.Nickname
	if nickname == "" {
		nickname = uid
	}

	label := "Trae-" + nickname
	if matched.Region == RegionGlobal {
		label = "TraeGlobal-" + nickname
	}

	storageMap := map[string]any{
		"type":          "freetier",
		"vendor":        matched.VendorID,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"expires_at":    expiresAt,
		"uid":           uid,
		"nickname":      nickname,
		"enterprise_id": info.EnterpriseID,
		"machine_id":    matched.MachineID,
		"device_id":     matched.DeviceID,
		"label":         label,
		"region":        string(matched.Region),
	}

	rawStorage, errJSON := json.MarshalIndent(storageMap, "", "  ")
	if errJSON != nil {
		matched.State = "failed"
		matched.ErrMsg = "生成凭证失败"
		return nil, fmt.Errorf("生成凭证失败: %w", errJSON)
	}

	fileName := matched.VendorID + "-" + uid + ".json"
	authData := &pluginapi.AuthData{
		Provider:    "freetier",
		ID:          matched.VendorID + "-" + uid,
		FileName:    fileName,
		Label:       label,
		StorageJSON: rawStorage,
	}
	matched.AuthData = authData
	matched.State = "success"

	logger.Info("trae: 授权成功，账号=%s (%s), vendor=%s", uid, nickname, matched.VendorID)
	return authData, nil
}

// SubmitCallback 接受用户粘贴的 localhost 回调链接。
//
// 上游只认 127.0.0.1 回调，插件与浏览器不在同一台机器时那条 HTTP 回调永远
// 到不了；控制台把浏览器停住的地址原样送回来，这里就地解析完成登录。
//
// 与 HTTP 回调不同，粘贴**只按 loginTraceID / machine_id 精确认领**：宁可报
// 不匹配，也不能把一条链接认到别的会话上（HTTP 回调还有 latestPending 兜底，
// 那是同机场景的便利；粘贴必须严格）。
func (s *SessionStore) SubmitCallback(ctx context.Context, state, rawURL string) (*core.CallbackSubmitResult, error) {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return nil, core.NewPluginError("invalid_request", "缺少登录会话 state，请重新发起登录", http.StatusBadRequest)
	}

	session, ok := s.get(trimmed)
	if !ok {
		return nil, core.NewPluginError("session_not_found",
			"登录会话不存在或已过期，请重新发起登录并在浏览器授权后立即粘贴回调链接", http.StatusNotFound)
	}
	if session.State == "success" && session.AuthData != nil {
		return &core.CallbackSubmitResult{
			Status:  pluginapi.AuthLoginStatusSuccess,
			Message: "登录成功",
			Auth:    *session.AuthData,
		}, nil
	}
	if session.State != "pending" {
		msg := session.ErrMsg
		if msg == "" {
			msg = "授权登录失败"
		}
		return &core.CallbackSubmitResult{Status: pluginapi.AuthLoginStatusError, Message: msg}, nil
	}

	// 解析失败时不回显原始链接：里面带 refreshToken / userJwt。
	info, errParse := ParseCallback(rawURL)
	if errParse != nil {
		return nil, core.NewPluginError("callback_parse_failed",
			"回调链接解析失败，请确认粘贴的是浏览器地址栏里的完整链接（含 http:// 与后面的参数）",
			http.StatusBadRequest)
	}

	matched := s.matchExact(info)
	if matched == nil || matched.SessionID != session.SessionID {
		return nil, core.NewPluginError("callback_mismatch",
			"回调链接与当前登录会话不匹配，请重新发起登录后重新授权", http.StatusBadRequest)
	}

	authData, errComplete := s.completeLogin(ctx, matched, info)
	if errComplete != nil {
		return nil, core.ToPluginError(errComplete)
	}
	return &core.CallbackSubmitResult{
		Status:  pluginapi.AuthLoginStatusSuccess,
		Message: "登录成功",
		Auth:    *authData,
	}, nil
}

// exchangeTraeToken 用 refreshToken 换 accessToken（Trae OAuth ExchangeToken）。
//
// 协议（已联调验证）：POST {host}/cloudide/api/v3/trae/oauth/ExchangeToken，
// PascalCase 载荷，响应取 Result.Token / Result.RefreshToken / Result.TokenExpireAt。
// host/clientID 由调用方按实例 profile 给出（国内版为 api.trae.cn +
// en1oxy7wnw8j9n，国际版为 growsg-normal.trae.ai + ono9krqynydwx5）。
func exchangeTraeToken(ctx context.Context, authHost, clientID, userID, refreshToken string) (string, string, int64, error) {
	payload := map[string]string{
		"ClientID":     clientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       userID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, authHost+EpExchange, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return "", "", 0, errReq
	}
	req.Header.Set("Content-Type", "application/json")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", "", 0, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", 0, fmt.Errorf("upstream exchange error %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Result struct {
			Token               string `json:"Token"`
			RefreshToken        string `json:"RefreshToken"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", 0, err
	}
	if res.Result.Token == "" {
		return "", "", 0, fmt.Errorf("exchange token failed: %s", string(body))
	}

	exp := res.Result.TokenExpireAt
	if exp > 1e12 {
		exp /= 1000
	}
	if exp <= 0 && res.Result.TokenExpireDuration > 0 {
		exp = time.Now().Unix() + res.Result.TokenExpireDuration
	}

	return res.Result.Token, res.Result.RefreshToken, exp, nil
}

func renderCallbackHTML(w http.ResponseWriter, code int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	color := "#3fb950"
	if code >= 400 {
		color = "#f85149"
	}
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; background: #0d1117; color: #c9d1d9; }
    .card { background: #161b22; border: 1px solid #30363d; padding: 36px 40px; border-radius: 12px; box-shadow: 0 8px 24px rgba(0,0,0,0.5); text-align: center; max-width: 440px; }
    h2 { color: %s; margin: 0 0 16px 0; font-size: 24px; }
    p { margin: 8px 0; font-size: 15px; color: #8b949e; line-height: 1.5; }
    .msg { color: #f0f6fc; font-weight: 500; font-size: 16px; margin-bottom: 16px; }
    .tip { color: #6e7681; font-size: 13px; margin-top: 24px; }
  </style>
</head>
<body>
  <div class="card">
    <h2>%s</h2>
    <div class="msg">%s</div>
    <p class="tip">您可以关闭此页面返回管理面板。</p>
  </div>
  <script>
    if (%d === 200) {
      setTimeout(function() { window.close(); }, 3000);
    }
  </script>
</body>
</html>`, title, color, title, message, code)
	_, _ = w.Write([]byte(html))
}

// Token 续期：调用 ExchangeToken 轮换 accessToken 与 refreshToken。

// RefreshCredential 用 refreshToken 轮换 accessToken（Trae OAuth ExchangeToken）。
//
// 协议与 exchangeTraeToken 相同：PascalCase 载荷，响应取 Result.Token。
func RefreshCredential(ctx context.Context, cred *Credential, cfg Config) (*Credential, bool, error) {
	if cred == nil || cred.RefreshToken == "" {
		return cred, false, nil
	}

	payload := map[string]string{
		"ClientID":     cfg.ClientID,
		"RefreshToken": cred.RefreshToken,
		"ClientSecret": "-",
		"UserID":       cred.UID,
	}
	bodyBytes, _ := json.Marshal(payload)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RefreshHost+EpExchange, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return nil, false, errReq
	}
	req.Header.Set("Content-Type", "application/json")

	client := hostbridge.Client(ctx, 30*time.Second)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, false, fmt.Errorf("trae refresh token request failed: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, false, Classify(resp.StatusCode, string(body))
	}

	var result struct {
		Result struct {
			Token               string `json:"Token"`
			RefreshToken        string `json:"RefreshToken"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("decode exchange token response: %w", err)
	}
	if result.Result.Token == "" {
		return nil, false, fmt.Errorf("exchange token failed: %s", string(body))
	}

	expiresAt := result.Result.TokenExpireAt
	if expiresAt > 1e12 {
		expiresAt /= 1000
	}
	if expiresAt <= 0 && result.Result.TokenExpireDuration > 0 {
		expiresAt = time.Now().Unix() + result.Result.TokenExpireDuration
	}

	updated := *cred
	updated.AccessToken = result.Result.Token
	if result.Result.RefreshToken != "" {
		updated.RefreshToken = result.Result.RefreshToken
	}
	updated.ExpiresAt = expiresAt

	return &updated, true, nil
}
func MergeStorageJSON(original []byte, updated *Credential) ([]byte, error) {
	if updated == nil {
		return original, nil
	}
	return core.MergeCredentialJSON(original, updated.StorageJSON, func(m map[string]any) {
		m["access_token"] = updated.AccessToken
		if updated.RefreshToken != "" {
			m["refresh_token"] = updated.RefreshToken
		}
		if updated.ExpiresAt > 0 {
			m["expires_at"] = updated.ExpiresAt
		}
		// 解析允许嵌套的 {auth:{...}} 结构（见 parseCredential），写回必须对称，
		// 否则嵌套凭证续期后只在顶层更新，下次解析仍读到旧 token。
		if authObj, ok := m["auth"].(map[string]any); ok {
			authObj["accessToken"] = updated.AccessToken
			if updated.RefreshToken != "" {
				authObj["refreshToken"] = updated.RefreshToken
			}
			if updated.ExpiresAt > 0 {
				authObj["expiresAt"] = updated.ExpiresAt
			}
		}
	})
}
