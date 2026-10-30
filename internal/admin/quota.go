package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/state"
)

// quotaJob 是额度查询里的一个账号任务。
type quotaJob struct {
	entry      core.HostAuth
	vendor     core.Vendor
	credential *core.Credential
}

// identity 返回该任务的账号身份字段（结果与缓存共用）。
func (j quotaJob) identity() quotaResult {
	return quotaResult{
		AuthID:     core.FirstNonEmpty(j.entry.ID, j.entry.Name),
		AuthIndex:  j.entry.AuthIndex,
		Name:       j.entry.Name,
		UID:        j.credential.FileIDValue(),
		Label:      core.FirstNonEmpty(j.credential.LabelValue(), j.entry.Label, j.entry.Name),
		VendorID:   j.vendor.ID(),
		VendorName: j.vendor.Name(),
		Realm:      j.credential.RegionValue(),
	}
}

// quotaResult 是单个账号的额度结果。
type quotaResult struct {
	AuthID     string `json:"auth_id"`
	AuthIndex  string `json:"auth_index,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
	Label      string `json:"label"`
	VendorID   string `json:"vendor_id,omitempty"`
	VendorName string `json:"vendor_name,omitempty"`
	Realm      string `json:"realm"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Remain     int64  `json:"remain"`
	Total      int64  `json:"total"`
	// Unlimited 报告剩余额度无上限，展示层据此显示「无限」。
	Unlimited bool                    `json:"unlimited,omitempty"`
	Metrics   []pluginapi.QuotaMetric `json:"metrics,omitempty"`
}

// quotaBatchOutcome 是一轮批量查询里单个账号的产出。
//
// 领取状态与额度结果分开携带：额度结果会进缓存并被多次读，领取状态只在
// 批末统一落盘一次，两者生命周期不同。
type quotaBatchOutcome struct {
	result quotaResult
	claim  *pluginapi.ClaimStatus
}

func (s *Service) handleQuotasRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	cfg := s.deps.Config()
	loaded, failures := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:       body.AccountIDs,
		EnabledRealms: cfg.EnabledRealms,
	})
	results := make([]quotaResult, 0, len(failures)+len(loaded))
	for _, failure := range failures {
		results = append(results, quotaResult{
			AuthID:  core.FirstNonEmpty(failure.Entry.ID, failure.Entry.Name),
			Label:   core.FirstNonEmpty(failure.Entry.Label, failure.Entry.Name, failure.Entry.ID),
			Message: failure.Message,
		})
	}
	if len(loaded) == 0 {
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}

	// 查询跑在**后台作用域**（空 callback ID + 独立超时），不跟随本次管理 RPC：
	// 页面刷新/关闭会让宿主取消这条 RPC，若查询挂在它的 context 上，这一轮
	// 一个账号都写不进缓存，账号表就只能显示「查询中…」。
	// 已有一轮在跑时不重复打上游：等它结束，再回读缓存。
	jobs := make([]quotaJob, 0, len(loaded))
	for _, account := range loaded {
		job := quotaJob{entry: account.Entry, vendor: account.Vendor, credential: account.Credential}
		// 不支持额度查询的供应商（如 OpenCode ZEN）不进查询轮次、不写缓存，
		// 也不进 /quotas 结果——页面据 /status 的 supports_quota=false 直接显示
		// 「-- / --」，不需要一条注定失败的结果。
		if !supportsQuota(account.Vendor) {
			continue
		}
		jobs = append(jobs, job)
	}
	if len(jobs) == 0 {
		// 全是失败项或不支持额度的账号：没有要跑的一轮，直接回结果。
		return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
	}
	done, owner := s.acquireQuotaRun()
	if owner {
		go s.runQuotaBatch(done, jobs)
	}
	select {
	case <-done:
	case <-ctx.Done():
		// 页面已走开：不阻塞后台那一轮，直接回当前缓存（可能只完成了一部分）。
	}

	for _, job := range jobs {
		identity := job.identity()
		if cached := s.getCachedQuota(identity.VendorID, identity.UID, identity.AuthID); cached != nil {
			results = append(results, *cached)
			continue
		}
		identity.Message = "额度查询尚未完成，请稍后刷新"
		results = append(results, identity)
	}
	return JSONResponse(http.StatusOK, map[string]any{"ok": true, "results": results})
}

// acquireQuotaRun 取得「额度查询轮次」的执行权。
//
// 返回本轮完成信号与是否为发起者。已有一轮在跑时返回它的完成信号，
// 调用方等同一轮结束即可，避免刷新页面时把同一批账号重复打向上游。
func (s *Service) acquireQuotaRun() (chan struct{}, bool) {
	s.quotaRunMu.Lock()
	defer s.quotaRunMu.Unlock()
	if s.quotaRun != nil {
		return s.quotaRun, false
	}
	s.quotaRun = make(chan struct{})
	return s.quotaRun, true
}

// finishQuotaRun 结束本轮：先关完成信号，再交出执行权。
func (s *Service) finishQuotaRun(done chan struct{}) {
	s.quotaRunMu.Lock()
	if s.quotaRun == done {
		s.quotaRun = nil
	}
	s.quotaRunMu.Unlock()
	close(done)
}

