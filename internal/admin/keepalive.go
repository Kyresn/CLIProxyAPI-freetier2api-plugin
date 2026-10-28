package admin

// 本文件是 token 保活的管理端点适配：请求解码 + 结果汇总。
//
// 续期与写回的实现在 internal/tasks（keepalive.go），与排程里的 keepalive
// 任务共用同一份逻辑；本文件只负责 HTTP 语义（结果汇总与响应形状）。
//
// 响应形状保持不变：{ok,total,succeeded,failed,message,results[]}。

import (
	"fmt"
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/hostbridge"
)

func (s *Service) handleKeepaliveRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	// 空请求体 / 空 account_ids = 全部账号；勾选了账号时页面只传选中的那些。
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()

	results := s.tasks.Keepalive(ctx, hostbridge.RequestCallbackID(req), body.AccountIDs, batchConcurrency)
	if len(results) == 0 {
		return JSONResponse(http.StatusOK, map[string]any{
			"ok": true, "message": "没有需要续期的可用账号", "succeeded": 0, "failed": 0, "results": []any{},
		})
	}

	succeeded, failed := 0, 0
	firstErrMsg := ""
	for _, result := range results {
		if result.OK {
			succeeded++
			continue
		}
		failed++
		if firstErrMsg == "" {
			firstErrMsg = result.Label + ": " + result.Message
		}
	}
	message := ""
	switch {
	case failed > 0 && succeeded == 0:
		message = fmt.Sprintf("全部账号续期失败（%s）", firstErrMsg)
	case failed > 0:
		message = fmt.Sprintf("续期完成：%d 个成功，%d 个失败（%s）", succeeded, failed, firstErrMsg)
	default:
		message = fmt.Sprintf("全账号 Token 续期成功（共 %d 个账号已更新）", succeeded)
	}
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": failed == 0, "total": len(results), "succeeded": succeeded, "failed": failed,
		"message": message, "results": results,
	})
}
