package tasks

// 本文件是任务引擎的实例入口：调度循环、队列状态与账号锁。

import (
	"context"
	"sync"
)

// Service 是一个 App 实例的任务引擎。
type Service struct {
	deps    Deps
	catalog *Catalog

	schedulerMu     sync.Mutex
	schedulerCancel context.CancelFunc
	schedulerWG     sync.WaitGroup
	running         bool
	pokeCh          chan string

	taskLockMu sync.Mutex
	taskLocks  map[string]*sync.Mutex
	queue      *queueState

	// refreshGate 按账号串行化凭证续期（见 keepalive.go）。
	refreshGate refreshGate
}

// New 构造任务引擎。
func New(deps Deps) *Service {
	return &Service{
		deps:      deps,
		catalog:   NewCatalog(deps.Registry()),
		pokeCh:    make(chan string, 4),
		taskLocks: map[string]*sync.Mutex{},
		queue:     &queueState{},
	}
}

// Catalog 返回任务目录。
func (s *Service) Catalog() *Catalog { return s.catalog }

// Start 启动后台调度循环（幂等）。
func (s *Service) Start(ctx context.Context) {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.schedulerCancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.schedulerCancel = cancel
	s.schedulerWG.Add(1)
	go func() {
		defer s.schedulerWG.Done()
		s.runTaskScheduler(runCtx)
	}()
}

// Stop 停止后台调度循环并等待退出。
func (s *Service) Stop() {
	s.schedulerMu.Lock()
	cancel := s.schedulerCancel
	s.schedulerCancel = nil
	s.schedulerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.schedulerWG.Wait()
}

// Poke 唤醒调度循环重新计算下次执行时刻。
func (s *Service) Poke() {
	select {
	case s.pokeCh <- schedulerReconfigure:
	default:
	}
}

// Status 返回调度器状态（供管理页展示）。
func (s *Service) Status() map[string]any { return s.schedulerStatus() }

// EffectiveSettings 返回页面设置与配置合并后的可读视图。
func (s *Service) EffectiveSettings() map[string]any { return s.effectiveSettings() }

// RunAsync 在后台执行一个任务族。
func (s *Service) RunAsync(kind string) {
	go s.runScheduledTaskContext(context.Background(), kind)
}
