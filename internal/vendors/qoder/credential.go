package qoder

// 本文件实现 Qoder 凭证的解析与归属判定。
//
// Qoder 的凭证形态比 WorkBuddy 松散得多（历史原因）：token 字段有七个候选名，
// 还可能是 qoder2api 导出格式把整对凭证塞在 secret 字段的 JSON 字符串里。
// 因此解析要尽可能宽，而**归属判定要尽可能严**——宽进严出。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"freetier2api-plugin/internal/core"
)

// 供应商实例标识。同时是凭证文件名前缀。
const (
	// VendorIDCN 是国内版（qoder.com.cn）。
	VendorIDCN = "qodercn"
	// VendorIDGlobal 是国际版（qoder.com）。
	VendorIDGlobal = "qoderglobal"

	// protocolName 是本协议在文件名里的公共标识段。
	protocolName = "qoder"
)

// Region / RegionCN / RegionGlobal / NormalizeRegion 定义在 chat.go
// ——它们同时服务端点表，属于协议层的基础设施。

// VendorIDFor 返回该区域对应的供应商标识。
func VendorIDFor(region Region) string {
	if region == RegionCN {
		return VendorIDCN
	}
	return VendorIDGlobal
}

// VendorNameFor 返回该区域的展示名。
func VendorNameFor(region Region) string {
	if region == RegionCN {
		return "Qoder 国内版"
	}
	return "Qoder 国际版"
}

// credentialTokenKeys 是凭证里可能承载 token 的字段名。
//
// 七个候选来自历史积累：不同版本的 qoder2api 导出、桌面端抓包、
// 用户手写各有各的写法。全部试一遍比要求用户改文件友好得多。
var credentialTokenKeys = []string{
	"token", "device_token", "access_token", "api_key",
	"personal_token", "pat", "qoder_token",
}

// Credential 是归一化后的 Qoder 凭证。
type Credential struct {
	// Token 是出站凭证：PAT（pt-…）或设备令牌（dt-…）。
	Token string
	// RefreshToken 用于续期（drt-… 前缀）。
	RefreshToken string
	// Region 是账号所属区域。
	Region Region
	// Label 是展示名。
	Label string
	// Email 是账号邮箱（登录流程会填）。
	Email string

	// MachineToken / MachineType / MachineCode 是官方组件签发的设备身份。
	//
	// 上游对活动列表按设备身份做**精确校验**（见 identity.go），插件无法
	// 伪造，只能由用户从自己登录过官方桌面的那台机器上取来填进凭证文件。
	// 为空表示未提供，签到时不发机器头。
	MachineToken string
	MachineType  string
	MachineCode  string
}

// ParseCredential 解析 Qoder 凭证 JSON。
func ParseCredential(raw []byte, fallbackRegion Region) (*Credential, error) {
	cred := &Credential{Region: fallbackRegion}
	if len(raw) == 0 {
		return cred, fmt.Errorf("credential is empty")
	}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return cred, fmt.Errorf("credential is not valid JSON: %w", errUnmarshal)
	}
	cred.Token = firstTokenIn(payload)
	if refresh, okRefresh := payload["refresh_token"].(string); okRefresh {
		cred.RefreshToken = strings.TrimSpace(refresh)
	}

	// qoder2api 的导出格式把整对凭证放在 secret 字段的 JSON 字符串里。
	// 直接导入这类文件（或原样拷贝 export 条目）时必须能解出 dt- 与 drt-。
	if nested, okNested := payload["secret"].(string); okNested && strings.TrimSpace(nested) != "" {
		trimmed := strings.TrimSpace(nested)
		inner := map[string]any{}
		if errInner := json.Unmarshal([]byte(trimmed), &inner); errInner == nil {
			if cred.Token == "" {
				cred.Token = firstTokenIn(inner)
			}
			if cred.RefreshToken == "" {
				if refresh, okRefresh := inner["refresh_token"].(string); okRefresh {
					cred.RefreshToken = strings.TrimSpace(refresh)
				}
			}
		} else if cred.Token == "" {
			// 非 JSON 的 secret 视作直接是凭证（与上游 ParseOAuthSecret 一致）。
			cred.Token = trimmed
		}
	}
	if cred.Token == "" {
		return cred, fmt.Errorf("credential has no token field (tried: %s)", strings.Join(credentialTokenKeys, ", "))
	}
	if region, okRegion := payload["region"].(string); okRegion && strings.TrimSpace(region) != "" {
		cred.Region = NormalizeRegion(region)
	}
	if label, okLabel := payload["label"].(string); okLabel {
		cred.Label = strings.TrimSpace(label)
	}
	if email, okEmail := payload["email"].(string); okEmail {
		cred.Email = strings.TrimSpace(email)
	}
	cred.MachineToken = firstStringIn(payload, "machine_token", "machineToken")
	cred.MachineType = firstStringIn(payload, "machine_type", "machineType")
	cred.MachineCode = firstStringIn(payload, "machine_code", "machineCode")
	return cred, nil
}

