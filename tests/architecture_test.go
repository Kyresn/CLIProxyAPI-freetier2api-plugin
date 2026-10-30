package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 本文件用静态检查守住分层边界、供应商目录白名单与测试布局。

const vendorImportPrefix = "freetier2api-plugin/internal/vendors/"

func TestNoCrossVendorImports(t *testing.T) {
	root := filepath.Join("..", "internal", "vendors")
	errWalk := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		owner := vendorOwner(path)
		if owner == "" {
			return nil
		}
		for _, imp := range fileImports(t, path) {
			if !strings.HasPrefix(imp, vendorImportPrefix) {
				continue
			}
			target := strings.Split(strings.TrimPrefix(imp, vendorImportPrefix), "/")[0]
			if target != owner {
				t.Errorf("%s：供应商包 %q 不得 import 另一个供应商包 %q", path, owner, target)
			}
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk vendors: %v", errWalk)
	}
}

func TestPluginAppVendorImportsOnlyInCompositionRoot(t *testing.T) {
	dir := filepath.Join("..", "internal", "app")
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read app: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() == "vendors.go" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, vendorImportPrefix) {
				t.Errorf("%s：只有组合根 vendors.go 可以直接 import 具体供应商", path)
			}
		}
	}
}

func vendorOwner(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/internal/vendors/")
	if len(parts) != 2 {
		return ""
	}
	return strings.Split(parts[1], "/")[0]
}

func fileImports(t *testing.T, path string) []string {
	t.Helper()
	file, errParse := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if errParse != nil {
		t.Fatalf("parse %s: %v", path, errParse)
	}
	out := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		value, errUnquote := strconv.Unquote(spec.Path.Value)
		if errUnquote != nil {
			t.Fatalf("unquote import in %s: %v", path, errUnquote)
		}
		out = append(out, value)
	}
	return out
}

func TestTaskEngineVendorNeutral(t *testing.T) {
	dir := filepath.Join("..", "internal", "tasks")
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read tasks: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, vendorImportPrefix) || imp == "freetier2api-plugin/internal/admin" {
				t.Errorf("%s：任务引擎不得依赖供应商或管理接口（%s）", path, imp)
			}
		}
	}

	coreTask := filepath.Join("..", "internal", "core", "task.go")
	raw, errRead := os.ReadFile(coreTask)
	if errRead != nil {
		t.Fatalf("read core/task.go: %v", errRead)
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"school", "growth"} {
		if strings.Contains(lower, banned) {
			t.Errorf("internal/core/task.go：通用任务契约不得出现供应商专属标识 %q", banned)
		}
	}
}

func TestCoreDoesNotImportUpperLayers(t *testing.T) {
	entries, errRead := os.ReadDir(filepath.Join("..", "internal", "core"))
	if errRead != nil {
		t.Fatalf("read core: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join("..", "internal", "core", entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, "freetier2api-plugin/internal/") {
				t.Errorf("%s：internal/core 只能依赖 cpasdk 与标准库（发现 %s）", path, imp)
			}
		}
	}
}

func TestTestLayout(t *testing.T) {
	rootEntries, errRead := os.ReadDir(".")
	if errRead != nil {
		t.Fatalf("read tests root: %v", errRead)
	}
	for _, entry := range rootEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() != "architecture_test.go" {
			t.Errorf("tests/%s：集成测试应放在 tests/integration/，tests/ 根只保留架构守卫", entry.Name())
		}
	}

	integrationEntries, errRead := os.ReadDir(filepath.Join("integration"))
	if errRead != nil {
		t.Fatalf("read tests/integration: %v", errRead)
	}
	for _, entry := range integrationEntries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			t.Errorf("tests/integration/%s：集成测试目录不应包含非测试源码", entry.Name())
		}
	}
}

