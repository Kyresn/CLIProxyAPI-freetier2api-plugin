package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

// StatusPayload 构造账号状态概览（导出给根层测试/兼容包装）。
func (s *Service) StatusPayload(req pluginapi.ManagementRequest) map[string]any {
	return s.buildStatusPayload(req)
}

// buildStatusPayload 构造账号状态概览（不含任何 token）。
func (s *Service) buildStatusPayload(req pluginapi.ManagementRequest) map[string]any {
	cfg := s.deps.Config()
	current := s.deps.State()
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	accounts := s.listAccountSummaries(ctx, hostbridge.RequestCallbackID(req))
	modelCounts := map[string]int{}
	for _, model := range current.Models {
		modelCounts[model.Realm]++
	}
	return map[string]any{
		"ok":                true,
		"plugin":            s.deps.Identity().ID,
		"version":           s.deps.Identity().Version,
		"enabled_realms":    cfg.EnabledRealms,
		"log_level":         logger.CurrentLevel(),
		"state_dir":         cfg.StateDir,
		"accounts":          accounts,
		"account_count":     len(accounts),
		"model_counts":      modelCounts,
		"models_fetched_at": current.ModelsFetchedAt,
		"restart_pending":   current.RestartPending,
		"settings":          s.settingsView(),
		"scheduler":         s.tasks.Status(),
	}
}

// accountSummary 是账号概览条目（不含凭证）。
type accountSummary struct {
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	// FileID 是账号身份（凭据文件名去 .json）：别名的键，也是页面向
	// /accounts/alias 提交的唯一标识。头两个字段（host 的 auth_id /
	// auth_index）会随宿主重扫变化，不能拿来当键。
	FileID string `json:"file_id,omitempty"`
	// Label 是**展示名**，链：账号别名 → 上游 label → host label → 文件名 → ID。
	// 别名是第一优先级，未设置时行为与引入别名之前完全一致。
	Label string `json:"label"`
	// NativeLabel 是去掉别名后的上游名称（上游 label → host label → 文件名 → ID）。
	//
	// 页面需要它来判断「用户填的名字是否就是原名」：填回原名不该存成一条
	// 与上游永远同步不了的别名。展示链里别名会覆盖 Label，因此必须单独给出。
	NativeLabel string `json:"native_label,omitempty"`
	// Alias 是用户自定义别名；空表示未设置。页面据此决定是否跳过 API key
	// 脱敏（别名是用户起的名字，不是凭证）。
	Alias           string               `json:"alias,omitempty"`
	VendorID        string               `json:"vendor_id"`
	VendorName      string               `json:"vendor_name"`
	Realm           string               `json:"realm"`
	Status          string               `json:"status"`
	Disabled        bool                 `json:"disabled"`
	RefreshedAt     string               `json:"refreshed_at,omitempty"`
	Checkin         *state.CheckinRecord `json:"checkin,omitempty"`
	Quota           *quotaResult         `json:"quota,omitempty"`
	SupportsCheckin bool                 `json:"supports_checkin"`
	// SupportsQuota 是「该供应商能否查出额度」的静态能力，与 Quota（查询结果）
	// 是两件事：没有结果时它区分「还没查」与「永远查不出来」。
	SupportsQuota bool `json:"supports_quota"`
	// Claim 是该账号的套餐领取状态；SupportsClaim 为 false 时前端不渲染入口。
	Claim         *state.ClaimRecord `json:"claim,omitempty"`
	SupportsClaim bool               `json:"supports_claim"`
	// CheckinHistory 是该账号最近的签到执行记录（新→旧，最多 checkin_history_limit 条）。
	CheckinHistory []state.CheckinHistoryEntry `json:"checkin_history,omitempty"`
}

