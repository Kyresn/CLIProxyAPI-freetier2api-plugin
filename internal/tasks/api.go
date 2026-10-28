package tasks

// 本文件是任务引擎对管理层的稳定 API：返回可直接 JSON 序列化的结果。

import (
	"context"
	"net/http"
	"time"

	"freetier2api-plugin/internal/core"
)

// QueueRequest 是「启动任务队列」的请求参数。
//
// 解析放在管理接口层（tasks 不得依赖 admin）：本包只接收已解析的参数，
// 不再自己解 JSON——那会让畸形请求静默退化成默认队列。
type QueueRequest struct {
	Concurrency int  `json:"concurrency"`
	Growth      bool `json:"growth"`
	School      bool `json:"school"`
}

// DefaultQueueRequest 是请求体缺省时的队列参数（旧控制台的默认行为）。
func DefaultQueueRequest() QueueRequest {
	return QueueRequest{Concurrency: 1, Growth: true, School: true}
}

// Scan 扫描全部账号的待办任务。
func (s *Service) Scan(ctx context.Context, callbackID string) map[string]any {
	scans := s.scanAllAccounts(ctx, callbackID)
	total := 0
	for _, scan := range scans {
		total += len(scan.Tasks)
	}
	return map[string]any{"ok": true, "accounts": scans, "pending_count": total}
}

// QueueSnapshot 返回任务队列快照。
func (s *Service) QueueSnapshot() map[string]any { return s.taskQueueSnapshot() }

// StartQueue 按已解析的参数启动任务队列。
func (s *Service) StartQueue(request QueueRequest, callbackID string) (map[string]any, int) {
	concurrency := request.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > queueMaxConcurrency {
		concurrency = queueMaxConcurrency
	}
	// growth/school 是旧控制台的筛选参数，只在这里映射成任务 kind；
	// 引擎本身按 TaskItem.Kind 驱动，不认识供应商专属任务族。
	selected := map[string]bool{}
	if request.Growth {
		selected["growth"] = true
	}
	if request.School {
		selected["school"] = true
	}
	if !request.Growth && !request.School {
		selected = nil
	}

	s.queue.mu.Lock()
	if s.queue.running {
		s.queue.mu.Unlock()
		return map[string]any{"ok": false, "error": "队列正在执行中"}, http.StatusConflict
	}
	s.queue.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	scans := s.scanAllAccounts(ctx, callbackID)
	cancel()

	items := s.buildQueueItems(scans, selected)
	if len(items) == 0 {
		return map[string]any{"ok": true, "started": false, "message": "全部账号没有待办任务"}, http.StatusOK
	}

	s.queue.mu.Lock()
	s.queue.running = true
	s.queue.seq++
	s.queue.conc = concurrency
	s.queue.startedAt = time.Now()
	s.queue.items = items
	seq := s.queue.seq
	s.queue.mu.Unlock()

	go s.runQueueItems(callbackID, concurrency, seq)
	return map[string]any{"ok": true, "started": true, "total": len(items), "seq": seq, "concurrency": concurrency}, http.StatusOK
}

// RunAuto 对单个账号执行单任务或一键完成。
func (s *Service) RunAuto(ctx context.Context, callbackID, identifier, code string) (map[string]any, int) {
	account := s.findAccount(ctx, callbackID, identifier)
	if account == nil {
		return map[string]any{"ok": false, "error": "账号不存在"}, http.StatusNotFound
	}
	if !s.tryLockAccount(account.uid()) {
		return map[string]any{"ok": false, "error": "该账号有任务动作正在执行中"}, http.StatusConflict
	}
	defer s.unlockAccount(account.uid())

	if code == "" {
		return map[string]any{"ok": true, "results": s.runAutoAll(ctx, account)}, http.StatusOK
	}
	result, errRun := s.runTaskForAccount(ctx, account, code)
	if errRun != nil {
		return map[string]any{"ok": false, "error": errRun.Error(), "task_code": code}, http.StatusOK
	}
	return map[string]any{
		"ok": true, "task_code": code, "message": result.Message,
		"credit": result.Credit, "energy": result.Energy,
	}, http.StatusOK
}

// RunAutoAll 对指定账号（targets 为空表示全部）依次执行一键完成。
func (s *Service) RunAutoAll(ctx context.Context, callbackID string, targets []string) map[string]any {
	loaded := s.collectAccounts(ctx, callbackID, targets)
	if len(loaded) == 0 {
		return map[string]any{"ok": true, "accounts": 0, "message": "没有可用账号"}
	}
	type accountResult struct {
		Label   string          `json:"label"`
		UID     string          `json:"uid"`
		Realm   string          `json:"realm"`
		Skipped string          `json:"skipped,omitempty"`
		Results []autoAllResult `json:"results,omitempty"`
	}
	out := make([]accountResult, 0, len(loaded))
	for _, account := range loaded {
		entry := accountResult{Label: account.label(), UID: account.uid(), Realm: account.vendor.Region()}
		if _, okRunner := core.As[core.TaskRunner](account.vendor); !okRunner {
			entry.Skipped = "该供应商没有可自动执行的任务"
			out = append(out, entry)
			continue
		}
		if !s.tryLockAccount(account.uid()) {
			entry.Skipped = "该账号有其它任务动作在执行"
			out = append(out, entry)
			continue
		}
		entry.Results = s.runAutoAll(ctx, account)
		s.unlockAccount(account.uid())
		out = append(out, entry)
		if !core.SleepContext(ctx, accountGap) {
			break
		}
	}
	done := 0
	for _, entry := range out {
		for _, result := range entry.Results {
			if result.Status == "done" {
				done++
			}
		}
	}
	return map[string]any{"ok": true, "accounts": len(out), "tasks_done": done, "results": out}
}

// ActivityStatus 返回各账号的活动任务状态（/school/status 的兼容投影）。
func (s *Service) ActivityStatus(ctx context.Context, callbackID string) map[string]any {
	return map[string]any{"ok": true, "accounts": s.activityStatusForAccounts(ctx, callbackID)}
}
