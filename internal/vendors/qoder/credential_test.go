package qoder

import (
	"os"
	"path/filepath"
	"testing"
)

// disableLocalMachineIdentity 让本机自动解析确定性地「拿不到身份」。
//
// 测试不能依赖开发机是否装了 Qoder 客户端：否则同一个用例在不同机器上
// 结果不同，而本机解析本身又需要 3.8 秒 spawn。
func disableLocalMachineIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("QODER_RUNTIME_INFO", filepath.Join(t.TempDir(), "missing-runtime-info"))
	t.Setenv("QODER_MACHINE_TOKEN_PATH", filepath.Join(t.TempDir(), "missing-machine-token.json"))
}

// 本文件锁住设备身份的解析规则。
//
// 背景（issue #13 / #10）：上游对 cosy-machinetoken + cosy-machinetype 做
// **精确校验**。2026-10-23 逐头隔离实测（同一国际号）：
//
//	不发机器头                      → 2 条 VIEW_DETAILS，无 CLAIM_BENEFIT
//	官方组件（Windows 真机）的配对   → 出现 CLAIM_BENEFIT（可领）
//	同一 token 改动 1 个字符        → 活动消失
//	同一 token 截断 1 位 / 增补 1 位 → 活动消失
//	A 设备 token + B 设备 type      → 活动消失
//	随机/派生 88 字符 token + type  → 活动消失
//	容器内官方组件（isVm=true）     → 活动消失
//
// 国内版更严格地反向：不发机器头可领，发了派生 token 反而被过滤。
// 结论：插件不得自行派生身份，只能转发用户提供的真值。

// TestResolveIdentityUsesProvidedValues 覆盖：用户提供的值原样透传，不改写。
func TestResolveIdentityUsesProvidedValues(t *testing.T) {
	const token = "P1gA-official-token-from-runtime-info"
	const machineType = "001b505914466e907f"
	cred := &Credential{MachineToken: token, MachineType: machineType, MachineCode: "c075415b76a3900977"}

	identity := ResolveIdentity(cred, nil, MachineIdentity{})
	if identity.Token != token || identity.Type != machineType {
		t.Errorf("应原样透传凭证里的身份: %+v", identity)
	}
	if identity.Code != "c075415b76a3900977" {
		t.Errorf("code 应透传: %q", identity.Code)
	}
	if !identity.Native() {
		t.Error("token+type 齐全时应可用")
	}
}

// TestResolveIdentityFallsBackToConfig 覆盖：凭证没带身份时用全局配置。
func TestResolveIdentityFallsBackToConfig(t *testing.T) {
	identity := ResolveIdentity(nil, map[string]string{
		"qoder_machine_token": "cfg-token",
		"qoder_machine_type":  "cfg-type",
		"qoder_machine_code":  "cfg-code",
	}, MachineIdentity{})
	if identity.Token != "cfg-token" || identity.Type != "cfg-type" || identity.Code != "cfg-code" {
		t.Errorf("应从配置读身份: %+v", identity)
	}
}

// TestResolveIdentityCredentialWinsOverConfig 覆盖：凭证级配置优先于全局配置。
//
// 凭证里的身份属于「这台设备登录的那个账号」，比全局配置更具体。
func TestResolveIdentityCredentialWinsOverConfig(t *testing.T) {
	identity := ResolveIdentity(
		&Credential{MachineToken: "cred-token", MachineType: "cred-type"},
		map[string]string{"qoder_machine_token": "cfg-token", "qoder_machine_type": "cfg-type"},
		MachineIdentity{},
	)
	if identity.Token != "cred-token" || identity.Type != "cred-type" {
		t.Errorf("凭证级身份应优先: %+v", identity)
	}
}

