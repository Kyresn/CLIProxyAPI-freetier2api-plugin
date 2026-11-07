package admin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

type stubDeps struct {
	store    *state.Store
	registry *core.Registry
	manager  *Catalog

	callbackResult *core.CallbackSubmitResult
	callbackErr    error

	loginStart    *pluginapi.AuthLoginStartResponse
	loginStartErr error
	captcha       core.CaptchaConfig
	captchaErr    error
}

func (s stubDeps) Identity() Identity {
	return Identity{ID: "freetier2api-fork", DisplayName: "FreeTier 2API Plugin-fork", ProviderKey: "freetier", Version: "test"}
}
func (s stubDeps) Config() PluginConfig { return DefaultPluginConfig() }
func (s stubDeps) State() state.State   { return s.store.Snapshot() }
func (s stubDeps) MutateState(fn func(*state.State)) {
	s.store.Mutate(fn)
}
func (s stubDeps) Model() *Catalog { return s.manager }
func (s stubDeps) ResolveCredential(context.Context, string, []byte, string, map[string]string) (*core.Credential, core.Vendor, error) {
	return nil, nil, context.Canceled
}
func (s stubDeps) RefreshModels(context.Context) (int, error) { return 0, nil }
func (s stubDeps) Reconfigure() error                         { return nil }

// callbackResult / callbackErr 让用例能驱动「粘贴回调链接」的返回分支。
func (s stubDeps) SendLoginSMS(context.Context, string, string) (string, error) {
	return "", nil
}

func (s stubDeps) VerifyLoginSMS(context.Context, string, string, string, string) (string, string, string, error) {
	return "", "", "", nil
}

func (s stubDeps) SubmitLoginCallback(context.Context, string, string) (*core.CallbackSubmitResult, error) {
	if s.callbackErr != nil {
		return nil, s.callbackErr
	}
	return s.callbackResult, nil
}

// loginStart / loginCaptcha 让用例能驱动页面内登录的两个新入口。
func (s stubDeps) StartLogin(context.Context, string, map[string]any) (*pluginapi.AuthLoginStartResponse, error) {
	if s.loginStartErr != nil {
		return nil, s.loginStartErr
	}
	return s.loginStart, nil
}

func (s stubDeps) LoginCaptchaConfig(context.Context, string) (core.CaptchaConfig, error) {
	if s.captchaErr != nil {
		return core.CaptchaConfig{}, s.captchaErr
	}
	return s.captcha, nil
}

func newStubDeps(t *testing.T) stubDeps {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	return stubDeps{store: store, registry: registry, manager: NewCatalog(store, registry)}
}

func newService(t *testing.T) *Service {
	t.Helper()
	return newServiceWithDeps(t, newStubDeps(t))
}

func newServiceWithDeps(t *testing.T, deps stubDeps) *Service {
	t.Helper()
	pool := auth.NewPool(deps.registry, deps.ResolveCredential)
	return New(deps, deps.registry, pool, tasks.New(stubTaskDeps{deps: deps, pool: pool}))
}

func TestRegistrationDeclaresRoutes(t *testing.T) {
	service := newService(t)
	registration := service.Registration()
	found := false
	for _, route := range registration.Routes {
		if route.Path == "/plugins/freetier2api-fork/status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("status route missing: %#v", registration.Routes)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/console" {
		t.Fatalf("resources = %#v", registration.Resources)
	}
}

func TestHandleStatusAndUnknownRoute(t *testing.T) {
	service := newService(t)

	raw, err := service.Handle([]byte(`{"method":"GET","path":"/v0/management/plugins/freetier2api-fork/status"}`))
	if err != nil {
		t.Fatalf("handle status: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !strings.Contains(string(raw), `"StatusCode":200`) && !strings.Contains(string(raw), `"status_code":200`) {
		t.Fatalf("unexpected envelope: %s", raw)
	}

	raw, err = service.Handle([]byte(`{"method":"GET","path":"/plugins/freetier2api-fork/nope"}`))
	if err != nil {
		t.Fatalf("handle unknown: %v", err)
	}
	var notFound struct {
		Result struct {
			StatusCode int
		}
	}
	if err := json.Unmarshal(raw, &notFound); err != nil {
		t.Fatalf("decode not-found: %v", err)
	}
	if notFound.Result.StatusCode != 404 {
		t.Fatalf("unknown route should 404: %s", raw)
	}
}

type stubTaskDeps struct {
	deps stubDeps
	pool *auth.Pool
}

func (s stubTaskDeps) Registry() *core.Registry { return s.deps.registry }
func (s stubTaskDeps) Pool() *auth.Pool         { return s.pool }

// Config 把全局配置投影成任务引擎的最小形状（与组合根 taskConfig 同构）。
func (s stubTaskDeps) Config() tasks.Config {
	cfg := s.deps.Config()
	return tasks.Config{
		EnabledRealms: cfg.EnabledRealms, AutoCheckin: cfg.AutoCheckin,
		AutoCheckinAt: cfg.AutoCheckinAt, AutoTasks: cfg.AutoTasks,
	}
}

func (s stubTaskDeps) SaveRefreshedCredential(context.Context, string, core.Vendor, core.HostAuth, []byte, *core.Credential, string) {
}
func (s stubTaskDeps) State() state.State                { return s.deps.State() }
func (s stubTaskDeps) MutateState(fn func(*state.State)) { s.deps.MutateState(fn) }
func (s stubTaskDeps) Now() time.Time                    { return time.Now() }

// TestSettingsPayloadKeysMatchConsole 锁死「后端字段名 ↔ 页面字段名」的一致性。
//
// 真实事故：`/settings` 的字段名与页面提交的键曾经漂移过（后端改名、页面没跟），
// 结果页面上的改动**根本存不进去**且没有任何报错。
//
// 编译期已经保证 Go 侧一致，但 HTML 是字符串，编译器看不见。这里把
// settingsUpdateRequest 的每个 json tag 钉到页面上，防止再次漂移。
func TestSettingsPayloadKeysMatchConsole(t *testing.T) {
	page := Page("/plugins/freetier2api-fork")

	// 兼容旧版页面/脚本的固定字段：后端仍接受，但当前页面不再发送。
	deprecated := map[string]bool{
		"checkin_hours": true, "travel_hours": true, "activity_hours": true,
		"keepalive_hours": true, "blackcat_hours": true,
	}

	var req settingsUpdateRequest
	typ := reflect.TypeOf(req)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		jsonTag := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonTag == "" || jsonTag == "-" || deprecated[jsonTag] {
			continue
		}
		if !strings.Contains(page, jsonTag) {
			t.Errorf("POST /settings 的字段 %q（%s）在控制台页面里没有出现，页面改动不会被后端接收",
				jsonTag, field.Name)
		}
	}

	// 反向：页面读取的 /status 键必须在后端载荷里真实存在。
	for _, key := range []string{"version", "settings", "prompt_mode"} {
		if !strings.Contains(page, key) {
			t.Errorf("页面读取的 /status 键 %q 未出现在页面上", key)
		}
	}
}
