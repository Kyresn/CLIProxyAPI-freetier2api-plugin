package raccoon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"freetier2api-plugin/internal/hostbridge"
)

// useLoopbackHost 把宿主 HTTP 桥接到真实网络。
func useLoopbackHost(t *testing.T) {
	t.Helper()
	hostbridge.ConfigureLoopback()
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

// TestApplyGrantHeadersUsesDesktopEnum 锁住登录奖励接口的平台枚举与组织头。
func TestApplyGrantHeadersUsesDesktopEnum(t *testing.T) {
	req, errNew := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}
	ApplyGrantHeaders(req, "tok", "org-1", "dev-1")
	if got := req.Header.Get("X-Client-Platform"); got != "desktop-windows" {
		t.Errorf("X-Client-Platform = %q, want desktop-windows", got)
	}
	if got := req.Header.Get("X-Client-Version"); got != "v1.0.35" {
		t.Errorf("X-Client-Version = %q, want v1.0.35", got)
	}
	if got := req.Header.Get("X-Org-Code"); got != "org-1" {
		t.Errorf("X-Org-Code = %q, want org-1", got)
	}
	if got := req.Header.Get("X-Raccoon-Language"); got != "zh" {
		t.Errorf("X-Raccoon-Language = %q, want zh", got)
	}
	if got := req.Header.Get("X-Client-Device-ID"); got != "dev-1" {
		t.Errorf("X-Client-Device-ID = %q, want dev-1", got)
	}
}

// TestClaimDailyGrantParsesPopupPoints 锁住领取金额来自 data.popup.points。
func TestClaimDailyGrantParsesPopupPoints(t *testing.T) {
	useLoopbackHost(t)
	var gotBody []byte
	var gotPlatform string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotPlatform = r.Header.Get("X-Client-Platform")
		_, _ = w.Write([]byte(`{"code":0,"data":{"granted":true,"popup":{"points":3000}}}`))
	}))
	defer server.Close()

	cred := &Credential{AccessToken: "tok", OfficeIdentity: "org-1"}
	granted, credit, errClaim := ClaimDailyGrant(context.Background(), server.URL, cred)
	if errClaim != nil {
		t.Fatalf("ClaimDailyGrant: %v", errClaim)
	}
	if !granted || credit != 3000 {
		t.Fatalf("granted=%v credit=%d, want true/3000", granted, credit)
	}
	if len(gotBody) != 0 {
		t.Fatalf("领取请求体应为空，得到 %q", string(gotBody))
	}
	if gotPlatform != "desktop-windows" {
		t.Fatalf("X-Client-Platform = %q", gotPlatform)
	}
}

// TestClaimDailyGrantAlreadyClaimed 锁住 granted:false 的幂等语义。
func TestClaimDailyGrantAlreadyClaimed(t *testing.T) {
	useLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"granted":false}}`))
	}))
	defer server.Close()

	granted, credit, errClaim := ClaimDailyGrant(context.Background(), server.URL, &Credential{AccessToken: "tok"})
	if errClaim != ErrCheckinNotGranted {
		t.Fatalf("err = %v, want ErrCheckinNotGranted", errClaim)
	}
	if granted || credit != 0 {
		t.Fatalf("granted=%v credit=%d, want false/0", granted, credit)
	}
}