// quotaCacheKey 返回额度缓存的唯一键：供应商 + 账号身份（FileID 优先，AuthID 兜底）。
//
// 键必须同时包含供应商与账号身份。额度结果曾按 Name / Label / AuthIndex 多键落库，
// 这些字段跨供应商并不唯一（两个账号同昵称、或同一个 auth_index 都很常见），于是 A
// 账号的额度会被 B 账号读到——OpenCode ZEN 显示 WorkBuddy 的「剩余 / 总额」正是
// 这条路径。
func quotaCacheKey(vendorID, fileID, authID string) string {
	vendorID = strings.TrimSpace(vendorID)
	accountID := core.FirstNonEmpty(fileID, authID)
	if vendorID == "" || accountID == "" {
		return ""
	}
	return vendorID + "\x00" + accountID
}

// getCachedQuota 读取账号额度缓存；身份不完整时视为未命中。
func (s *Service) getCachedQuota(vendorID, fileID, authID string) *quotaResult {
	key := quotaCacheKey(vendorID, fileID, authID)
	if key == "" {
		return nil
	}
	s.quotaMu.RLock()
	defer s.quotaMu.RUnlock()
	q, ok := s.quotaCache[key]
	if !ok {
		return nil
	}
	return &q
}

// cacheQuotaResult 写入账号额度缓存；身份不完整时不写。
func (s *Service) cacheQuotaResult(q quotaResult) {
	key := quotaCacheKey(q.VendorID, q.UID, q.AuthID)
	if key == "" {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	s.quotaCache[key] = q
}

func (s *Service) listAccountSummaries(ctx context.Context, callbackID string) []accountSummary {
	current := s.deps.State()
	loaded, failures := s.pool.Load(ctx, callbackID, auth.Options{IncludeDisabled: true})
	for _, failure := range failures {
		logger.Debug("skip account %s: %s", core.FirstNonEmpty(failure.Entry.Name, failure.Entry.ID), failure.Message)
	}
	out := make([]accountSummary, 0, len(loaded))
	for _, account := range loaded {
		entry := account.Entry
		credential := account.Credential
		vendor := account.Vendor
		uid := credential.FileIDValue()
		alias := strings.TrimSpace(current.AccountAliases[uid])
		native := core.FirstNonEmpty(credential.LabelValue(), entry.Label, entry.Name, entry.ID)
		summary := accountSummary{
			AuthID:          core.FirstNonEmpty(entry.ID, entry.Name),
			AuthIndex:       entry.AuthIndex,
			FileID:          uid,
			Alias:           alias,
			Label:           core.FirstNonEmpty(alias, native),
			NativeLabel:     native,
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           credential.RegionValue(),
			Status:          entry.Status,
			Disabled:        entry.Disabled,
			SupportsCheckin: core.SupportsCheckinFor(vendor, credential),
			SupportsQuota:   supportsQuota(vendor),
			SupportsClaim:   supportsClaim(vendor),
		}
		var modTime time.Time
		if !entry.ModTime.IsZero() {
			modTime = entry.ModTime
		} else if path := core.FirstNonEmpty(account.FilePath, entry.Path); path != "" {
			if info, errStat := os.Stat(path); errStat == nil {
				modTime = info.ModTime()
			}
		}
		if !modTime.IsZero() {
			summary.RefreshedAt = modTime.Format(time.RFC3339)
		}
		if record, okRecord := current.Checkin[uid]; okRecord {
			summary.Checkin = &record
		} else if record, okLegacy := lookupLegacyCheckin(current, credential); okLegacy {
			summary.Checkin = &record
		}
		summary.CheckinHistory = current.CheckinHistory[state.CheckinHistoryKey(vendor.ID(), uid)]
		if record, okRecord := current.Claim[uid]; okRecord {
			claim := record
			summary.Claim = &claim
		}
		// 只有支持额度查询的供应商才读取额度缓存。不支持额度的账号（OpenCode
		// ZEN、Cline 等）不该显示任何「剩余 / 总额」，即便缓存里存在同名字段。
		if supportsQuota(vendor) {
			if q := s.getCachedQuota(vendor.ID(), uid, summary.AuthID); q != nil {
				summary.Quota = q
				// A successful quota read proves the credential is usable even when
				// the host retained a stale runtime error from an earlier request.
				// Do not mutate host auth state; only avoid displaying a contradictory
				// error in this plugin's account overview.
				summary.Status = displayedAccountStatus(summary.Status, q.OK)
			}
		}
		out = append(out, summary)
	}
	return out
}

func displayedAccountStatus(status string, quotaOK bool) string {
	if strings.EqualFold(strings.TrimSpace(status), "error") && quotaOK {
		return "active"
	}
	return status
}

// supportsQuota 报告该供应商是否提供额度查询。
//
// 能力来自接口断言，而不是「查询结果里有没有这一条」——后者会让「不支持」
// 和「还没查」看起来一样，页面于是永远分不清该显示「-- / --」还是「查询中…」。
func supportsQuota(vendor core.Vendor) bool {
	_, okQuota := core.As[core.QuotaProvider](vendor)
	return okQuota
}

// supportsClaim 报告该供应商是否提供套餐领取。
func supportsClaim(vendor core.Vendor) bool {
	_, okClaimer := core.As[core.PlanClaimer](vendor)
	return okClaimer
}

// lookupLegacyCheckin 读取「账号身份迁移前的 UID 键」下的签到记录（只读回退）。
func lookupLegacyCheckin(current state.State, credential *core.Credential) (state.CheckinRecord, bool) {
	legacyUID := credential.UIDValue()
	if legacyUID == "" || legacyUID == credential.FileIDValue() {
		return state.CheckinRecord{}, false
	}
	record, okRecord := current.Checkin[legacyUID]
	return record, okRecord
}

func (s *Service) buildLogsPayload(req pluginapi.ManagementRequest) map[string]any {
	since := queryInt64(req.Query, "since", 0)
	limit := int(queryInt64(req.Query, "limit", 200))
	entries, newest := logger.Snapshot(uint64(since), limit)
	return map[string]any{"ok": true, "entries": entries, "next": newest}
}

type accountBatchRequest struct {
	AccountIDs []string `json:"account_ids"`
}

// accountEnabledRequest 是启用/停用账号的请求体。
type accountEnabledRequest struct {
	AccountIDs []string `json:"account_ids"`
	Enabled    bool     `json:"enabled"`
}

// handleAccountEnabledRequest 批量启用/停用账号。
//
// 停用不是删除：凭证文件保留，只在 JSON 上写宿主认的 "disabled" 字段，宿主
// 扫盘后该账号不再参与路由；启用即删掉该字段。两者都不动 token，随时可逆。
func (s *Service) handleAccountEnabledRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountEnabledRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	if len(body.AccountIDs) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids 不能为空"})
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	entries, errList := hostbridge.ListAuths(ctx, callbackID, s.registry)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "error": "读取账号列表失败：" + errList.Error()})
	}
	wanted := wantedAccountIDs(body.AccountIDs)
	results := make([]accountActionResult, 0, len(body.AccountIDs))
	changed := 0
	for _, entry := range entries {
		if !wanted.matches(entry) || entry.Disabled == !body.Enabled {
			// 已经是目标状态的不重复写盘：宿主扫盘会再触发一次重载。
			continue
		}
		result := accountActionResult{
			AuthID: core.FirstNonEmpty(entry.ID, entry.Name),
			Label:  core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
		}
		if errSet := s.setAuthDisabled(ctx, callbackID, entry, !body.Enabled); errSet != nil {
			result.Message = errSet.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		if body.Enabled {
			result.Message = "已启用"
		} else {
			result.Message = "已禁用"
		}
		changed++
		results = append(results, result)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "changed": changed, "results": results})
}

