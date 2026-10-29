package tasks

// 并发续期的去重语义。
//
// 排程保活、控制台「Token 续期」与额度查询遇 401 的重试共用 RefreshAccount。
// 三者在续期前各自加载账号，手里是同一份旧凭证；MiniMax 这类刷新会轮换
// refresh_token 的上游，会让后到的那次拿到 invalid_grant——正常账号被误判成
// 「需要重新授权」。这里锁定「同一份快照只刷一次、写回一次」。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// rotatingVendor 模拟刷新时轮换 token 的上游，并统计刷新次数。
type rotatingVendor struct {
	id    string
	calls atomic.Int32
}

func (v *rotatingVendor) ID() string                                     { return v.id }
func (v *rotatingVendor) Name() string                                   { return v.id }
func (v *rotatingVendor) Region() string                                 { return "cn" }
func (v *rotatingVendor) ModelPrefix() string                            { return "" }
func (v *rotatingVendor) Match(string, string, map[string]any) bool      { return false }
func (v *rotatingVendor) Parse([]byte, string) (*core.Credential, error) { return nil, nil }
func (v *rotatingVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *rotatingVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *rotatingVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}

func (v *rotatingVendor) Refresh(_ context.Context, cred *core.Credential) (*core.Credential, bool, error) {
	// 放慢一点，让并发调用确实停在锁上。
	time.Sleep(20 * time.Millisecond)
	round := v.calls.Add(1)
	cred.Token = fmt.Sprintf("token-%d", round)
	cred.RefreshToken = fmt.Sprintf("refresh-%d", round)
	return cred, true, nil
}

// TestRefreshAccountDeduplicatesConcurrentRefresh 验证并发续期只打一次上游、
// 只写回一次，且后续用新凭证的续期照常进行（去重不会误伤真正的下一次刷新）。
func TestRefreshAccountDeduplicatesConcurrentRefresh(t *testing.T) {
	vendor := &rotatingVendor{id: "zcodetest"}
	deps := newFakeDeps(time.Now())
	service := &Service{deps: deps}

	// 每个并发入口都有自己的凭证快照，与生产一致（各自从账号池加载）。
	newAccount := func(token string) *accountContext {
		return &accountContext{
			entry:      core.HostAuth{ID: "zcodetest-1"},
			vendor:     vendor,
			credential: &core.Credential{FileID: "zcodetest-1", Token: token, RefreshToken: "refresh-0"},
		}
	}

	const callers = 4
	var wait sync.WaitGroup
	results := make([]bool, callers)
	errors := make([]error, callers)
	for i := 0; i < callers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errors[index] = service.RefreshAccount(context.Background(), "", newAccount("token-0"))
		}(i)
	}
	wait.Wait()

	if got := vendor.calls.Load(); got != 1 {
		t.Fatalf("upstream refresh calls = %d, want 1 (同一份快照只应刷一次)", got)
	}
	if deps.refreshSaves != 1 {
		t.Fatalf("credential write-backs = %d, want 1", deps.refreshSaves)
	}
	for index := range results {
		if errors[index] != nil {
			t.Fatalf("caller %d failed: %v", index, errors[index])
		}
		if !results[index] {
			t.Fatalf("caller %d must report refreshed (凭证已由并发续期更新)", index)
		}
	}

	// 拿到新 token 后的下一次续期是真实轮次，不能被去重拦掉。
	if refreshed, err := service.RefreshAccount(context.Background(), "", newAccount("token-1")); err != nil || !refreshed {
		t.Fatalf("follow-up refresh = %v, %v; want true, nil", refreshed, err)
	}
	if got := vendor.calls.Load(); got != 2 {
		t.Fatalf("upstream refresh calls = %d, want 2", got)
	}
}
