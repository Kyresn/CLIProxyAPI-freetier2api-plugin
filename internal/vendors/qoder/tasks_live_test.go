//go:build live

package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/hostbridge"
)

// Qoder 签到的实况探针（-tags live）。
//
// 运行方式：
//
//	QODER_LIVE=1 QODER_LIVE_CREDS=<auth.json 路径> ;
//	  go test -tags live ./internal/vendors/qoder/ -run TestLiveCheckin -v
//
// 默认只做**只读**探测（列活动 + 查额度），不发起领取：领取是真实发放
// 积分的外部副作用，误触发会消耗当天的领取窗口。
//
// 确认要测领取链路时再加 QODER_LIVE_CLAIM=1，且务必在上游窗口开启
// （每日 10:00 UTC+8）之后运行。
//
// 只打印截断后的非敏感信息（轮次、状态、额度），不打印凭证内容。

// resolveLiveIdentity 取探针账号自己提供的设备身份。
//
// 探针必须与插件走同一条身份来源，否则测的就不是线上行为：身份只来自凭证
// 或 QODER_LIVE_MACHINE_TOKEN/TYPE（模拟全局配置），插件不派生。
func resolveLiveIdentity(cred *Credential) MachineIdentity {
	return ResolveIdentity(cred, map[string]string{
		"qoder_machine_token": os.Getenv("QODER_LIVE_MACHINE_TOKEN"),
		"qoder_machine_type":  os.Getenv("QODER_LIVE_MACHINE_TYPE"),
		"qoder_machine_code":  os.Getenv("QODER_LIVE_MACHINE_CODE"),
	}, MachineIdentity{})
}

// configureQoderProbeHost 装上把出站请求真发到上游的宿主。
func configureQoderProbeHost(t *testing.T) {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("probe host: unsupported method %s", method)
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		rawPayload, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		if errDecode := json.Unmarshal(rawPayload, &req); errDecode != nil {
			return nil, errDecode
		}
		var bodyReader io.Reader
		if len(req.Body) > 0 {
			bodyReader = strings.NewReader(string(req.Body))
		}
		httpReq, errNew := http.NewRequest(req.Method, req.URL, bodyReader)
		if errNew != nil {
			return nil, errNew
		}
		for k, vs := range req.Headers {
			for _, v := range vs {
				httpReq.Header.Add(k, v)
			}
		}
		client := &http.Client{Timeout: 60 * time.Second}
		resp, errDo := client.Do(httpReq)
		if errDo != nil {
			return nil, errDo
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return nil, errRead
		}
		return json.Marshal(map[string]any{"StatusCode": resp.StatusCode, "Headers": resp.Header, "Body": respBody})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

// TestLiveCheckin 对真实上游跑一次签到探测。
func TestLiveCheckin(t *testing.T) {
	if os.Getenv("QODER_LIVE") == "" {
		t.Skip("QODER_LIVE 未设置，跳过实况探针")
	}
	path := os.Getenv("QODER_LIVE_CREDS")
	if path == "" {
		t.Skip("QODER_LIVE_CREDS 未设置")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}
	region := RegionCN
	if os.Getenv("QODER_LIVE_REGION") == "global" {
		region = RegionGlobal
	}
	cred, errParse := ParseCredential(raw, region)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	configureQoderProbeHost(t)

	// QODER_LIVE_AT 可指定判定时刻（RFC3339），用来在真实上游上复现
	// 「窗口未开启」等时刻相关分支——那是签到正确性的关键分界。
	if rawAt := strings.TrimSpace(os.Getenv("QODER_LIVE_AT")); rawAt != "" {
		parsed, errParse := time.Parse(time.RFC3339, rawAt)
		if errParse != nil {
			t.Fatalf("解析 QODER_LIVE_AT 失败: %v", errParse)
		}
		restore := checkinNow
		checkinNow = func() time.Time { return parsed.In(campaignZone) }
		t.Cleanup(func() { checkinNow = restore })
		t.Logf("判定时刻覆盖为: %s", parsed.In(campaignZone).Format("2006-01-02 15:04:05 MST"))
	}

	// 先记录领取前的额度基线，用于判断领取是否真的到账。
	quotaBefore, errQuota := readAddonQuotaTotal(context.Background(), region, checkinBearer(cred.Token))
	if errQuota != nil {
		t.Logf("读取额度基线失败（将只用 reward 回读判定）: %v", errQuota)
	}

	identity := resolveLiveIdentity(cred)
	t.Logf("设备身份: native=%v", identity.Native())

	result, errCheckin := Checkin(context.Background(), cred, identity)
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	t.Logf("签到结果: status=%s amount=%d window=%s notRecorded=%v", result.Status, result.Amount, result.WindowDate, result.NotRecorded)
	t.Logf("说明: %s", result.Message)

	quotaAfter, errQuotaAfter := readAddonQuotaTotal(context.Background(), region, checkinBearer(cred.Token))
	if errQuotaAfter == nil {
		t.Logf("拓展包额度: %v → %v", quotaBefore, quotaAfter)
	}

	if os.Getenv("QODER_LIVE_CLAIM") == "" {
		t.Log("未设置 QODER_LIVE_CLAIM，仅做只读探测")
		return
	}
	if result.Status != CheckinClaimed {
		t.Logf("状态不是 %s，跳过领取回读", CheckinClaimed)
		return
	}
	granted, amount := readCampaignReward(context.Background(), region, checkinBearer(cred.Token), identity, result.CampaignID)
	t.Logf("奖励回读: granted=%v amount=%d", granted, amount)
}