// layerRule 描述一个内部包的依赖边界。
//
// 之所以用一张显式的表而不是逐个 if：重构时新增/删除内部包必须同步这张表，
// 「忘记更新分层规则」会立刻在 CI 里失败，而不是靠人记住。
type layerRule struct {
	pkg string
	// forbidden 是该包**不允许**import 的 internal 包（精确到一级目录）。
	forbidden []string
	// allowlist 非空时，import 集合必须完全落在其中（供应商包用）。
	allowlist []string
}

// layerRules 是仓库的分层契约。
//
// 依赖方向自上而下：app -> admin / tasks / auth / hostbridge / vendors -> core。
// 同层之间不互相依赖（admin 与 tasks、tasks 与 auth 都禁止）。
//
// 供应商包用 allowlist 而不是 forbidden：它们的依赖面必须**恰好**是
// core（契约）+ hostbridge（宿主通信）+ logger（日志）三者，多一个都不行——
// 这条规则挡住了 vendor 反向依赖 auth / state / tasks / admin 的所有可能。
var layerRules = []layerRule{
	{pkg: "core", forbidden: []string{"admin", "app", "auth", "hostbridge", "logger", "state", "tasks", "vendors"}},
	{pkg: "logger"},
	{pkg: "state", forbidden: []string{"admin", "app", "auth", "hostbridge", "tasks", "vendors"}},
	{pkg: "hostbridge", forbidden: []string{"admin", "app", "auth", "state", "tasks", "vendors"}},
	{pkg: "auth", forbidden: []string{"admin", "app", "tasks", "vendors"}},
	{pkg: "tasks", forbidden: []string{"admin", "app", "vendors"}},
	{pkg: "admin", forbidden: []string{"app"}},
	{pkg: "vendors/autoclaw", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/cline", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/codearts", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/minimaxcode", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/opencodezen", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/qoder", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/raccoon", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/trae", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/workbuddy", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/zcode", allowlist: []string{"core", "hostbridge", "logger"}},
}

// internalPackages 是 internal/ 下一级允许存在的目录名白名单。
//
// 它的作用是**发现问题**：新增一个包却忘了在 layerRules 里定边界时，
// 白名单会先把它拦下来，提示补规则，而不是让它静默进入依赖图。
var internalPackages = map[string]bool{
	"admin": true, "app": true, "auth": true, "core": true,
	"hostbridge": true, "logger": true, "state": true, "tasks": true, "vendors": true,
}

func TestInternalPackageAllowlist(t *testing.T) {
	entries, errRead := os.ReadDir(filepath.Join("..", "internal"))
	if errRead != nil {
		t.Fatalf("read internal: %v", errRead)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !internalPackages[entry.Name()] {
			t.Errorf("internal/%s：新增内部包必须同步 internalPackages 与 layerRules（分层守卫要求显式声明边界）", entry.Name())
		}
	}
}

// TestDeclaredPackagesHaveBoundaryRules 让白名单与边界规则互相兜底：
//
//   - internalPackages 里的非组合根包，必须在 layerRules 里有边界（否则新增
//     包只加白名单就能绕过依赖检查）；
//   - layerRules 里的非 vendor 包，必须也在 internalPackages 里（否则删掉
//     白名单条目后，目录在 internal/ 下却不再被声明）。
//
// app 是组合根、vendors 由每个供应商条目覆盖，两者都按约定排除。
func TestDeclaredPackagesHaveBoundaryRules(t *testing.T) {
	covered := make(map[string]bool, len(layerRules))
	for _, rule := range layerRules {
		covered[strings.Split(rule.pkg, "/")[0]] = true
	}
	for pkg := range internalPackages {
		if pkg == "app" || pkg == "vendors" {
			continue
		}
		if !covered[pkg] {
			t.Errorf("internal/%s is allowlisted but has no layerRules boundary", pkg)
		}
	}
	for pkg := range covered {
		if pkg == "vendors" {
			continue
		}
		if !internalPackages[pkg] {
			t.Errorf("internal/%s has a layerRules boundary but is missing from internalPackages", pkg)
		}
	}
}

func TestLayerBoundaries(t *testing.T) {
	root := filepath.Join("..", "internal")
	for _, rule := range layerRules {
		dir := filepath.Join(root, filepath.FromSlash(rule.pkg))
		entries, errRead := os.ReadDir(dir)
		if errRead != nil {
			t.Fatalf("read %s: %v", rule.pkg, errRead)
		}
		banned := make(map[string]bool, len(rule.forbidden))
		for _, name := range rule.forbidden {
			banned[name] = true
		}
		allowed := make(map[string]bool, len(rule.allowlist))
		for _, name := range rule.allowlist {
			allowed[name] = true
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			for _, imp := range fileImports(t, path) {
				if !strings.HasPrefix(imp, "freetier2api-plugin/internal/") {
					continue
				}
				target := strings.TrimPrefix(imp, "freetier2api-plugin/internal/")
				if banned[strings.Split(target, "/")[0]] {
					t.Errorf("%s：internal/%s 不得依赖 internal/%s", path, rule.pkg, target)
				}
				if len(allowed) > 0 && !allowed[target] {
					t.Errorf("%s：internal/%s 只能依赖 %v，实际依赖了 %s",
						path, rule.pkg, rule.allowlist, target)
				}
			}
		}
	}
}

// TestVendorsNeverWriteCredentialsDirectly 守住「凭证写回只有一条路径」。
//
// 分层 allowlist 只限制 vendor 只能依赖 core/hostbridge/logger，但 hostbridge
// 同时提供 HTTP 桥与宿主回调，光靠 import 图看不出「这个 vendor 在发
// host.auth.save」。凭证写回（合并 → host.auth.save → 落盘）必须由
// internal/tasks 的保活编排统一做，供应商只实现 core.Refresher 刷新 token——
// 否则每加一个供应商就多一份会漂移的写回实现。
func TestVendorsNeverWriteCredentialsDirectly(t *testing.T) {
	forbidden := []string{"MethodHostAuthSave", "MethodAuthSave", "host.auth.save"}
	root := filepath.Join("..", "internal", "vendors")
	errWalk := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		body := stripComments(string(raw))
		for _, token := range forbidden {
			if strings.Contains(body, token) {
				t.Errorf("%s：供应商不得自行写回凭证（出现 %q）——刷新 token 只实现 core.Refresher，写回由 internal/tasks 统一编排", path, token)
			}
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk vendors: %v", errWalk)
	}
}

// ---------------------------------------------------------------------------
// 供应商目录结构与文件白名单守卫
// ---------------------------------------------------------------------------

// vendorRequiredFiles / vendorOptionalFiles 是 internal/vendors/<name> 的封闭白名单。
//
// 文件名即职责：新增角色必须先改这里和 README「架构与目录规范」，再落代码。
// login.go 只能有一个；登录渠道不得拆成 login_*.go 源码文件。测试与 assets/ 单独处理。
var vendorRequiredFiles = []string{
	"adapter.go",
	"chat.go",
	"credential.go",
	"models.go",
	"errors.go",
	"login.go",
}

var vendorOptionalFiles = []string{
	"tasks.go",
}

var vendorDirNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

var embedDirectivePattern = regexp.MustCompile(`(?m)^//go:embed[ \t]+([^\r\n]+)$`)

// TestVendorDirectoryLayout 守住 vendors 目录的封闭白名单。
//
// 它检查：目录名、必备文件、登录文件、package 名、禁止子包/杂文件/symlink。
// 新增文件角色时，必须同时更新本文件的 vendorRequiredFiles/vendorOptionalFiles
// 与 README，否则这里会失败——这正是「规范不会再次漂移」的棘轮。
func TestVendorDirectoryLayout(t *testing.T) {
	root, rootEntries := vendorRootEntries(t)
	for _, entry := range rootEntries {
		if !entry.IsDir() {
			t.Errorf("internal/vendors/%s：vendors 根目录只允许供应商子目录", entry.Name())
		}
	}

	allowed := slices.Concat(vendorRequiredFiles, vendorOptionalFiles)
	for _, vendor := range vendorDirectoryNames(rootEntries) {
		t.Run(vendor, func(t *testing.T) {
			if !vendorDirNamePattern.MatchString(vendor) {
				t.Errorf("internal/vendors/%s：目录名必须是小写字母/数字", vendor)
			}
			dir := filepath.Join(root, vendor)
			entries, errRead := os.ReadDir(dir)
			if errRead != nil {
				t.Fatalf("read %s: %v", dir, errRead)
			}
			present := make(map[string]bool, len(entries))
			for _, entry := range entries {
				name := entry.Name()
				entryPath := filepath.Join(dir, name)
				if entry.Type()&os.ModeSymlink != 0 {
					t.Errorf("%s：vendor 目录不允许符号链接", entryPath)
					continue
				}
				if entry.IsDir() {
					if name != "assets" {
						t.Errorf("%s：vendor 目录只允许 assets/ 子目录，发现 %q", dir, name)
					}
					continue
				}
				present[name] = true
				if strings.HasSuffix(name, "_test.go") {
					continue
				}
				if !strings.HasSuffix(name, ".go") {
					t.Errorf("%s：vendor 目录只允许 .go 文件与 assets/，发现 %q", entryPath, name)
					continue
				}
				if strings.HasPrefix(name, "login_") && strings.HasSuffix(name, ".go") {
					t.Errorf("%s：登录渠道必须合并进唯一的 login.go，禁止 login_*.go 源码文件", entryPath)
					continue
				}
				if slices.Contains(vendorRequiredFiles, name) || slices.Contains(vendorOptionalFiles, name) {
					continue
				}
				t.Errorf("%s：不在 vendor 文件白名单内；允许 %v + *_test.go + assets/", entryPath, allowed)
			}
			for _, name := range vendorRequiredFiles {
				if !present[name] {
					t.Errorf("%s：缺少必备文件 %s", dir, name)
				}
			}
			assertVendorPackageNames(t, dir, vendor)
		})
	}
}

// TestVendorAssetsAreEmbedded 守住 assets/ 与 go:embed 的双向一致：
// 每个资源都必须被引用，每条 embed 路径都必须能匹配到资源。
func TestVendorAssetsAreEmbedded(t *testing.T) {
	root, rootEntries := vendorRootEntries(t)
	for _, vendor := range vendorDirectoryNames(rootEntries) {
		t.Run(vendor, func(t *testing.T) {
			dir := filepath.Join(root, vendor)
			assetsDir := filepath.Join(dir, "assets")
			info, errStat := os.Stat(assetsDir)
			if os.IsNotExist(errStat) {
				return
			}
			if errStat != nil {
				t.Fatalf("stat %s: %v", assetsDir, errStat)
			}
			if !info.IsDir() {
				t.Fatalf("%s：assets 必须是目录", assetsDir)
			}
			assets := vendorAssetFiles(t, dir, assetsDir)
			if len(assets) == 0 {
				t.Errorf("%s：assets/ 存在但没有文件", assetsDir)
				return
			}
			patterns := vendorEmbedPatterns(t, dir)
			matched := make(map[string]bool, len(assets))
			for _, pattern := range patterns {
				if !safeEmbedPattern(t, dir, pattern) {
					continue
				}
				hit := false
				for _, asset := range assets {
					if embedPatternMatches(pattern, asset) {
						matched[asset] = true
						hit = true
					}
				}
				if !hit {
					t.Errorf("%s：go:embed %q 没有匹配到 assets/ 下的文件", dir, pattern)
				}
			}
			for _, asset := range assets {
				if !matched[asset] {
					t.Errorf("%s：assets 文件未被任何 go:embed 引用", filepath.Join(dir, filepath.FromSlash(asset)))
				}
			}
		})
	}
}

// TestEveryVendorHasLayerRule 让「新增 vendor 目录」不可能绕过依赖边界检查。
//
// TestLayerBoundaries 只遍历 layerRules；如果新 vendor 没登记，它的 import
// 就完全不受约束。这里用目录集合反向校验 layerRules，并锁定 allowlist。
func TestEveryVendorHasLayerRule(t *testing.T) {
	dirs := vendorDirectories(t)
	rules := make(map[string]layerRule)
	for _, rule := range layerRules {
		if !strings.HasPrefix(rule.pkg, "vendors/") {
			continue
		}
		name := strings.TrimPrefix(rule.pkg, "vendors/")
		if strings.Contains(name, "/") {
			t.Errorf("layerRules：%s 不应再声明 vendor 子包；vendor 必须是单包", rule.pkg)
			continue
		}
		if _, exists := rules[name]; exists {
			t.Errorf("layerRules：vendors/%s 重复声明", name)
			continue
		}
		rules[name] = rule
	}
	want := []string{"core", "hostbridge", "logger"}
	for _, dir := range dirs {
		rule, ok := rules[dir]
		if !ok {
			t.Errorf("internal/vendors/%s：缺少 layerRules 边界；新增供应商必须显式声明 allowlist %v", dir, want)
			continue
		}
		if !sameStringSet(rule.allowlist, want) {
			t.Errorf("internal/vendors/%s：allowlist = %v，必须恰好是 %v", dir, rule.allowlist, want)
		}
	}
	stale := make([]string, 0)
	for name := range rules {
		if !slices.Contains(dirs, name) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	for _, name := range stale {
		t.Errorf("layerRules 声明了 vendors/%s，但目录不存在", name)
	}
}

// TestVendorRegistrationMatchesDirectories 守住组合根：
// 每个 vendor 目录都必须被 internal/app/vendors.go 导入并在 BuiltinFactories 注册。
func TestVendorRegistrationMatchesDirectories(t *testing.T) {
	dirs := vendorDirectories(t)
	appFile := filepath.Join("..", "internal", "app", "vendors.go")
	imported := make(map[string]bool)
	for _, imp := range fileImports(t, appFile) {
		if !strings.HasPrefix(imp, vendorImportPrefix) {
			continue
		}
		name := strings.Split(strings.TrimPrefix(imp, vendorImportPrefix), "/")[0]
		imported[name] = true
	}
	for _, dir := range dirs {
		if !imported[dir] {
			t.Errorf("internal/vendors/%s：目录存在但 internal/app/vendors.go 没有导入/注册", dir)
		}
	}
	stale := make([]string, 0)
	for name := range imported {
		if !slices.Contains(dirs, name) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	for _, name := range stale {
		t.Errorf("internal/app/vendors.go 导入了 vendors/%s，但目录不存在", name)
	}
	body := functionBody(t, appFile, "BuiltinFactories")
	for _, dir := range dirs {
		if !strings.Contains(body, dir+".Instances") {
			t.Errorf("internal/app/vendors.go：BuiltinFactories 未注册 %s.Instances", dir)
		}
	}
}

func vendorRootEntries(t *testing.T) (string, []os.DirEntry) {
	t.Helper()
	root := filepath.Join("..", "internal", "vendors")
	entries, errRead := os.ReadDir(root)
	if errRead != nil {
		t.Fatalf("read vendors: %v", errRead)
	}
	return root, entries
}

func vendorDirectories(t *testing.T) []string {
	t.Helper()
	_, entries := vendorRootEntries(t)
	return vendorDirectoryNames(entries)
}

func vendorDirectoryNames(entries []os.DirEntry) []string {
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	slices.Sort(dirs)
	return dirs
}

func assertVendorPackageNames(t *testing.T, dir, want string) {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read %s: %v", dir, errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		file, errParse := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.PackageClauseOnly)
		if errParse != nil {
			t.Fatalf("parse %s: %v", filePath, errParse)
		}
		if file.Name.Name != want {
			t.Errorf("%s：package %q 与目录名 %q 不一致", filePath, file.Name.Name, want)
		}
	}
}

func vendorAssetFiles(t *testing.T, dir, assetsDir string) []string {
	t.Helper()
	assets := make([]string, 0)
	errWalk := filepath.Walk(assetsDir, func(assetPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, errRel := filepath.Rel(dir, assetPath)
		if errRel != nil {
			return errRel
		}
		assets = append(assets, filepath.ToSlash(rel))
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk %s: %v", assetsDir, errWalk)
	}
	slices.Sort(assets)
	return assets
}

func vendorEmbedPatterns(t *testing.T, dir string) []string {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read %s: %v", dir, errRead)
	}
	patterns := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		raw, errReadFile := os.ReadFile(filePath)
		if errReadFile != nil {
			t.Fatalf("read %s: %v", filePath, errReadFile)
		}
		for _, match := range embedDirectivePattern.FindAllStringSubmatch(string(raw), -1) {
			for _, field := range strings.Fields(match[1]) {
				field = strings.Trim(field, "`\"")
				if field != "" {
					patterns = append(patterns, field)
				}
			}
		}
	}
	slices.Sort(patterns)
	return patterns
}

func safeEmbedPattern(t *testing.T, dir, pattern string) bool {
	t.Helper()
	invalid := strings.HasPrefix(pattern, "/") || filepath.IsAbs(pattern)
	if !invalid {
		for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
			if segment == ".." {
				invalid = true
				break
			}
		}
	}
	if invalid {
		t.Errorf("%s：go:embed 路径必须是 vendor 目录内的相对路径：%q", dir, pattern)
		return false
	}
	return true
}