// wantedAccountIDs 把页面提交的账号标识集合包成匹配器。
//
// 页面用 auth_id 作为勾选键（账号表里的唯一稳定标识），但宿主的三个标识
// （ID / Name / AuthIndex）都可能在重扫后变化，统一在这里比对一次。
func wantedAccountIDs(targets []string) accountIDSet {
	set := make(accountIDSet, len(targets))
	for _, target := range targets {
		set[strings.TrimSpace(target)] = true
	}
	return set
}

type accountIDSet map[string]bool

func (s accountIDSet) matches(entry core.HostAuth) bool {
	return s[entry.ID] || s[entry.Name] || s[entry.AuthIndex]
}

func (s *Service) handleAccountDeleteRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	if len(body.AccountIDs) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "account_ids 不能为空"})
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	entries, errList := hostbridge.ListAuths(ctx, callbackID, s.registry)
	if errList != nil {
		return JSONResponse(http.StatusOK, map[string]any{"ok": false, "error": "读取账号列表失败：" + errList.Error()})
	}
	wanted := wantedAccountIDs(body.AccountIDs)
	results := make([]accountActionResult, 0, len(body.AccountIDs))
	deleted := 0
	for _, entry := range entries {
		if !wanted.matches(entry) {
			continue
		}
		result := accountActionResult{
			AuthID: core.FirstNonEmpty(entry.ID, entry.Name),
			Label:  core.FirstNonEmpty(entry.Label, entry.Name, entry.ID),
		}
		if errDelete := s.deleteAuthFile(ctx, callbackID, entry); errDelete != nil {
			result.OK = false
			result.Message = errDelete.Error()
			results = append(results, result)
			continue
		}
		result.OK = true
		result.Message = "已删除"
		deleted++
		results = append(results, result)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "deleted": deleted, "results": results})
}