// TestResolveIdentityRefusesHalfIdentity 覆盖：只有一半身份时整体作废。
//
// 上游要求 token 与 type 成对，半套身份解锁不了任何活动；保留它只会让
// 调用方误以为「有身份」，从而在日志里给出错误结论。
func TestResolveIdentityRefusesHalfIdentity(t *testing.T) {
	cases := []struct {
		name string
		cred *Credential
		raw  map[string]string
	}{
		{"token only", &Credential{MachineToken: "tok"}, nil},
		{"type only", &Credential{MachineType: "ty"}, nil},
		{"token in cred, type in config", &Credential{MachineToken: "tok"}, map[string]string{"qoder_machine_type": ""}},
	}
	for _, testCase := range cases {
		identity := ResolveIdentity(testCase.cred, testCase.raw, MachineIdentity{})
		if identity.Native() {
			t.Errorf("%s: 半套/空身份不应可用: %+v", testCase.name, identity)
		}
		if identity.Token != "" || identity.Type != "" {
			t.Errorf("%s: 半套身份应被清空，避免误判: %+v", testCase.name, identity)
		}
	}
}

// TestResolveIdentityPairsAcrossSources 覆盖：token 来自凭证、type 来自配置
// 时仍算成对——两者都真实存在，只是写在不同位置。
func TestResolveIdentityPairsAcrossSources(t *testing.T) {
	identity := ResolveIdentity(&Credential{MachineToken: "tok"}, map[string]string{"qoder_machine_type": "ty"}, MachineIdentity{})
	if !identity.Native() {
		t.Errorf("跨来源拼成的完整身份应可用: %+v", identity)
	}
}

// TestResolveIdentityNeverDerives 覆盖：没有用户身份且本机没有官方客户端时，
// 绝不生成身份。
//
// 这条是本文件最重要的约束——派生值会让活动被上游整条过滤。
func TestResolveIdentityNeverDerives(t *testing.T) {
	disableLocalMachineIdentity(t)
	identity := ResolveIdentity(&Credential{Token: "dt-some-account-token"}, map[string]string{}, MachineIdentity{})
	if identity.Native() {
		t.Errorf("没有用户提供的身份时不得自行生成: %+v", identity)
	}
	if identity.Version != DesktopVersion {
		t.Errorf("version = %q, 期望默认 %q", identity.Version, DesktopVersion)
	}
}

// TestResolveIdentityFallsBackToLocalMachineToken 覆盖：用户没提供身份时，
// 使用本机官方客户端缓存的 machine_token.json（不是插件自己派生）。
func TestResolveIdentityFallsBackToLocalMachineToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine_token.json")
	if errWrite := os.WriteFile(path, []byte(`{"token":"local-tok","type":"local-type"}`), 0o600); errWrite != nil {
		t.Fatalf("write fixture: %v", errWrite)
	}
	t.Setenv("QODER_RUNTIME_INFO", filepath.Join(t.TempDir(), "missing-runtime-info"))
	t.Setenv("QODER_MACHINE_TOKEN_PATH", path)

	identity := ResolveIdentity(nil, nil, LocalMachineIdentity(RegionGlobal))
	if identity.Token != "local-tok" || identity.Type != "local-type" {
		t.Errorf("应使用本机官方身份: %+v", identity)
	}
}

// TestParseRuntimeInfoOutput 覆盖：runtime-info 输出带前缀日志时仍能解析。
func TestParseRuntimeInfoOutput(t *testing.T) {
	identity, okIdentity := parseRuntimeInfoOutput(`log line
{"machineToken":"rt-tok","machineType":"rt-type","machineCode":"rt-code","accountOutcome":"success"}`)
	if !okIdentity {
		t.Fatal("应解析出身份")
	}
	if identity.Token != "rt-tok" || identity.Type != "rt-type" || identity.Code != "rt-code" {
		t.Errorf("解析结果不对: %+v", identity)
	}
	if _, okHalf := parseRuntimeInfoOutput(`{"machineToken":"only-token"}`); okHalf {
		t.Error("缺少 machineType 时应视为无效")
	}
}