func embedPatternMatches(pattern, asset string) bool {
	if matched, _ := path.Match(pattern, asset); matched {
		return true
	}
	// go:embed 也接受目录名；目录会递归包含其下所有文件。
	prefix := strings.TrimSuffix(filepath.ToSlash(pattern), "/")
	return prefix != "" && strings.HasPrefix(asset, prefix+"/")
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[string]bool, len(got))
	for _, value := range got {
		set[value] = true
	}
	for _, value := range want {
		if !set[value] {
			return false
		}
	}
	return true
}

func functionBody(t *testing.T, filePath, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, errParse := parser.ParseFile(fset, filePath, nil, 0)
	if errParse != nil {
		t.Fatalf("parse %s: %v", filePath, errParse)
	}
	raw, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("read %s: %v", filePath, errRead)
	}
	for _, decl := range file.Decls {
		fn, okFunc := decl.(*ast.FuncDecl)
		if !okFunc || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Lbrace).Offset
		end := fset.Position(fn.Body.Rbrace).Offset
		return string(raw[start:end])
	}
	t.Fatalf("%s：未找到函数 %s", filePath, name)
	return ""
}

// stripComments 去掉 Go 源码里的注释，只留可编译的代码文本。
//
// 守卫要在代码里找符号，而不是在解释这个符号的注释里找——否则开发者
// 一旦在注释里说明「这里不该出现 host.auth.save」就会把自己绊在守卫上。
func stripComments(src string) string {
	var out strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				i = len(src)
			} else {
				i += end
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
			out.WriteByte(' ')
		case src[i] == '\'' || src[i] == '"' || src[i] == '`':
			quote := src[i]
			out.WriteByte(quote)
			i++
			for i < len(src) {
				if quote != '`' && src[i] == '\\' {
					out.WriteByte(src[i])
					i++
					if i < len(src) {
						out.WriteByte(src[i])
						i++
					}
					continue
				}
				out.WriteByte(src[i])
				if src[i] == quote {
					i++
					break
				}
				i++
			}
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String()
}
