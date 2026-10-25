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
}

// NewRegistry 构造一个空注册表。
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Vendor{}}
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
	return nil
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
func (r *Registry) Resolve(fileName, provider string, raw map[string]any) (Vendor, bool) {
	if r == nil {
		return nil, false
	}
	if raw != nil {
		if rawVendor, okVendor := raw[VendorKey].(string); okVendor {
			if v, ok := r.ByID(rawVendor); ok {
				return v, true
			}
		}
	}

	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base != "" {
		r.mu.RLock()
		ids := make([]string, len(r.order))
		copy(ids, r.order)
		r.mu.RUnlock()
		sort.Slice(ids, func(i, j int) bool {
			if len(ids[i]) != len(ids[j]) {
				return len(ids[i]) > len(ids[j])
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids {
			if FileNameMatchesConvention(base, id) {
				return r.byID[id], true
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

// SplitModelID 拆分模型 ID；只有已知供应商或区域别名才剥离前缀。
func (r *Registry) SplitModelID(modelID string) (vendorID, bareModel string) {
	trimmed := strings.TrimSpace(modelID)
	index := strings.Index(trimmed, ":")
	if index < 0 {
		return "", trimmed
	}
	prefix := strings.ToLower(strings.TrimSpace(trimmed[:index]))
	if _, ok := r.ByID(prefix); !ok && !isRegionAlias(prefix) {
		return "", trimmed
	}
	return prefix, trimmed[index+1:]
}

// ModelKeyFor 生成模型禁用键 <vendorID>:<裸名>。
func (r *Registry) ModelKeyFor(vendorID, modelID string) string {
	_, bare := r.SplitModelID(modelID)
	return strings.ToLower(strings.TrimSpace(vendorID)) + ":" + strings.TrimSpace(bare)
}

// FileNameFor 生成 <vendor>-<标识>.json。
func FileNameFor(vendorID, identifier string) string {
	vendorID = strings.ToLower(strings.TrimSpace(vendorID))
	identifier = SanitizeFileComponent(identifier)
	if identifier == "" {
		identifier = "account"
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
