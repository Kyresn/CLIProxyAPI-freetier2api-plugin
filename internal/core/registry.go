package core

// 本文件提供显式的供应商注册表。
//
// Registry 由组合根构造并注入到 app / admin / tasks / auth 与各供应商，
// 不再使用包级全局注册表：这样同一个进程里的多个 App 实例彼此隔离，
// 测试也不需要 ResetForTest。

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// Registry 是供应商实例的有序注册表。
type Registry struct {
	mu    sync.RWMutex
	order []string
	byID  map[string]Vendor
	// legacy 是「拆分前的旧 vendorID」→「它演变成的实例 ID」。
	//
	// 只由实现了 LegacyIDReporter 的实例在 Register 时登记；用于让用户在
	// disabled_models / 模型别名里写的旧前缀继续生效。
	legacy map[string][]string
}

// NewRegistry 构造一个空注册表。
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Vendor{}, legacy: map[string][]string{}}
}

// Register 注册一个供应商实例；重复或空 ID 返回错误。
func (r *Registry) Register(v Vendor) error {
	if v == nil {
		return NewPluginError("vendor_invalid", "nil vendor", 500)
	}
	id := strings.ToLower(strings.TrimSpace(v.ID()))
	if id == "" {
		return NewPluginError("vendor_invalid", "vendor ID is empty", 500)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[id]; exists {
		return NewPluginError("vendor_duplicate", "duplicate vendor ID: "+id, 500)
	}
	r.byID[id] = v
	r.order = append(r.order, id)
	if reporter, okReporter := v.(LegacyIDReporter); okReporter {
		for _, legacyID := range reporter.LegacyIDs() {
			legacyID = strings.ToLower(strings.TrimSpace(legacyID))
			// 旧 ID 已被某个现役实例占用时以现役为准（ByID 优先），不登记。
			if legacyID == "" || legacyID == id {
				continue
			}
			r.legacy[legacyID] = appendUniqueID(r.legacy[legacyID], id)
		}
	}
	return nil
}

// appendUniqueID 追加不重复的 ID（注册顺序保持不变）。
func appendUniqueID(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

// Vendors 返回全部供应商（按注册顺序）。
func (r *Registry) Vendors() []Vendor {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Vendor, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// ByID 按 ID 取供应商。
func (r *Registry) ByID(id string) (Vendor, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.byID[strings.ToLower(strings.TrimSpace(id))]
	return v, ok
}

// Resolve 按 vendor 字段 -> 文件名前缀 -> 内容嗅探的顺序判定凭证归属。
//
// 声明或文件名里出现的是**家族**标识时（拆分前的旧 ID、cn / global 区域
// 别名），交给各实例自己的 Match 定位；现役 ID 保持「声明即归属」的原语义。
func (r *Registry) Resolve(fileName, provider string, raw map[string]any) (Vendor, bool) {
	if r == nil {
		return nil, false
	}
	if raw != nil {
		if rawVendor, okVendor := raw[VendorKey].(string); okVendor {
			if v, ok := r.ByID(rawVendor); ok {
				return v, true
			}
			if v, ok := r.resolveFamily(rawVendor, fileName, provider, raw); ok {
				return v, true
			}
		}
	}

	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		r.mu.RLock()
		ids := make([]string, len(r.order))
		copy(ids, r.order)
		legacyIDs := make([]string, 0, len(r.legacy))
		for legacyID := range r.legacy {
			legacyIDs = append(legacyIDs, legacyID)
		}
		r.mu.RUnlock()
		sortByLengthDesc(ids)
		for _, id := range ids {
			if FileNameMatchesConvention(base, id) {
				return r.byID[id], true
			}
		}
		// 旧 ID 的文件名（zcode-*.json）仍要归到它演变成的实例：这类文件只
		// 说明家族，落哪个实例见 resolveFamily。缺这段，存量文件会落到内容
		// 嗅探，被先注册的 apikey 型供应商抢走（线上真实发生过）。
		sortByLengthDesc(legacyIDs)
		for _, legacyID := range legacyIDs {
			if !FileNameMatchesConvention(base, legacyID) {
				continue
			}
			if v, ok := r.resolveFamily(legacyID, fileName, provider, raw); ok {
				return v, true
			}
		}
	}

	for _, v := range r.Vendors() {
		if v.Match(fileName, provider, raw) {
			return v, true
		}
	}
	return nil, false
}

// resolveFamily 把家族标识（拆分前的旧 ID、cn / global 区域别名）落到实例。
//
// 先让各实例自己的 Match 按内容判定（旧凭证可能带 region），都不认领时退回
// 该家族声明的第一个实例——拆分前那种凭证只可能属于这一个，宁可归它也不
// 让内容嗅探把文件判给别家。
func (r *Registry) resolveFamily(family, fileName, provider string, raw map[string]any) (Vendor, bool) {
	ids := r.VendorIDsForPrefix(family)
	for _, id := range ids {
		if v, ok := r.ByID(id); ok && v.Match(fileName, provider, raw) {
			return v, true
		}
	}
	for _, id := range ids {
		if v, ok := r.ByID(id); ok {
			return v, true
		}
	}
	return nil, false
}

// sortByLengthDesc 按「长名优先、同长字典序」排序，保证前缀包含关系里
// 更具体的名字（zcodeglobal）先于短名（zcode）被文件名匹配到。
func sortByLengthDesc(names []string) {
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
}

// ParseCredential 归属并解析一份凭证，同时兜底账号身份。
func (r *Registry) ParseCredential(raw []byte, fileNameHint string, attributes map[string]string) (*Credential, bool) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return nil, false
	}
	fileName := strings.TrimSpace(fileNameHint)
	if fileName != "" && !strings.HasSuffix(strings.ToLower(fileName), ".json") {
		fileName += ".json"
	}
	vendor, okVendor := r.Resolve(fileName, attributes["provider"], payload)
	if !okVendor {
		return nil, false
	}
	credential, errParse := vendor.Parse(raw, fileName)
	if errParse != nil {
		return nil, false
	}
	if credential.FileIDValue() == "" {
		credential.SetFileID(FileIDFromFileName(fileName))
	}
	applyAttributeOverrides(credential, attributes)
	return credential, true
}

