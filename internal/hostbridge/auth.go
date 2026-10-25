package hostbridge

// 本文件封装宿主提供的凭证回调（host.auth.list / host.auth.get）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// ---- 凭证写回（宿主边界） ----

// SaveAuth 把一份凭证交给宿主热更新（host.auth.save），返回实际写出的 JSON。
//
// **凭证写回只有这一条路径**：uid 归一也在这里做，调用方拿返回值落盘即可，
// 保证「交给宿主的」与「写到磁盘的」是同一份内容（两份不一致会让宿主按
// 文件名与 uid 各登记一次）。
func SaveAuth(callbackID, name string, storageJSON []byte) ([]byte, error) {
	payload := AlignCredentialUID(storageJSON, name)
	_, errCall := Call(callbackID, pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: payload,
	})
	if errCall != nil {
		return payload, errCall
	}
	return payload, nil
}

// AlignCredentialUID 把凭据 JSON 里 uid 的大小写归一成凭证文件名的拼写。
//
// 宿主在 Windows（NTFS 大小写不敏感）上同时按**文件名**与 JSON 里的 **uid**
// 登记账号。两者只差大小写时（zcode-eyJh---1KC8.json vs uid
// zcode-eyjh---1kc8）会被当成两条记录：控制台出现重复卡片、用量按两个
// auth_index 拆分，且重启后依旧存在。
//
// 只改「大小写不敏感相等、但字面不同」的 uid——那正是宿主按 uid 派生出第二种
// 拼写的判据；供应商自有上游 uid（与文件名无关）一律不碰。解析失败原样返回：
// 归一是尽力而为，不能因此阻断写回。
//
// 宿主侧修复（router-for-me/EasyCLIProxyAPI#517）落地后可以删除本函数与
// SaveAuth 里的调用。
func AlignCredentialUID(storageJSON []byte, fileName string) []byte {
	fileID := core.FileIDFromFileName(fileName)
	if fileID == "" || len(storageJSON) == 0 {
		return storageJSON
	}
	// 用 RawMessage 而不是 map[string]any：只替换 uid，其余字段的数值与转义
	// 原样保留（float64 往返会让超过 2^53 的整数丢精度）。
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(storageJSON, &payload); errUnmarshal != nil || payload == nil {
		return storageJSON
	}
	var current string
	if errUnmarshal := json.Unmarshal(payload["uid"], &current); errUnmarshal != nil {
		return storageJSON
	}
	current = strings.TrimSpace(current)
	// 字面已经一致就什么都不用做：EqualFold 为真只说明「大小写等价」，
	// 不代表拼写相同，不能省掉这一层判断。
	if current == "" || current == fileID || !strings.EqualFold(current, fileID) {
		return storageJSON
	}
	aligned, errMarshal := json.Marshal(fileID)
	if errMarshal != nil {
		return storageJSON
	}
	payload["uid"] = aligned
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// 关掉 HTML 转义：Marshaller 默认会把 < > & 写成 \u003c 之类，凭证里
	// 的任意字段都不该因为一次 uid 归一被改写。
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(payload); errEncode != nil {
		return storageJSON
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
}

// listHostAuths 列出本插件名下的全部凭证。
//
// **必须容忍被前置插件抢走的凭证**：宿主遍历插件解析凭证文件时，
// 一旦某个插件报错或认领就中止循环，且会把「当前询问的插件的 identifier」
// 兜底填进 provider。因此归属判定过宽的前置插件（如 qoder2api 判定
// `provider == "qoder"` 即认领）会把本插件的文件抢走，那些账号的 provider
// 变成对方。
//
// 更麻烦的是插件的 priority 可能被宿主覆盖：从插件商店安装/升级时宿主会
// 重写 plugins.configs.<id> 整块配置，手动加的 priority 会被抹掉，
// 顺序又变回按 id 升序（qoder2api < workbuddy2api）。因此**不能只靠优先级**。
//
// 兜底依据：这些凭证文件的文件名仍是 workbuddy-*.json（本插件的命名约定），
// 按名字也属于本插件，因此一并收下——否则用户的合法账号会在页面上凭空消失。
func ListAuths(ctx context.Context, callbackID string, registry *core.Registry) ([]core.HostAuth, error) {
	raw, errCall := Call(callbackID, pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil, fmt.Errorf("host.auth.list: %w", errCall)
	}
	// 宿主可能返回数组，也可能返回带包装的对象；两种都接受。
	var entries []core.HostAuth
	if errUnmarshal := json.Unmarshal(raw, &entries); errUnmarshal == nil {
		return filterPluginAuths(entries, registry), nil
	}
	var wrapper struct {
		Auths []core.HostAuth `json:"auths"`
		Files []core.HostAuth `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", errUnmarshal)
	}
	// 宿主返回的 Auths 是内存运行时实例，Files 是磁盘物理文件。
	// 按文件名将 Files 的文件属性（如 ModTime、Path）合入 Auths，避免将同一凭证重复展示。
	entries = mergeAuthsAndFiles(wrapper.Auths, wrapper.Files)
	return filterPluginAuths(entries, registry), nil
}

// ResolveAuthPath 把宿主给出的凭证文件路径归一化成绝对路径。
//
// 宿主不一定给绝对路径：EasyCLIProxyAPI 把内核的 oauth.auth-dir 配成相对路径
// （默认 ../oauth，相对内核工作目录），CLIProxyAPI 扫盘时把相对路径写进 auth
// 属性，host.auth.list / host.auth.get 又原样返回。插件与宿主是同进程、同工作
// 目录，所以 filepath.Abs 解析出来的就是宿主实际读写的那个文件；这也是宿主自己
// 管理接口 DELETE /v0/management/auth-files 的做法。
//
// 归一化只放在**宿主边界**：Path 是本插件所有路径消费方（删除、stat 取修改时间、
// 读文件兜底、续期写回）的共同输入，各自再换算一次就会出现「有的路径是绝对、
// 有的是相对」的分歧。空值原样返回，交给调用方按「没有文件」处理。
func ResolveAuthPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	abs, errAbs := filepath.Abs(trimmed)
	if errAbs != nil {
		return trimmed
	}
	return abs
}

// mergeAuthsAndFiles 合并宿主 Auths 与 Files，避免同一凭证出现重复条目。
func mergeAuthsAndFiles(auths, files []core.HostAuth) []core.HostAuth {
	if len(files) == 0 {
		return auths
	}
	if len(auths) == 0 {
		return files
	}

	out := make([]core.HostAuth, len(auths))
	copy(out, auths)
	authIdxByName := make(map[string]int, len(auths))
	for i, a := range out {
		if key := normalizeAuthKey(a.Name); key != "" {
			authIdxByName[key] = i
		}
		if key := normalizeAuthKey(a.ID); key != "" {
			authIdxByName[key] = i
		}
	}

	for _, file := range files {
		key := normalizeAuthKey(file.Name)
		if key == "" {
			key = normalizeAuthKey(file.ID)
		}
		if idx, found := authIdxByName[key]; found {
			mergeHostAuthEntry(&out[idx], file)
		} else {
			out = append(out, file)
		}
	}
	return out
}

// filterPluginAuths 只保留属于本插件的凭证，并对重复条目（如同时出现在 Auths 与 Files 中）进行去重合并。
//
// 判据有两层，命中任一即收下：
//  1. provider 是本插件（正常情况）；
//  2. 文件名符合本插件约定（workbuddy-*.json）——用于兜住被前置插件抢走的
//     凭证。只看 provider 会让这些账号从页面上消失，而用户无从知道原因。
//
// 两层都不会误吞别家凭证：别家的文件名不含 workbuddy。
func filterPluginAuths(entries []core.HostAuth, registry *core.Registry) []core.HostAuth {
	out := make([]core.HostAuth, 0, len(entries))
	indexByKey := make(map[string]int)

	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		belongs := (provider != "" && strings.EqualFold(provider, core.ProviderKey)) || fileNameBelongsToPlugin(entry.Name, registry)
		if !belongs {
			continue
		}
		entry.Path = ResolveAuthPath(entry.Path)

		// 归一化去重键：优先用去掉 .json 的 Name，其次用 ID
		key := normalizeAuthKey(entry.Name)
		if key == "" {
			key = normalizeAuthKey(entry.ID)
		}
		if key == "" {
			key = strings.TrimSpace(entry.AuthIndex)
		}

		if idx, exists := indexByKey[key]; exists && key != "" {
			mergeHostAuthEntry(&out[idx], entry)
			continue
		}

		if key != "" {
			indexByKey[key] = len(out)
		}
		out = append(out, entry)
	}
	return out
}

// logHostCallFailure 记录一次宿主回调失败（供降级路径使用）。
func logHostCallFailure(method string, err error) {
	if err == nil {
		return
	}
	logger.Debug("host callback %s failed: %v", method, err)
}

// accountIdentity 返回一个宿主账号条目的**唯一权威身份标识**。
//
// 这是「按账号落状态」的唯一取值点：签到记录、任务历史、额度缓存都必须经它，
// 否则同一个账号会在不同路径上算出不同身份。
//
// 之所以需要它：宿主给同一条账号记录提供了多个标识（ID、Name、AuthIndex），
// 三者并不相等（AuthIndex 是运行期索引，Name 才对应磁盘文件名）。早期代码
// 有的路径传 AuthIndex、有的传 Name，于是「UID 取自文件名」的供应商
// （Qoder/ZCode）在签到与展示上得到两个身份，签到写进 A、页面查 B，
// 永远显示「未签到」。统一收口到本函数后，这类分歧不可能再出现。
//
// 取值优先级：Name（磁盘文件名，登录落盘时生成，可读且稳定）→ ID（宿主
// 稳定标识，运行时账号没有 Name）→ AuthIndex（最后兜底，仅保证非空）。
func AccountIdentity(entry core.HostAuth) string {
	for _, candidate := range []string{entry.Name, entry.ID, entry.AuthIndex} {
		if trimmed := core.FileIDFromFileName(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeAuthKey(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, ".json")
	return s
}

func mergeHostAuthEntry(target *core.HostAuth, source core.HostAuth) {
	if target.AuthIndex == "" && source.AuthIndex != "" {
		target.AuthIndex = source.AuthIndex
	}
	if target.Path == "" && source.Path != "" {
		target.Path = source.Path
	}
	if target.ModTime.IsZero() && !source.ModTime.IsZero() {
		target.ModTime = source.ModTime
	}
	if target.Label == "" && source.Label != "" {
		target.Label = source.Label
	}
	if target.Status == "" && source.Status != "" {
		target.Status = source.Status
	}
	if target.RuntimeOnly && !source.RuntimeOnly {
		target.RuntimeOnly = false
	}
}

// fileNameBelongsToPlugin 判断文件名是否符合本插件的命名约定。
//
// 判据来自供应商注册表而不是硬编码前缀：每个供应商实例的 ID 就是它
// 的凭证文件名前缀（workbuddycn / qoderglobal / cline / opencodezen）。
// 新增供应商时它自动生效，不需要在这里同步一份名单。
//
// 兼容旧前缀 workbuddy：历史凭证文件是 workbuddy-<uid>.json，当时还没有
// cn/global 之分。
func fileNameBelongsToPlugin(name string, registry *core.Registry) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), ".json"))
	if base == "" {
		return false
	}
	if base == "workbuddy" {
		return true
	}
	for _, vendor := range registry.Vendors() {
		id := strings.ToLower(strings.TrimSpace(vendor.ID()))
		if id == "" {
			continue
		}
		if base == id {
			return true
		}
		for _, separator := range []string{"-", "_", "."} {
			if strings.HasPrefix(base, id+separator) ||
				strings.Contains(base, separator+id) {
				return true
			}
		}
	}
	// 旧前缀 workbuddy（未分区时代）：workbuddy-<uid>.json 与 my-workbuddy.json。
	for _, separator := range []string{"-", "_", "."} {
		if strings.HasPrefix(base, "workbuddy"+separator) ||
			strings.Contains(base, separator+"workbuddy") {
			return true
		}
	}
	return false
}

// fetchAuthJSON 按 auth_index 取回凭证 JSON。
//
// 宿主的 host.auth.get 只认 auth_index（按 id 查会失败），
// 因此调用方若是从 id 出发，必须先经列表把 id 映射成 index。
func FetchAuthJSON(ctx context.Context, callbackID, authID string, registry *core.Registry) ([]byte, error) {
	index := strings.TrimSpace(authID)
	if index == "" {
		return nil, fmt.Errorf("auth id is empty")
	}
	// 先直接按 index 查（多数调用方传进来的就是 index）。
	if raw, okFound := AuthJSON(ctx, callbackID, index); okFound {
		return raw, nil
	}
	// 回退：把 id 映射成 index 再查。
	entries, errList := ListAuths(ctx, callbackID, registry)
	if errList != nil {
		return nil, errList
	}
	for _, entry := range entries {
		if entry.ID != index && entry.Name != index {
			continue
		}
		if raw, okFound := AuthJSON(ctx, callbackID, entry.AuthIndex); okFound {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("auth %s not found", authID)
}

// getAuthJSONAndPathByIndex 按 auth_index 取凭证 JSON 和物理文件路径。
func AuthJSONAndPath(ctx context.Context, callbackID, authIndex string) ([]byte, string, bool) {
	trimmed := strings.TrimSpace(authIndex)
	if trimmed == "" {
		return nil, "", false
	}
	raw, errCall := Call(callbackID, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: trimmed})
	if errCall != nil {
		logHostCallFailure(pluginabi.MethodHostAuthGet, errCall)
		return nil, "", false
	}
	var response pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, "", false
	}
	if len(response.JSON) == 0 {
		return nil, "", false
	}
	return response.JSON, ResolveAuthPath(response.Path), true
}

// getAuthJSONByIndex 按 auth_index 取凭证 JSON。
func AuthJSON(ctx context.Context, callbackID, authIndex string) ([]byte, bool) {
	raw, _, ok := AuthJSONAndPath(ctx, callbackID, authIndex)
	return raw, ok
}

// activeVendorSet 返回当前拥有至少一个可用凭证的供应商 ID 集合。
//
// 用于模型列表过滤：如果某个供应商未添加 auth/key，模型列表既不去上游获取，
// 也不在列表里展示该供应商的模型。
func ActiveVendorSet(ctx context.Context, callbackID string, registry *core.Registry) map[string]bool {
	entries, errList := ListAuths(ctx, callbackID, registry)
	if errList != nil {
		logger.Debug("activeVendorSet: list host auths failed: %v", errList)
		return map[string]bool{}
	}
	active := make(map[string]bool)
	for _, entry := range entries {
		if !entry.Usable() {
			continue
		}
		raw, _, okRaw := AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
		if !okRaw && entry.Path != "" {
			if r, err := os.ReadFile(entry.Path); err == nil && len(r) > 0 {
				raw = r
				okRaw = true
			}
		}
		if !okRaw || len(raw) == 0 {
			continue
		}
		if credential, okParse := registry.ParseCredential(raw, entry.Name, nil); okParse {
			if id := credential.VendorIDValue(); id != "" {
				active[id] = true
			}
		}
	}
	return active
}
