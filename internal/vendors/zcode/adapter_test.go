package zcode

import (
	"testing"

	"freetier2api-plugin/internal/core"
)

func TestConfigureBaseURLPerInstance(t *testing.T) {
	v := Instances(core.Env{})[0].(*adapter)
	v.Configure(core.Settings{}, map[string]string{"zcode_base_url": "http://zcode.test"})
	if v.baseURL != "http://zcode.test" {
		t.Fatalf("baseURL = %q", v.baseURL)
	}
}

// TestInstancesExposeDomesticAndInternational 锁住两个独立供应商实例。
func TestInstancesExposeDomesticAndInternational(t *testing.T) {
	vendors := Instances(core.Env{})
	if len(vendors) != 2 {
		t.Fatalf("instances = %d, want 2", len(vendors))
	}
	ids := map[string]bool{vendors[0].ID(): true, vendors[1].ID(): true}
	if !ids[VendorIDCN] || !ids[VendorIDGlobal] {
		t.Fatalf("ids = %#v, want %s + %s", ids, VendorIDCN, VendorIDGlobal)
	}
	names := map[string]string{vendors[0].ID(): vendors[0].Name(), vendors[1].ID(): vendors[1].Name()}
	if names[VendorIDCN] != VendorNameCN || names[VendorIDGlobal] != VendorNameGlobal {
		t.Fatalf("names = %#v", names)
	}

	cn := vendors[0].(*adapter)
	global := vendors[1].(*adapter)
	if !cn.Match("zcodecn-a.json", "freetier", map[string]any{"vendor": VendorIDCN, "jwt_token": "x"}) {
		t.Fatal("国内实例应认领 vendor=zcodecn")
	}
	if cn.Match("zcodeglobal-a.json", "freetier", map[string]any{"vendor": VendorIDGlobal, "jwt_token": "x"}) {
		t.Fatal("国内实例不应认领国际凭证")
	}
	// 旧凭证 vendor=zcode：按 region 归属；缺 region 默认国际版。
	if !cn.Match("zcode-old.json", "freetier", map[string]any{"vendor": VendorID, "region": RegionCN, "jwt_token": "x"}) {
		t.Fatal("旧国内凭证应按 region 归到国内实例")
	}
	if global.Match("zcode-old.json", "freetier", map[string]any{"vendor": VendorID, "region": RegionCN, "jwt_token": "x"}) {
		t.Fatal("旧国内凭证不应归到国际实例")
	}
	if !global.Match("zcode-old.json", "freetier", map[string]any{"vendor": VendorID, "jwt_token": "x"}) {
		t.Fatal("缺 region 的旧凭证应默认归国际实例")
	}
}