func (s *Service) deleteAuthFile(ctx context.Context, callbackID string, entry core.HostAuth) error {
	raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("读取凭证失败，无法确认归属")
	}
	targetPath, errSafe := safeAuthFilePath(s.registry, entry, raw, core.FirstNonEmpty(filePath, entry.Path))
	if errSafe != nil {
		return errSafe
	}
	if errRemove := os.Remove(targetPath); errRemove != nil {
		return fmt.Errorf("删除文件失败：%w", errRemove)
	}
	logger.Info("deleted auth file %s (vendor=%s)", filepath.Base(targetPath),
		credentialVendorLabel(s.registry, raw, filepath.Base(targetPath)))
	return nil
}

// credentialVendorLabel 反查凭证真正的供应商标识，供日志使用。
//
// 宿主的 auth 记录只有 provider（=本插件）与文件名，Name 常常就是文件名本身；
// 直接把它当 vendor 写日志会出现 vendor=codearts-codearts-user.json 这种误导
// （issue #58）。查不到归属时写 unknown，不拿文件名冒充。
func credentialVendorLabel(registry *core.Registry, raw []byte, fileName string) string {
	credential, okCredential := registry.ParseCredential(raw, fileName, nil)
	if !okCredential {
		return "unknown"
	}
	return core.FirstNonEmpty(credential.VendorIDValue(), "unknown")
}

// setAuthDisabled 在凭证文件上写/清 disabled 标记。
//
// 写回沿用凭证保存的两条路径（见 authfile.go）：优先 host.auth.save，宿主会
// 同时更新磁盘与内存注册表；不可用时才直接写盘，让宿主自己扫到。校验与删除
// 同源——先确认这份凭证属于本插件、路径是可写的普通 .json。
func (s *Service) setAuthDisabled(ctx context.Context, callbackID string, entry core.HostAuth, disabled bool) error {
	raw, filePath, okRaw := hostbridge.AuthJSONAndPath(ctx, callbackID, entry.AuthIndex)
	if !okRaw {
		return fmt.Errorf("读取凭证失败，无法确认归属")
	}
	targetPath, errSafe := safeAuthFilePath(s.registry, entry, raw, core.FirstNonEmpty(filePath, entry.Path))
	if errSafe != nil {
		return errSafe
	}
	updated, errUpdate := setCredentialDisabled(raw, disabled)
	if errUpdate != nil {
		return errUpdate
	}
	if errSave := saveAuthFile(callbackID, filepath.Base(targetPath), updated); errSave != nil {
		return errSave
	}
	logger.Info("auth file %s disabled=%t (vendor=%s)", filepath.Base(targetPath), disabled,
		credentialVendorLabel(s.registry, raw, filepath.Base(targetPath)))
	return nil
}

