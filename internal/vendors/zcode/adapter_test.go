package zcode

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestInstancesExposeDomesticAndInternational 锁住两个独立供应商实例。
func TestInstancesExposeDomesticAndInternational(t *testing.T) {
	instances := Instances(core.Env{})
	if len(instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(instances))
	}
	names := map[string]string{}
	regions := map[string]string{}
	for _, vendor := range instances {
		names[vendor.ID()] = vendor.Name()
		regions[vendor.ID()] = vendor.Region()
	}
	if names[VendorIDCN] != VendorNameCN || names[VendorIDGlobal] != VendorNameGlobal {
		t.Fatalf("names = %#v", names)
	}
	if regions[VendorIDCN] != RegionCN || regions[VendorIDGlobal] != RegionGlobal {
		t.Fatalf("regions = %#v", regions)
	}

	cn := instances[0].(*adapter)
	global := instances[1].(*adapter)
	// 显式 vendor 字段是强证据：同族另一实例的凭证必须否决。
	if !cn.Match("zcodecn-a.json", "freetier", map[string]any{"vendor": VendorIDCN, "jwt_token": "x"}) {
		t.Fatal("国内实例应认领 vendor=zcodecn")
	}
	if cn.Match("zcodeglobal-a.json", "freetier", map[string]any{"vendor": VendorIDGlobal, "jwt_token": "x"}) {
		t.Fatal("国内实例不应认领国际凭证")
	}
	if global.Match("zcodecn-a.json", "freetier", map[string]any{"vendor": VendorIDCN, "jwt_token": "x"}) {
		t.Fatal("国际实例不应认领国内凭证")
	}
	// 旧凭证 vendor=zcode：按 region 归属；缺 region 默认国际版。
	legacyCN := map[string]any{"vendor": VendorID, "region": RegionCN, "jwt_token": "x"}
	if !cn.Match("zcode-old.json", "freetier", legacyCN) {
		t.Fatal("旧国内凭证应按 region 归到国内实例")
	}
	if global.Match("zcode-old.json", "freetier", legacyCN) {
		t.Fatal("旧国内凭证不应归到国际实例")
	}
	if !global.Match("zcode-old.json", "freetier", map[string]any{"vendor": VendorID, "jwt_token": "x"}) {
		t.Fatal("缺 region 的旧凭证应默认归国际实例")
	}
	// 别家凭证绝不认领（宿主会把所有非内建格式挨个喂过来）。
	foreign := map[string]any{"vendor": "workbuddycn", "jwt_token": "x"}
	if cn.Match("workbuddycn-a.json", "freetier", foreign) || global.Match("workbuddycn-a.json", "freetier", foreign) {
		t.Fatal("不应认领别家凭证")
	}
	// 无 vendor、无文件名前缀、无 ZCode 特征的文件也不认领。
	bare := map[string]any{"api_key": "plain-key"}
	if cn.Match("account-1.json", "freetier", bare) || global.Match("account-1.json", "freetier", bare) {
		t.Fatal("无任何 ZCode 特征时不应认领")
	}
}

// TestLegacyIDsOnlyOnGlobalInstance 锁住旧前缀只映射到国际版。
//
// 拆分前 zcode 只有国际版（provider 恒为 zai），旧前缀若同时映射到国内版，
// 用户写的 zcode:GLM-5.3 会同时禁用/改名两家，语义就错了。
func TestLegacyIDsOnlyOnGlobalInstance(t *testing.T) {
	cn := Instances(core.Env{})[0].(*adapter)
	global := Instances(core.Env{})[1].(*adapter)
	if ids := cn.LegacyIDs(); len(ids) != 0 {
		t.Fatalf("国内实例不应声明旧 ID，got %v", ids)
	}
	ids := global.LegacyIDs()
	if len(ids) != 1 || ids[0] != VendorID {
		t.Fatalf("国际实例旧 ID = %v，want [%s]", ids, VendorID)
	}
}

// TestConfigureBaseURLPerInstance 锁住实例级配置键与旧族级键兼容。
func TestConfigureBaseURLPerInstance(t *testing.T) {
	cn, global := newTestInstances()
	cn.Configure(core.Settings{}, map[string]string{configKeyBaseURLCN: "http://cn.test"})
	global.Configure(core.Settings{}, map[string]string{configKeyBaseURLGlobal: "http://global.test"})
	if cn.baseURL != "http://cn.test" {
		t.Fatalf("国内实例 baseURL = %q", cn.baseURL)
	}
	if global.baseURL != "http://global.test" {
		t.Fatalf("国际实例 baseURL = %q", global.baseURL)
	}

	// 旧族级键两个实例都读：升级前只有一个实例，存量配置必须继续生效。
	cn, global = newTestInstances()
	legacy := map[string]string{configKeyBaseURLLegacy: "http://legacy.test"}
	cn.Configure(core.Settings{}, legacy)
	global.Configure(core.Settings{}, legacy)
	if cn.baseURL != "http://legacy.test" || global.baseURL != "http://legacy.test" {
		t.Fatalf("旧键未生效：cn=%q global=%q", cn.baseURL, global.baseURL)
	}

	// 实例键优先于旧键，避免两处都配时行为不可预期。
	both := map[string]string{configKeyBaseURLCN: "http://cn.test", configKeyBaseURLLegacy: "http://legacy.test"}
	cn, _ = newTestInstances()
	cn.Configure(core.Settings{}, both)
	if cn.baseURL != "http://cn.test" {
		t.Fatalf("实例键应优先，baseURL = %q", cn.baseURL)
	}
}

// TestConfigFieldsHaveNoDuplicates 锁住宿主配置表单不出现同名重复项。
//
// 宿主对 ConfigFields 只做 append 不去重（internal/app/main.go 的
// configFields）：两个实例报同一个键名就会看到两条同名输入框。
func TestConfigFieldsHaveNoDuplicates(t *testing.T) {
	cn, global := newTestInstances()
	seen := map[string]bool{}
	for _, vendor := range []*adapter{cn, global} {
		for _, field := range vendor.ConfigFields() {
			if seen[field.Name] {
				t.Fatalf("配置键 %q 被重复上报", field.Name)
			}
			seen[field.Name] = true
		}
	}
	if !seen[configKeyBaseURLCN] || !seen[configKeyBaseURLGlobal] || !seen[configKeyClaimMinInterval] {
		t.Fatalf("配置字段缺失：%#v", seen)
	}
}

func newTestInstances() (cn, global *adapter) {
	instances := Instances(core.Env{})
	return instances[0].(*adapter), instances[1].(*adapter)
}