// runQuotaBatch 在后台作用域里查询全部账号额度，逐个写缓存。
//
// 每个账号完成即写缓存（成功与失败都写），因此页面在轮次中途刷新也能
// 看到已完成的账号，而不是整批退回「查询中」。
func (s *Service) runQuotaBatch(done chan struct{}, jobs []quotaJob) {
	defer s.finishQuotaRun(done)

	ctx, cancel := context.WithTimeout(hostbridge.WithCallbackID(context.Background(), ""), requestTimeout)
	defer cancel()

	outcomes := accountpkg.RunBatch(jobs, batchConcurrency, func(current quotaJob) (outcome quotaBatchOutcome) {
		outcome.result = current.identity()
		// **成功与失败都要缓存**：缓存是账号表唯一的数据源。只缓存成功结果会让
		// 失败账号在下次 /status 时退回「没有结果」，页面于是永远停在「查询中…」——
		// 而「上游没有额度接口」这类结论早已确定，不存在还会变。
		defer func() {
			result := outcome.result
			s.cacheQuotaResult(result)
		}()
		quota, errQuota := s.queryQuota(ctx, current)
		if errQuota != nil {
			outcome.result.Message = errQuota.Error()
			return outcome
		}
		result := &outcome.result
		if quota.Claim != nil {
			outcome.claim = quota.Claim
		}
		result.OK = true
		result.Summary = accountpkg.Summary(quota)
		totals := accountpkg.Totals(quota)
		result.Remain, result.Total, result.Unlimited = totals.Remaining, totals.Total, totals.Unlimited
		result.Metrics = quota.Summary
		return outcome
	})

	// 领取状态跟着额度一起刷新，但**批末才落盘**：Mutate 每次都会写一遍
	// state.json，逐账号写等于把一次「查询额度」放大成 N 次磁盘写。
	claims := map[string]state.ClaimRecord{}
	for _, outcome := range outcomes {
		if outcome.claim != nil {
			claims[outcome.result.UID] = state.ClaimRecord{
				ClaimStatus: *outcome.claim,
				CheckedAt:   time.Now().Format(time.RFC3339),
			}
		}
	}
	if len(claims) > 0 {
		s.deps.MutateState(func(current *state.State) {
			for uid, record := range claims {
				current.Claim[uid] = record
			}
		})
	}
}

// queryQuota 查询单个账号的额度（含一次过期凭证的保活重试）。
//
// 只负责取数；缓存、领取状态落盘由 runQuotaBatch 统一负责。
func (s *Service) queryQuota(ctx context.Context, job quotaJob) (*pluginapi.QuotaFetchResponse, error) {
	quotaProvider, okQuota := core.As[core.QuotaProvider](job.vendor)
	if !okQuota {
		return nil, core.QuotaUnsupported(job.vendor)
	}
	quota, errQuota := quotaProvider.Quota(ctx, job.credential)
	if errQuota != nil && isAuthExpired(errQuota) {
		// 凭证过期类错误：先保活刷新一次再重试。MiniMax 的 access token 只有
		// 1 小时有效期，不刷新的话额度列会一直挂着 401，用户只能自己想到去点
		// 「一键保活」——那条路还要求他先认出这是凭证问题。
		if refreshed := s.refreshedCredential(ctx, "", job); refreshed != nil {
			quota, errQuota = quotaProvider.Quota(ctx, refreshed)
		}
	}
	if errQuota != nil && isTransientFailure(errQuota) {
		// 传输层抖动（上游 EOF / 连接被重置）在实测里是成片出现的：一次批量
		// 查询里同一轮多个账号同时失败，几秒后重查就正常。重试一次的成本远低于
		// 让用户对着一列「查询失败」自己点刷新。
		if core.SleepContext(ctx, quotaRetryDelay) {
			quota, errQuota = quotaProvider.Quota(ctx, job.credential)
		}
	}
	return quota, errQuota
}

// quotaRetryDelay 是瞬时故障重试前的等待。
const quotaRetryDelay = 500 * time.Millisecond

// isTransientFailure 判断错误是否属于「稍后重试可能成功」的传输层故障。
func isTransientFailure(err error) bool {
	var failure *core.Failure
	if errors.As(err, &failure) {
		return failure.Kind == core.FailureKindTransient
	}
	var pluginErr *core.PluginError
	if errors.As(err, &pluginErr) {
		return pluginErr.HTTPStatus == http.StatusBadGateway ||
			pluginErr.HTTPStatus == http.StatusServiceUnavailable
	}
	return false
}

// isAuthExpired 判断错误是否属于「凭证已过期/失效」类（401）。
//
// 只认 401：403/404 往往意味着账号没这个权限或上游改版，刷新不会变好，
// 盲目刷新只会白白多打一次上游。
func isAuthExpired(err error) bool {
	var failure *core.Failure
	if errors.As(err, &failure) {
		return failure.Kind == core.FailureKindCredential || failure.HTTPStatus() == http.StatusUnauthorized
	}
	var pluginErr *core.PluginError
	if errors.As(err, &pluginErr) {
		return pluginErr.HTTPStatus == http.StatusUnauthorized
	}
	return false
}

// refreshedCredential 对单个账号跑一次保活，并返回刷新后的凭证。
//
// 复用保活那条既有通道（它负责刷新 + 写盘 + 热更新宿主），额度查询因此不需要
// 自己再造一套刷新逻辑。保活失败或账号消失时返回 nil，由调用方按原错误上报。
func (s *Service) refreshedCredential(ctx context.Context, callbackID string, target quotaJob) *core.Credential {
	authID := core.FirstNonEmpty(target.entry.ID, target.entry.Name)
	if authID == "" {
		return nil
	}
	s.tasks.Keepalive(ctx, callbackID, []string{authID}, 1)

	loaded, _ := s.pool.Load(ctx, callbackID, accountpkg.Options{
		Targets:       []string{authID},
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	for _, account := range loaded {
		if account.Vendor.ID() == target.vendor.ID() {
			return account.Credential
		}
	}
	return nil
}