// firstStringIn 返回给定键里第一个非空字符串值。
func firstStringIn(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// firstTokenIn 返回一层 JSON 里第一个非空的 token 字段。
func firstTokenIn(payload map[string]any) string {
	for _, key := range credentialTokenKeys {
		if value, okValue := payload[key].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// LooksLikeCredential 判断一份 JSON 是否属于 Qoder（任意区域）。
//
// **判定必须偏严**：宿主会把所有非内建格式的凭证依次喂给每个插件，
// 误吞别家的会让宿主用本插件的结构覆盖对方账号（且是静默的）。
//
// 只接受 Qoder 特有的证据：
//  1. token 值带 Qoder 独有的前缀（pt- / drt-）；
//  2. 文件名含 qoder 段；
//  3. 有 secret 字段（qoder2api 导出格式的特征）。
//
// **刻意不用 dt- 前缀**：WorkBuddy 的凭证也有 device_token 字段且同样用
// dt- 前缀（实测两家的设备令牌前缀相同），拿它当判据必然跨供应商误吞。
func LooksLikeCredential(raw map[string]any, fileName, provider string) bool {
	if hasQoderTokenPrefix(raw) {
		return true
	}
	if _, okSecret := raw["secret"].(string); okSecret {
		return true
	}
	if core.FileNameMatchesConvention(fileName, protocolName, VendorIDCN, VendorIDGlobal) {
		return true
	}
	_ = provider
	return false
}

// hasQoderTokenPrefix 判断凭证里的 token 是否带 Qoder 独有前缀。
//
// pt- 个人访问令牌 / drt- 刷新令牌 —— 这两个前缀是 Qoder 独有的。
// 设备令牌的 dt- 前缀**不在此列**：WorkBuddy 也用同样的前缀。
func hasQoderTokenPrefix(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	for _, key := range append(append([]string{}, credentialTokenKeys...), "refresh_token") {
		value, okValue := raw[key].(string)
		if !okValue {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "pt-") || strings.HasPrefix(trimmed, "drt-") {
			return true
		}
	}
	return false
}

// RegionForCredential 从凭证推断所属区域，推断不出返回空。
//
// 与区域相关的字段只有一个 region（Qoder 的端点表不像 WorkBuddy 那样
// 能从 domain 反推——它压根不写 domain 字段）。
func RegionForCredential(raw map[string]any) Region {
	if raw == nil {
		return ""
	}
	if region, okRegion := raw["region"].(string); okRegion && strings.TrimSpace(region) != "" {
		return NormalizeRegion(region)
	}
	return ""
}

// ---- 活动设备身份 ----
//
// 上游对 cosy-machinetoken + cosy-machinetype 做精确校验，只接受官方组件签发的
// 原始配对。这里只读取用户自己提供的真实身份（凭证字段 → 全局配置），绝不自造。

// MachineIdentity 是一次签到请求要带的设备身份。
type MachineIdentity struct {
	// Token / Type 是成对出现的机器身份，由官方组件签发，必须原样使用。
	Token string
	Type  string
	// Code 是 cosy-machinecode，可选（实测不影响活动可见性）。
	Code string
	// Version 是 cosy-version（桌面端版本）。
	Version string
}

// Native 报告身份是否可用（token 与 type 必须成对，缺一不可）。
func (m MachineIdentity) Native() bool { return m.Token != "" && m.Type != "" }

// DesktopVersion 是活动平台认的桌面端版本（cosy-version 头）。
const DesktopVersion = "0.4.8"

// ResolveIdentity 组装本次请求要用的设备身份。
//
// 优先级：
//  1. 用户显式提供的身份（凭证字段 → 插件全局配置）——原样使用；
//  2. 本机 Qoder 客户端签发的身份（local，见 LocalMachineIdentity）；
//  3. 都没有时返回零值，调用方据此**不发**任何机器头。
//
// 用户只提供一半身份时整体作废且不回退本机：那是用户显式的错误配置，
// 静默换成另一套身份会让问题更难排查。
func ResolveIdentity(cred *Credential, raw map[string]string, local MachineIdentity) MachineIdentity {
	identity := MachineIdentity{
		Version: core.FirstNonEmpty(strings.TrimSpace(raw["qoder_desktop_version"]), DesktopVersion),
	}
	userProvided := false
	if cred != nil {
		identity.Token = strings.TrimSpace(cred.MachineToken)
		identity.Type = strings.TrimSpace(cred.MachineType)
		identity.Code = strings.TrimSpace(cred.MachineCode)
		userProvided = identity.Token != "" || identity.Type != "" || identity.Code != ""
	}
	configToken := strings.TrimSpace(raw["qoder_machine_token"])
	configType := strings.TrimSpace(raw["qoder_machine_type"])
	configCode := strings.TrimSpace(raw["qoder_machine_code"])
	userProvided = userProvided || configToken != "" || configType != "" || configCode != ""
	identity.Token = core.FirstNonEmpty(identity.Token, configToken)
	identity.Type = core.FirstNonEmpty(identity.Type, configType)
	identity.Code = core.FirstNonEmpty(identity.Code, configCode)

	if userProvided {
		if !identity.Native() {
			// 只有一个头等于没有：上游要求成对，半套身份既解锁不了活动，还会
			// 让请求形态偏离官方客户端。清空以免调用方误以为「有身份」。
			return MachineIdentity{Version: identity.Version}
		}
		return identity
	}

	if local.Native() {
		local.Version = identity.Version
		return local
	}
	return MachineIdentity{Version: identity.Version}
}

// LocalMachineIdentity 解析本机官方组件签发的设备身份；拿不到时返回零值。
//
// 上游对 cosy-machinetoken + cosy-machinetype 做精确校验：缺少这一对头时
// /sash/api/v1/me/campaigns 只下发 VIEW_DETAILS，**不会**下发可领取的
// CLAIM_BENEFIT，于是插件误判「今天没有可领取的活动」。
//
// 身份由官方组件 runtime-info 生成，且与 environment 参数绑定：
//  1. 主路径：spawn 本机 Qoder 客户端的 runtime-info <env> --account-stdin；
//  2. 退路：读取客户端缓存的 machine_token.json；
//  3. 都拿不到时返回零值（不发机器头），由用户手工配置兜底。
//
// environment 必须按区域选：官方 asar 里 environment === 'global' ? 3 : 0，
// 国内版账号配国际版 environment 会拿到另一套身份，活动同样被过滤。
func LocalMachineIdentity(region Region) MachineIdentity {
	if identity, okIdentity := readRuntimeInfoIdentity(region); okIdentity {
		return identity
	}
	for _, path := range machineTokenPaths(region) {
		if identity, okIdentity := readMachineTokenFile(path); okIdentity {
			return identity
		}
	}
	return MachineIdentity{}
}

// qoderClient 是本机 Qoder 客户端在该区域的目录特征。
type qoderClient struct {
	// dataDir 是 Unix 下的数据目录名（<home>/<dataDir>）。
	dataDir string
	// product 是 Windows 下 %APPDATA% 里的产品目录名。
	product string
}

// clientFor 返回该区域对应的客户端目录特征。
func clientFor(region Region) qoderClient {
	if region == RegionCN {
		return qoderClient{dataDir: ".qoder-cn", product: "Qoder CN"}
	}
	return qoderClient{dataDir: ".qoder", product: "Qoder"}
}

// runtimeInfoEnvironment 是 runtime-info 的第一个位置参数（官方 asar 口径）。
func runtimeInfoEnvironment(region Region) string {
	if region == RegionCN {
		return "0"
	}
	return "3"
}

// runtimeInfoTimeout 限制单次 spawn（实测正常约 3.8 秒）。
const runtimeInfoTimeout = 20 * time.Second

// readRuntimeInfoIdentity 实时调用官方 runtime-info 生成身份。
func readRuntimeInfoIdentity(region Region) (MachineIdentity, bool) {
	executable, okExecutable := locateRuntimeInfo(region)
	if !okExecutable {
		return MachineIdentity{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeInfoTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, executable, runtimeInfoEnvironment(region), "--account-stdin")
	command.Stdin = strings.NewReader(`{"account":""}`)
	output, errRun := command.Output()
	if errRun != nil {
		return MachineIdentity{}, false
	}
	return parseRuntimeInfoOutput(string(output))
}

// locateRuntimeInfo 定位本机 runtime-info 可执行文件。
//
// 路径形态：<home>/<dataDir>/.bin/umid-<hash>/runtime-info(.exe)。
// 目录名带哈希（随版本变化），因此枚举而不是写死。
func locateRuntimeInfo(region Region) (string, bool) {
	if override := strings.TrimSpace(os.Getenv("QODER_RUNTIME_INFO")); override != "" {
		if fileExists(override) {
			return override, true
		}
		return "", false
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return "", false
	}
	name := "runtime-info"
	if runtime.GOOS == "windows" {
		name = "runtime-info.exe"
	}
	binDir := filepath.Join(home, clientFor(region).dataDir, ".bin")
	entries, errRead := os.ReadDir(binDir)
	if errRead != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "umid-") {
			continue
		}
		candidate := filepath.Join(binDir, entry.Name(), name)
		if fileExists(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// parseRuntimeInfoOutput 从 runtime-info 的 JSON 输出里取出身份。
//
// 输出可能带前缀日志，故从第一个 '{' 开始解析。
func parseRuntimeInfoOutput(output string) (MachineIdentity, bool) {
	start := strings.IndexByte(output, '{')
	if start < 0 {
		return MachineIdentity{}, false
	}
	var payload struct {
		MachineToken string `json:"machineToken"`
		MachineType  string `json:"machineType"`
		MachineCode  string `json:"machineCode"`
	}
	if errUnmarshal := json.Unmarshal([]byte(output[start:]), &payload); errUnmarshal != nil {
		return MachineIdentity{}, false
	}
	token := strings.TrimSpace(payload.MachineToken)
	machineType := strings.TrimSpace(payload.MachineType)
	if token == "" || machineType == "" {
		return MachineIdentity{}, false
	}
	return MachineIdentity{Token: token, Type: machineType, Code: strings.TrimSpace(payload.MachineCode)}, true
}

// machineTokenPaths 返回该区域客户端磁盘缓存的候选路径（退路）。
func machineTokenPaths(region Region) []string {
	if override := strings.TrimSpace(os.Getenv("QODER_MACHINE_TOKEN_PATH")); override != "" {
		return []string{override}
	}
	client := clientFor(region)
	paths := make([]string, 0, 3)
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		paths = append(paths, filepath.Join(appData, client.product, "SharedClientCache", "cache", "machine_token.json"))
	}
	home, errHome := os.UserHomeDir()
	if errHome == nil {
		paths = append(paths,
			filepath.Join(home, "Library", "Application Support", client.product, "SharedClientCache", "cache", "machine_token.json"),
			filepath.Join(home, ".config", client.product, "SharedClientCache", "cache", "machine_token.json"))
	}
	return paths
}

// readMachineTokenFile 读取客户端缓存的身份文件。
func readMachineTokenFile(path string) (MachineIdentity, bool) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return MachineIdentity{}, false
	}
	var payload struct {
		Token string `json:"token"`
		Type  string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return MachineIdentity{}, false
	}
	token := strings.TrimSpace(payload.Token)
	machineType := strings.TrimSpace(payload.Type)
	if token == "" || machineType == "" {
		return MachineIdentity{}, false
	}
	return MachineIdentity{Token: token, Type: machineType}, true
}

func fileExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && !info.IsDir()
}
