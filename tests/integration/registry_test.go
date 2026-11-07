package tests

// 本文件用测试锁死 registry.json 的格式。
//
// registry.json 是用户配置三方源后真正会被读取的文件，手改坏 = 商店里
// 直接看不见插件（而且宿主对此是静默的），因此值得用测试守住。
//
// 宿主 CLIProxyAPI/internal/pluginstore 对三方源清单的校验规则：
//   - 顶层 schema_version 必须等于 1
//   - 每个条目必填 id / name / description / author；github-release 类型还要 repository
//   - id 匹配 ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$
//   - version 可选（宿主从 latest release 推导），有则必须匹配 ^[0-9][0-9A-Za-z.+-]*$
//   - repository 必须是 https://github.com/{owner}/{repo}

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"freetier2api-plugin/internal/admin"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
)

var (
	// unwrapRe 匹配「再剥一层信封」的写法：.result 后面不接 s（区分合法的 .results）。
	unwrapRe             = regexp.MustCompile(`\.result\s*[;,)\n]`)
	pluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	pluginVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
	githubRepositoryRe   = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+$`)
)

// registryManifest 是三方源清单的结构。
type registryManifest struct {
	SchemaVersion int `json:"schema_version"`
	Plugins       []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Author      string   `json:"author"`
		Repository  string   `json:"repository"`
		Homepage    string   `json:"homepage"`
		License     string   `json:"license"`
		Version     string   `json:"version"`
		Tags        []string `json:"tags"`
	} `json:"plugins"`
}

// TestRegistryManifestIsValid 验证 registry.json 满足宿主的校验规则。
func TestRegistryManifestIsValid(t *testing.T) {
	raw, errRead := os.ReadFile("../../registry.json")
	if errRead != nil {
		t.Fatalf("read registry.json: %v", errRead)
	}
	var manifest registryManifest
	if errUnmarshal := json.Unmarshal(raw, &manifest); errUnmarshal != nil {
		t.Fatalf("registry.json is not valid JSON: %v", errUnmarshal)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", manifest.SchemaVersion)
	}
	if len(manifest.Plugins) == 0 {
		t.Fatal("registry must declare at least one plugin")
	}

	for _, entry := range manifest.Plugins {
		if entry.ID != app.PluginID {
			t.Fatalf("registry id = %q, must match the plugin id %q (the library file name)", entry.ID, app.PluginID)
		}
		if !pluginIDPattern.MatchString(entry.ID) {
			t.Fatalf("id %q does not match the host pattern", entry.ID)
		}
		for field, value := range map[string]string{
			"name": entry.Name, "description": entry.Description, "author": entry.Author,
		} {
			if strings.TrimSpace(value) == "" {
				t.Fatalf("required field %q is empty", field)
			}
		}
		if !githubRepositoryRe.MatchString(entry.Repository) {
			t.Fatalf("repository %q must be https://github.com/{owner}/{repo}", entry.Repository)
		}
		if entry.Version != "" && !pluginVersionPattern.MatchString(entry.Version) {
			t.Fatalf("version %q does not match the host pattern", entry.Version)
		}
	}
}

// TestPluginIDMatchesLibraryName 验证插件 ID 与产物名一致。
//
// 宿主按 {id}{ext} 找动态库；ID 与文件名不一致会让插件静默不被加载。
func TestPluginIDMatchesLibraryName(t *testing.T) {
	if app.PluginID != "freetier2api-fork" {
		t.Fatalf("app.PluginID = %q; changing it requires renaming the build output and registry", app.PluginID)
	}
	if app.ProviderKey != core.ProviderKey {
		t.Fatalf("app.ProviderKey (%q) must match core.ProviderKey (%q)", app.ProviderKey, core.ProviderKey)
	}
}

// TestConsolePagePathsAreDeclared 验证控制台页引用的路径都在路由表里声明。
//
// 页面引用了未声明的路由会表现为「点了没反应」，而宿主不会报错。
func TestConsolePagePathsAreDeclared(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	page := admin.Page(app.ManagementRoutePrefix)

	declared := map[string]bool{}
	for _, route := range testApp.ManagementRegistration().Routes {
		suffix := strings.TrimPrefix(route.Path, app.ManagementRoutePrefix)
		declared[suffix] = true
	}

	// 页面里通过 MGMT + path 拼接调用接口，因此按已知清单核对。
	referenced := []string{
		"/status", "/logs", "/checkin", "/quotas", "/models/refresh",
		"/models/alias", "/settings", "/tasks/scan", "/tasks/run", "/tasks/queue", "/tasks/auto",
		"/keepalive",
	}
	for _, path := range referenced {
		if !strings.Contains(page, `"`+path) {
			t.Fatalf("console page does not reference %q (test list is stale?)", path)
		}
		if !declared[path] {
			t.Fatalf("console page calls %q but the route is not declared", path)
		}
	}
}