// TestRuntimeInfoEnvironmentFollowsRegion 覆盖：runtime-info 的 environment
// 参数按区域选（官方 asar: environment === 'global' ? 3 : 0）。
//
// 国内版账号配国际版 environment 会拿到另一套身份，活动因此被上游过滤，
// 是两个实例必须各自解析身份的直接原因。
func TestRuntimeInfoEnvironmentFollowsRegion(t *testing.T) {
	if got := runtimeInfoEnvironment(RegionCN); got != "0" {
		t.Errorf("国内版 environment = %q, want 0", got)
	}
	if got := runtimeInfoEnvironment(RegionGlobal); got != "3" {
		t.Errorf("国际版 environment = %q, want 3", got)
	}
	if cn, global := clientFor(RegionCN), clientFor(RegionGlobal); cn.dataDir == global.dataDir || cn.product == global.product {
		t.Errorf("两个区域必须指向不同的客户端目录: %+v / %+v", cn, global)
	}
}

// TestIdentityDesktopVersionOverride 覆盖：cosy-version 可被配置覆盖。
func TestIdentityDesktopVersionOverride(t *testing.T) {
	identity := ResolveIdentity(nil, map[string]string{"qoder_desktop_version": "9.9.9"}, MachineIdentity{})
	if identity.Version != "9.9.9" {
		t.Errorf("version = %q, 期望 9.9.9", identity.Version)
	}
	if fallback := ResolveIdentity(nil, nil, MachineIdentity{}); fallback.Version != DesktopVersion {
		t.Errorf("未配置时 version = %q, 期望 %q", fallback.Version, DesktopVersion)
	}
}

// TestNativeRequiresTokenAndType 覆盖：身份可用的判据是两个头成对。
func TestNativeRequiresTokenAndType(t *testing.T) {
	cases := []struct {
		name     string
		identity MachineIdentity
		want     bool
	}{
		{"both", MachineIdentity{Token: "tok", Type: "ty"}, true},
		{"token only", MachineIdentity{Token: "tok"}, false},
		{"type only", MachineIdentity{Type: "ty"}, false},
		{"empty", MachineIdentity{}, false},
	}
	for _, testCase := range cases {
		if got := testCase.identity.Native(); got != testCase.want {
			t.Errorf("%s: Native() = %v, 期望 %v", testCase.name, got, testCase.want)
		}
	}
}

// TestCheckinHeadersOmitMachineWhenUnavailable 覆盖：没有真身份时一个机器头都不发。
//
// 这是国内版能正常领取的前提——发了派生 token 反而会把活动过滤掉。
func TestCheckinHeadersOmitMachineWhenUnavailable(t *testing.T) {
	disableLocalMachineIdentity(t)
	headers := checkinHeaders("dt-token", ResolveIdentity(nil, nil, MachineIdentity{}))
	for _, key := range []string{"cosy-machinetoken", "cosy-machinetype", "cosy-machinecode", "cosy-machineid"} {
		if value, okValue := headers[key]; okValue {
			t.Errorf("无真身份时不应发送 %s = %q", key, value)
		}
	}
	// 桌面端标识仍必须带上：没有它连签到接口都不放行。
	if headers["cosy-clienttype"] != "10" {
		t.Errorf("cosy-clienttype = %q, 期望 10", headers["cosy-clienttype"])
	}
	if headers["cosy-version"] == "" {
		t.Error("cosy-version 不应为空")
	}
}

// TestCheckinHeadersSendProvidedMachineIdentity 覆盖：有真身份时原样发送。
func TestCheckinHeadersSendProvidedMachineIdentity(t *testing.T) {
	identity := MachineIdentity{Token: "tok", Type: "ty", Code: "cd", Version: DesktopVersion}
	headers := checkinHeaders("dt-token", identity)
	if headers["cosy-machinetoken"] != "tok" {
		t.Errorf("cosy-machinetoken = %q", headers["cosy-machinetoken"])
	}
	if headers["cosy-machinetype"] != "ty" {
		t.Errorf("cosy-machinetype = %q", headers["cosy-machinetype"])
	}
	if headers["cosy-machinecode"] != "cd" {
		t.Errorf("cosy-machinecode = %q", headers["cosy-machinecode"])
	}
}
