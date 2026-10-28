package admin

// 本文件是任务引擎的管理接口适配层：解析请求、调用 tasks.Service、封装响应。

import (
	"net/http"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/tasks"
)

func (s *Service) handleTasksScan(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	return JSONResponse(http.StatusOK, s.tasks.Scan(ctx, hostbridge.RequestCallbackID(req)))
}

func (s *Service) handleTasksRun(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	// 缺省值与旧行为一致：growth + school 全开、并发 1；请求体只覆盖它给出的字段。
	body, errDecode := decodeRequestInto(req.Body, tasks.DefaultQueueRequest())
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	payload, status := s.tasks.StartQueue(body, hostbridge.RequestCallbackID(req))
	return JSONResponse(status, payload)
}

func (s *Service) handleTasksQueue(_ pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return JSONResponse(http.StatusOK, s.tasks.QueueSnapshot())
}

// taskAutoRequest 是「对单个账号执行单个任务」的请求体。
type taskAutoRequest struct {
	AuthID    string `json:"auth_id"`
	UID       string `json:"uid"`
	AccountID string `json:"account_id"`
	TaskCode  string `json:"task_code"`
}

func (s *Service) handleTasksAuto(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[taskAutoRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	identifier := core.FirstNonEmpty(body.AuthID, body.UID, body.AccountID)
	result, status := s.tasks.RunAuto(ctx, hostbridge.RequestCallbackID(req), identifier, body.TaskCode)
	return JSONResponse(status, result)
}

func (s *Service) handleTasksAutoAll(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	// 空请求体 = 全部账号；勾选了账号时页面传 account_ids 限定范围。
	body, errDecode := decodeRequest[accountBatchRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	return JSONResponse(http.StatusOK, s.tasks.RunAutoAll(ctx, hostbridge.RequestCallbackID(req), body.AccountIDs))
}

func (s *Service) handleSchoolStatus(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	return JSONResponse(http.StatusOK, s.tasks.ActivityStatus(ctx, hostbridge.RequestCallbackID(req)))
}