// SplitModelID 拆分模型 ID；只有已知前缀才剥离。
//
// 「已知」= 现役供应商 ID、拆分前的旧 ID，或 cn / global 区域别名。
func (r *Registry) SplitModelID(modelID string) (vendorID, bareModel string) {
	trimmed := strings.TrimSpace(modelID)
	index := strings.Index(trimmed, ":")
	if index < 0 {
		return "", trimmed
	}
	prefix := strings.ToLower(strings.TrimSpace(trimmed[:index]))
	if len(r.VendorIDsForPrefix(prefix)) == 0 {
		return "", trimmed
	}
	return prefix, trimmed[index+1:]
}

// VendorIDsForPrefix 把模型前缀展开成现役供应商实例 ID。
//
//   - 现役 ID：返回自身；
//   - 拆分前的旧 ID：返回它演变成的实例（通常一个，如 zcode → zcodeglobal）；
//   - 区域别名（cn / global）：返回该区域的全部实例；
//   - 其它：返回 nil，调用方应把这个前缀当成模型名的一部分。
//
// 拆分实例会让旧 vendorID 从注册表消失，用户存量配置里的 <旧ID>:<模型>
// 若只按现役 ID 索引就会静默失效；统一走本方法即可自然兼容。
func (r *Registry) VendorIDsForPrefix(prefix string) []string {
	if r == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(prefix))
	if key == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.byID[key]; ok {
		return []string{key}
	}
	if ids := r.legacy[key]; len(ids) > 0 {
		return append([]string(nil), ids...)
	}
	if !isRegionAlias(key) {
		return nil
	}
	ids := make([]string, 0, 2)
	for _, id := range r.order {
		if r.byID[id].Region() == key {
			ids = append(ids, id)
		}
	}
	return ids
}

// ModelKeyFor 生成模型禁用键 <vendorID>:<裸名>。
func (r *Registry) ModelKeyFor(vendorID, modelID string) string {
	_, bare := r.SplitModelID(modelID)
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(bare)
}

// FileNameFor 生成 <vendor>-<标识>.json。
//
// 标识为空时按 seeds（通常是 access token）的稳定指纹兜底，**不能**退到固定
// 占位名：上游不给 uid 的供应商（AutoClaw 国内版的 token 不是 JWT）一旦落到
// 同一个固定文件名，第二个账号会静默覆盖第一个。
//
// 指纹对同一份凭证稳定（重登覆盖同一文件），对不同凭证各不相同；两者都拿不到
// 时只能退到随机串——那只保证互不覆盖，保证不了重登幂等，调用方有身份字段
// （uid / userId / 邮箱 / 手机号）时必须传进来。
func FileNameFor(vendorID, identifier string, seeds ...string) string {
	vendorID = strings.ToLower(strings.TrimSpace(vendorID))
	identifier = SanitizeFileComponent(identifier)
	if identifier == "" {
		for _, seed := range seeds {
			if fingerprint := TokenFingerprint(seed); fingerprint != "" {
				// "t" 前缀：指纹本身也是十六进制串，不加前缀会与真实 uid 撞名。
				identifier = "t" + fingerprint
				break
			}
		}
	}
	if identifier == "" {
		identifier = RandomHex(8)
	}
	return vendorID + "-" + identifier + ".json"
}

// FileIDFromFileName 返回凭证文件名去掉 .json 后的账号身份。
//
// 扩展名按大小写不敏感剥离：Windows 上用户改出来的 X.JSON 与 x.json 是同一个
// 文件，身份里若留着扩展名，同一账号会被算成两个。
func FileIDFromFileName(fileName string) string {
	trimmed := strings.TrimSpace(fileName)
	const ext = ".json"
	if len(trimmed) > len(ext) && strings.EqualFold(trimmed[len(trimmed)-len(ext):], ext) {
		return trimmed[:len(trimmed)-len(ext)]
	}
	return trimmed
}

// SanitizeFileComponent 只保留文件名安全字符并截断。
func SanitizeFileComponent(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '_', r == '-', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), "-.@")
}

func isRegionAlias(prefix string) bool {
	switch prefix {
	case "cn", "global":
		return true
	}
	return false
}
