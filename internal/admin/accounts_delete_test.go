package admin

// 本文件锁定「删除账号」前的全部校验。
//
// 真实事故：EasyCLIProxyAPI 把内核的 oauth.auth-dir 配成相对路径（默认
// ../oauth，相对内核工作目录）。CLIProxyAPI 扫盘时把相对路径写进 auth 属性，
// host.auth.get / host.auth.list 原样返回，删除保护于是收到
// `..\oauth\<name>.json`，一律判成「非绝对路径」——Windows 桌面版点「删除」
// 永远删不掉账号。
//
// 路径归一化在 hostbridge 边界完成（见 hostbridge.ResolveAuthPath），这里只钉住
// 「这个路径能不能删」：相对路径能删、文件不存在要报错（不能假报成功）、
// 非 .json / 符号链接 / 目录 一律拒绝、目标文件属于另一个账号时拒绝。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"freetier2api-plugin/internal/core"
)

// deleteTestVendor 的最小凭证解析：按 vendor / uid 两个字段即可支撑归属比对。
type deleteTestVendor struct{ fakeVendor }

func (v *deleteTestVendor) Parse(raw []byte, _ string) (*core.Credential, error) {
	var payload struct {
		Vendor string `json:"vendor"`
		UID    string `json:"uid"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return &core.Credential{VendorID: payload.Vendor, UID: payload.UID}, nil
}

func newDeleteTestRegistry(t *testing.T) *core.Registry {
	t.Helper()
	registry := core.NewRegistry()
	for _, id := range []string{"traecn", "workbuddycn"} {
		if err := registry.Register(&deleteTestVendor{fakeVendor{Meta: core.Meta{VendorID: id}}}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return registry
}

func writeAuthFile(t *testing.T, dir, name string, payload map[string]any) string {
	t.Helper()
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal credential: %v", errMarshal)
	}
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write credential: %v", errWrite)
	}
	return path
}

// TestSafeAuthFileForDeleteRequiresNormalizedPath 锁定边界契约：删除校验只接受
// 已经由 hostbridge 归一化的绝对路径。相对路径的解析在宿主边界做（见
// hostbridge.ResolveAuthPath），端到端行为由 tests/integration 覆盖。
func TestSafeAuthFileForDeleteRequiresNormalizedPath(t *testing.T) {
	dir := t.TempDir()
	absolute := writeAuthFile(t, dir, "traecn-u-1.json", map[string]any{"vendor": "traecn", "uid": "u-1"})
	entry := core.HostAuth{Name: "traecn-u-1.json", AuthIndex: "1"}
	hostRaw := []byte(`{"vendor":"traecn","uid":"u-1"}`)
	registry := newDeleteTestRegistry(t)

	got, errSafe := safeAuthFilePath(registry, entry, hostRaw, absolute)
	if errSafe != nil {
		t.Fatalf("绝对路径应允许删除: %v", errSafe)
	}
	if got != absolute {
		t.Fatalf("返回路径 = %q, want %q", got, absolute)
	}

	if _, errRelative := safeAuthFilePath(registry, entry, hostRaw, filepath.Base(absolute)); errRelative == nil {
		t.Fatal("未归一化的相对路径必须被拒绝（归一化是宿主边界的职责）")
	}
}

// TestSafeAuthFileForDeleteRejectsMissingFile 锁定「不存在 ≠ 已删除」。
// 宿主给的路径可能因为工作目录基准不同而指向别处，静默成功会让页面显示
// 「已删除」而账号其实还在。
func TestSafeAuthFileForDeleteRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	entry := core.HostAuth{Name: "traecn-u-1.json", AuthIndex: "1"}
	_, errSafe := safeAuthFilePath(newDeleteTestRegistry(t), entry,
		[]byte(`{"vendor":"traecn","uid":"u-1"}`), filepath.Join(dir, "traecn-u-1.json"))
	if errSafe == nil {
		t.Fatal("文件不存在时必须报错，不能当作删除成功")
	}
}

// TestSafeAuthFileForDeleteRejectsRuntimeCredential 运行时凭证没有文件可删。
func TestSafeAuthFileForDeleteRejectsRuntimeCredential(t *testing.T) {
	entry := core.HostAuth{Name: "traecn-u-1", AuthIndex: "1", RuntimeOnly: true}
	if _, errSafe := safeAuthFilePath(newDeleteTestRegistry(t), entry,
		[]byte(`{"vendor":"traecn","uid":"u-1"}`), ""); errSafe == nil {
		t.Fatal("运行时凭证必须拒绝删除")
	}
}

// TestSafeAuthFileForDeleteRejectsNonCredentialFiles 锁定安全检查没有被放宽：
// 非 .json、目录、符号链接都不许删。
func TestSafeAuthFileForDeleteRejectsNonCredentialFiles(t *testing.T) {
	registry := newDeleteTestRegistry(t)
	dir := t.TempDir()
	entry := core.HostAuth{Name: "traecn-u-1.txt", AuthIndex: "1"}
	hostRaw := []byte(`{"vendor":"traecn","uid":"u-1"}`)
	payload := map[string]any{"vendor": "traecn", "uid": "u-1"}

	textFile := writeAuthFile(t, dir, "traecn-u-1.txt", payload)
	if _, errSafe := safeAuthFilePath(registry, entry, hostRaw, textFile); errSafe == nil {
		t.Fatal("非 .json 文件必须拒绝删除")
	}

	subdir := filepath.Join(dir, "traecn-dir.json")
	if errMkdir := os.Mkdir(subdir, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	if _, errSafe := safeAuthFilePath(registry, entry, hostRaw, subdir); errSafe == nil {
		t.Fatal("目录必须拒绝删除")
	}

	target := writeAuthFile(t, dir, "traecn-target.json", payload)
	link := filepath.Join(dir, "traecn-link.json")
	if errLink := os.Symlink(target, link); errLink != nil {
		t.Skipf("symlink unavailable: %v", errLink)
	}
	if _, errSafe := safeAuthFilePath(registry, entry, hostRaw, link); errSafe == nil {
		t.Fatal("符号链接必须拒绝删除")
	}
}

// TestSafeAuthFileForDeleteRejectsForeignCredential 防止删错账号：路径归一化用
// 进程工作目录，基准不一致时同名文件可能属于另一个账号。
func TestSafeAuthFileForDeleteRejectsForeignCredential(t *testing.T) {
	registry := newDeleteTestRegistry(t)
	dir := t.TempDir()
	path := writeAuthFile(t, dir, "traecn-u-1.json", map[string]any{"vendor": "traecn", "uid": "u-other"})
	entry := core.HostAuth{Name: "traecn-u-1.json", AuthIndex: "1"}

	if _, errSafe := safeAuthFilePath(registry, entry,
		[]byte(`{"vendor":"traecn","uid":"u-1"}`), path); errSafe == nil {
		t.Fatal("目标文件属于另一个账号时必须拒绝删除")
	}
}