// TestManagementResponseUsesHostWireFormat 验证管理响应沿用宿主的线格式。
//
// pluginapi.ManagementResponse **没有 json tag**，宿主按 Go 字段名
// （StatusCode / Headers / Body）解码，且 Body 是 []byte——Go 会把它编码成
// base64，宿主解码后把原始字节写给浏览器。
//
// 曾试图改成 snake_case + json.RawMessage 的自有格式，结果宿主反序列化失败，
// 页面报 "plugin resource handler failed" 且什么都不显示。这条测试锁住
// 「必须用宿主结构体、键名必须是 Go 字段名」这个契约。
func TestManagementResponseUsesHostWireFormat(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	resp := app.JSONResponse(http.StatusOK, map[string]any{"hello": "world"})
	raw, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	// 键名必须是宿主认得的 Go 字段名，不能是 snake_case。
	if _, okStatus := decoded["StatusCode"]; !okStatus {
		t.Fatalf("response must use the host's Go field names, got %s", raw)
	}
	if _, okBad := decoded["status_code"]; okBad {
		t.Fatalf("snake_case keys break the host decoder, got %s", raw)
	}
	if _, okBody := decoded["Body"]; !okBody {
		t.Fatalf("Body must be present, got %s", raw)
	}
	// HTML 响应同样走宿主结构体。
	htmlRaw, errHTML := json.Marshal(app.HTMLResponse(http.StatusOK, []byte("<html>x</html>")))
	if errHTML != nil {
		t.Fatalf("marshal html: %v", errHTML)
	}
	if !strings.Contains(string(htmlRaw), `"StatusCode":200`) {
		t.Fatalf("html response must use the host wire format, got %s", htmlRaw)
	}
}

// TestHostReservedQuotaPathNotShadowed 验证插件路由不与宿主保留路径冲突。
//
// 宿主的单账号额度路由是 /v0/management/plugins/<id>/quota，
// 插件自己的批量接口必须换路径（/quotas），否则会被宿主遮蔽。
func TestHostReservedQuotaPathNotShadowed(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)
	for _, route := range testApp.ManagementRegistration().Routes {
		suffix := strings.TrimPrefix(route.Path, app.ManagementRoutePrefix)
		if suffix == "/quota" {
			t.Fatal("plugin must not declare /quota (the host owns that path for single-account quota)")
		}
	}
}

// TestCapabilitiesMatchImplementedHandlers 验证声明的能力都有对应实现。
//
// 声明了能力却没有实现会让宿主调用到「未知方法」，表现为功能静默失效。
func TestCapabilitiesMatchImplementedHandlers(t *testing.T) {
	installFakeHost(t)
	setupTestPlugin(t)

	registration := testApp.Registration()
	if !registration.Capabilities.ModelProvider || !registration.Capabilities.AuthProvider ||
		!registration.Capabilities.Executor || !registration.Capabilities.QuotaProvider ||
		!registration.Capabilities.ManagementAPI {
		t.Fatal("all five capabilities must be declared")
	}

	// 每个能力的关键方法都必须能被分发（不返回 unknown_method）。
	methods := []string{
		"model.static", "model.for_auth",
		"auth.identifier", "auth.parse", "auth.login.start", "auth.login.poll", "auth.refresh",
		"executor.identifier", "executor.execute", "executor.execute_stream",
		"executor.count_tokens", "executor.http_request",
		"quota.identifier", "quota.describe", "quota.fetch", "quota.reset",
		"management.register", "management.handle",
	}
	for _, method := range methods {
		raw, errHandle := testApp.HandleMethod(method, nil)
		if errHandle != nil {
			// 业务错误（如缺参数）是可接受的；协议层错误才是问题。
			continue
		}
		if strings.Contains(string(raw), "unknown_method") {
			t.Fatalf("method %s is declared but not dispatched", method)
		}
	}
}