type accountAddRequest struct {
	Vendor string `json:"vendor"`
	APIKey string `json:"api_key"`
	Label  string `json:"label"`
	// CredentialJSON 是供应商原生凭证 JSON（token 对等复杂凭证）。
	// 提供时优先于 api_key：原样交给供应商的 Parse 归一，不套 api_key 信封。
	CredentialJSON string `json:"credential_json,omitempty"`
}

func (s *Service) handleAccountAddRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountAddRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	vendorID := strings.TrimSpace(body.Vendor)
	apiKey := strings.TrimSpace(body.APIKey)
	label := strings.TrimSpace(body.Label)
	if vendorID == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "vendor 不能为空"})
	}
	credentialJSON := strings.TrimSpace(body.CredentialJSON)
	if apiKey == "" && credentialJSON == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "api_key 不能为空"})
	}
	vendor, okVendor := s.registry.ByID(vendorID)
	if !okVendor {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("未知供应商 %q", vendorID)})
	}
	var rawJSON []byte
	seed := label
	if credentialJSON != "" {
		// 原生凭证 JSON：补 vendor 归属字段后原样落盘，交给供应商 Parse 归一。
		var doc map[string]any
		if err := json.Unmarshal([]byte(credentialJSON), &doc); err != nil || doc == nil {
			return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "credential_json 不是合法的 JSON 对象"})
		}
		doc[core.VendorKey] = vendor.ID()
		encoded, errMarshal := json.MarshalIndent(doc, "", "  ")
		if errMarshal != nil {
			return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errMarshal.Error()})
		}
		rawJSON = encoded
		if seed == "" {
			seed = core.RandomHex(8)
		}
	} else {
		payload := map[string]any{
			"type":         s.deps.Identity().ProviderKey,
			core.VendorKey: vendor.ID(),
			"api_key":      apiKey,
		}
		if label != "" {
			payload["label"] = label
		}
		encoded, errMarshal := json.MarshalIndent(payload, "", "  ")
		if errMarshal != nil {
			return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errMarshal.Error()})
		}
		rawJSON = encoded
		if seed == "" {
			seed = maskedAPIKey(apiKey)
		}
	}
	if seed == "" {
		seed = core.RandomHex(8)
	}
	fileName := core.FileNameFor(vendor.ID(), seed)
	callbackID := hostbridge.RequestCallbackID(req)
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	if errLimit := s.pool.CheckVendorLimit(ctx, callbackID, vendor, fileName); errLimit != nil {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": errLimit.Error()})
	}

	if errSave := saveAuthFile(callbackID, fileName, rawJSON); errSave != nil {
		return JSONResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": errSave.Error()})
	}
	logger.Info("added auth file %s for vendor %s", fileName, vendor.ID())
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "auth_id": fileName, "message": "账号保存成功"})
}

// maskedAPIKey 生成 API Key 的脱敏种子（Cline/OpenCode ZEN 等 key 型凭证）。
func maskedAPIKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 8 {
		if trimmed == "" {
			return ""
		}
		return "***"
	}
	return trimmed[:4] + "***" + trimmed[len(trimmed)-4:]
}

// accountAliasRequest 是设置/清除账号别名的请求体。
type accountAliasRequest struct {
	FileID string `json:"file_id"`
	Alias  string `json:"alias"`
}

// handleAccountAliasRequest 设置或清除某个账号的展示别名。
//
// 键用账号身份（FileID）而不是昵称/邮箱：token 刷新、重登都不改凭证文件名，
// 别名因此自动保留，也不会因为两个账号同昵称而互相覆盖。
//
// 别名是纯展示字段（不参与任何 ID 解析），所以允许空格等字符，只做首尾裁剪；
// 空串表示清除，恢复上游名称。不需要重建宿主模型注册表——账号名只影响本页。
func (s *Service) handleAccountAliasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountAliasRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	fileID := strings.TrimSpace(body.FileID)
	if fileID == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "file_id 不能为空"})
	}
	alias := strings.TrimSpace(body.Alias)
	s.deps.MutateState(func(current *state.State) {
		if alias == "" {
			delete(current.AccountAliases, fileID)
			return
		}
		if current.AccountAliases == nil {
			current.AccountAliases = map[string]string{}
		}
		current.AccountAliases[fileID] = alias
	})
	logger.Info("account %s alias: %q", fileID, alias)
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "alias": alias})
}

