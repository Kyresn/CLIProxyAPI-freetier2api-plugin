package tasks

// 本文件是「凭证续期 + 写回」的唯一编排。
//
// 此前这件事有两份实现：管理端点的批量保活，以及 workbuddy 在
// RunScheduled(keepalive) 里自己发宿主 RPC + 写磁盘。后者让供应商包依赖了
// internal/hostbridge，破坏了「vendor 只依赖 core/hostbridge/logger」的边界，
// 而且两份实现的「合并 → host.auth.save → 落盘」逻辑会各自漂移。
//
// 现在供应商只负责一件事：实现 core.Refresher 刷新 token；写回由本文件统一
// 完成，供应商因此不必认识宿主。

import (
	"context"
	"sync"

	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// KeepaliveResult 是一个账号的续期结果（管理端点 JSON 契约的一部分）。
type KeepaliveResult struct {
	AuthID   string `json:"auth_id"`
	Label    string `json:"label"`
	VendorID string `json:"vendor_id,omitempty"`
	Realm    string `json:"realm"`
	OK       bool   `json:"ok"`
	Message  string `json:"message"`
}

// RefreshAccount 刷新单个账号的凭证，凭证发生变更时写回宿主与磁盘。
//
// 写回走 deps.SaveRefreshedCredential（由组合根实现），因此本包不需要知道
// 宿主 RPC 的存在。callbackID 为空表示后台任务（无请求作用域）。
// 返回 refreshed 表示凭证确实变了。
//
// 同一账号的续期按 refreshGate 串行化：排程保活、控制台「Token 续期」与
// 额度查询遇 401 的重试可能同时命中同一个账号，而它们手里是续期前加载的
// 同一份旧凭证。
func (s *Service) RefreshAccount(ctx context.Context, callbackID string, account *accountContext) (refreshed bool, err error) {
	authID := core.FirstNonEmpty(account.entry.ID, account.entry.Name, account.uid())
	before := account.credential.TokenValue()
	reuse, unlock := s.refreshGate.begin(authID, before)
	defer unlock()
	if reuse {
		// 这份凭证已被并发的续期刷新并写回；再刷一次只会拿旧 refresh_token
		// 换回 invalid_grant，把一个正常账号变成「需要重新授权」。
		return true, nil
	}
	updated, changed, errRefresh := core.RefreshVendor(account.vendor, ctx, account.credential)
	if errRefresh != nil {
		return false, errRefresh
	}
	if !changed {
		return false, nil
	}
	s.refreshGate.finish(authID, before, updated.TokenValue())
	s.deps.SaveRefreshedCredential(ctx, callbackID, account.vendor,
		account.entry, account.rawJSON, updated, account.filePath)
	return true, nil
}

// refreshGate 按账号串行化续期，并复用「同一份凭证已经刷过」的结果。
//
// 只靠加锁不够：后到的调用手里仍是续期前的快照，解锁后再刷一次等于用旧
// refresh_token 打上游。因此再记一份「本次刷新的前后 access token」指纹，
// 输入 token 与上次刷新的输入相同时直接复用那次结果。
//
// 条目数只与账号数同阶（每账号一条），不随调用次数增长。
type refreshGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	done  map[string]refreshDone
}

// refreshDone 是一次成功刷新的凭证指纹。
type refreshDone struct {
	before string // 刷新前的 access token
	after  string // 刷新后的 access token
}

// begin 取得该账号的续期权；reuse 为 true 表示手里的凭证已被并发的续期
// 刷过，调用方应直接复用那次结果，不要再打上游。
func (g *refreshGate) begin(authID, accessToken string) (reuse bool, unlock func()) {
	lock := g.lockFor(authID)
	lock.Lock()

	g.mu.Lock()
	record, okRecord := g.done[authID]
	g.mu.Unlock()
	// before == after 说明上游并没换 token，指纹不可用于判重。
	reuse = okRecord && accessToken != "" && record.before != record.after && accessToken == record.before
	return reuse, lock.Unlock
}

// finish 记录一次成功刷新的指纹。
func (g *refreshGate) finish(authID, before, after string) {
	g.mu.Lock()
	if g.done == nil {
		g.done = map[string]refreshDone{}
	}
	g.done[authID] = refreshDone{before: before, after: after}
	g.mu.Unlock()
}

// lockFor 取（必要时创建）某账号的续期锁。
func (g *refreshGate) lockFor(authID string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.locks == nil {
		g.locks = map[string]*sync.Mutex{}
	}
	lock, okLock := g.locks[authID]
	if !okLock {
		lock = &sync.Mutex{}
		g.locks[authID] = lock
	}
	return lock
}

// Keepalive 对全部（或指定的）账号执行 token 续期，供管理端点调用。
//
// 逐账号的失败不影响其它账号：结果里逐条给出原因，由调用方汇总。
func (s *Service) Keepalive(ctx context.Context, callbackID string, targets []string, concurrency int) []KeepaliveResult {
	loaded, _ := s.deps.Pool().Load(ctx, callbackID, auth.Options{
		Targets:       targets,
		EnabledRealms: s.deps.Config().EnabledRealms,
	})
	jobs := make([]*accountContext, 0, len(loaded))
	for _, item := range loaded {
		jobs = append(jobs, newAccountContext(item))
	}
	return auth.RunBatch(jobs, concurrency, func(job *accountContext) KeepaliveResult {
		return s.keepaliveOne(ctx, callbackID, job)
	})
}

// keepaliveOne 是单账号的续期与结果归一。
func (s *Service) keepaliveOne(ctx context.Context, callbackID string, account *accountContext) KeepaliveResult {
	label := account.label()
	result := KeepaliveResult{
		AuthID:   core.FirstNonEmpty(account.entry.ID, account.entry.Name),
		Label:    label,
		VendorID: account.vendor.ID(),
		Realm:    account.credential.RegionValue(),
	}
	refreshed, errRefresh := s.RefreshAccount(ctx, callbackID, account)
	if errRefresh != nil {
		result.Message = errRefresh.Error()
		logger.Error("keepalive %s: %v", label, errRefresh)
		return result
	}
	result.OK = true
	if refreshed {
		result.Message = "Token 续期成功"
	} else {
		result.Message = "凭证有效（未变更）"
	}
	logger.Info("keepalive %s: refreshed=%t", label, refreshed)
	return result
}