func (s *Service) handleCheckinRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	results := s.runForAccounts(ctx, hostbridge.RequestCallbackID(req), body.AccountIDs,
		func(vendor core.Vendor, _ *core.Credential) bool { return core.SupportsCheckin(vendor) },
		func(ctx context.Context, vendor core.Vendor, credential *core.Credential) (accountActionOutcome, error) {
			// 供应商级能力为真时仍要按账号复核：企业账号天生没有签到活动，
			// 如实报「跳过 + 原因」，而不是让上游 400 变成一条失败记录。
			if !core.SupportsCheckinFor(vendor, credential) {
				reason := core.FirstNonEmpty(core.CheckinUnsupportedReason(vendor, credential), "本账号不支持签到")
				return accountActionOutcome{Message: reason, Skipped: true}, nil
			}
			result, errCheckin := s.tasks.Checkin(ctx, vendor, credential, tasks.CheckinTriggerManual)
			if errCheckin != nil {
				return accountActionOutcome{}, errCheckin
			}
			// NotRecorded 表示这次签到**没有真正完成**（上游窗口未开启、
			// 领取被拒、未确认发放）。如实标成「跳过」而不是成功，控制台
			// 就不会显示「签到完成」而用户实际没拿到积分。
			return accountActionOutcome{Message: result.Message, Skipped: result.NotRecorded}, nil
		})
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

type accountActionResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
	// Skipped 报告本次动作「没有真正完成」。
	//
	// 它与 OK 区分开：签到类动作可能请求成功但**什么都没领到**（上游窗口
	// 未开启、领取被风控拒绝、未能确认发放）。把这类结果算作成功会让
	// 控制台显示「签到完成」，用户以为领到了而实际没到账。
	Skipped bool `json:"skipped,omitempty"`
}

// accountActionOutcome 是一次账号动作的结果。
type accountActionOutcome struct {
	// Message 是可读说明。
	Message string
	// Skipped 报告动作「请求成功但没有真正完成」。
	Skipped bool
}

func (s *Service) runForAccounts(
	ctx context.Context,
	callbackID string,
	targets []string,
	filter func(core.Vendor, *core.Credential) bool,
	action func(context.Context, core.Vendor, *core.Credential) (accountActionOutcome, error),
) []accountActionResult {
	cfg := s.deps.Config()
	loaded, failures := s.pool.Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: cfg.EnabledRealms,
		Filter: func(account auth.Account) bool {
			return filter == nil || filter(account.Vendor, account.Credential)
		},
	})
	results := make([]accountActionResult, 0, len(loaded)+len(failures))
	for _, failure := range failures {
		results = append(results, accountActionResult{
			AuthID: core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:  core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			OK:     false, Message: failure.Message,
		})
	}
	jobResults := auth.RunBatch(loaded, batchConcurrency, func(current auth.Account) accountActionResult {
		result := accountActionResult{
			AuthID:   core.FirstNonEmpty(current.Entry.ID, current.Entry.Name),
			Label:    core.FirstNonEmpty(current.Credential.LabelValue(), current.Entry.Label),
			VendorID: current.Vendor.ID(),
			Realm:    current.Credential.RegionValue(),
		}
		outcome, errAction := action(ctx, current.Vendor, current.Credential)
		if errAction != nil {
			result.OK = false
			result.Message = errAction.Error()
		} else {
			result.OK = true
			result.Skipped = outcome.Skipped
			result.Message = outcome.Message
		}
		return result
	})
	return append(results, jobResults...)
}
