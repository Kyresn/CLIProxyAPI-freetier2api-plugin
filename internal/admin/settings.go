package admin

import (
	"net/http"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
	"freetier2api-plugin/internal/tasks"
)

type settingsUpdateRequest struct {
	AutoCheckin   *bool            `json:"auto_checkin"`
	AutoCheckinAt string           `json:"auto_checkin_at"`
	AutoTasks     *bool            `json:"auto_tasks"`
	PromptMode    *string          `json:"prompt_mode"`
	ModelNameTag  *string          `json:"model_name_tag"`
	TaskSchedules map[string][]int `json:"task_schedules"`

	// Deprecated: 兼容旧版页面/脚本的固定字段。
	CheckinHours   []int `json:"checkin_hours"`
	TravelHours    []int `json:"travel_hours"`
	ActivityHours  []int `json:"activity_hours"`
	KeepaliveHours []int `json:"keepalive_hours"`
	BlackcatHours  []int `json:"blackcat_hours"`
}

func (s *Service) handleSettingsRequest(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[settingsUpdateRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	promptMode, errPrompt := normalizePromptMode(body.PromptMode)
	if errPrompt != nil {
		return ErrorResponse(errPrompt)
	}
	modelNameTag, errTag := normalizeModelNameTagPointer(body.ModelNameTag)
	if errTag != nil {
		return ErrorResponse(errTag)
	}
	changed := map[string]any{}
	s.deps.MutateState(func(current *state.State) {
		if body.AutoCheckin != nil {
			current.AutoCheckin = body.AutoCheckin
			changed["auto_checkin"] = *body.AutoCheckin
		}
		if at := strings.TrimSpace(body.AutoCheckinAt); at != "" {
			if _, _, errClock := ParseClock(at); errClock == nil {
				current.AutoCheckinAt = at
				changed["auto_checkin_at"] = at
			}
		}
		if body.AutoTasks != nil {
			current.AutoTasks = body.AutoTasks
			changed["auto_tasks"] = *body.AutoTasks
		}
		if body.PromptMode != nil {
			// 空串表示撤销页面覆盖，重新由 YAML 配置决定。
			current.PromptMode = promptMode
			changed["prompt_mode"] = promptMode
		}
		if body.ModelNameTag != nil {
			// 渠道标识改变对外模型 ID：宿主模型注册表未重建前 /v1/models
			// 仍是旧名字。置 RestartPending 与手动别名/禁用同语义——控制台
			// 据此提示「待生效」并触发 syncModelConfig 自愈。比较**生效值**：
			// 页面覆盖与 YAML 相同时不重建注册表。
			before := EffectiveModelNameTag(s.deps.Config(), current.ModelNameTag)
			current.ModelNameTag = modelNameTag
			if EffectiveModelNameTag(s.deps.Config(), modelNameTag) != before {
				current.RestartPending = true
			}
			changed[ModelNameTagKey] = modelNameTag
		}
		if current.TaskSchedules == nil {
			current.TaskSchedules = map[string][]int{}
		}
		for kind, hours := range body.TaskSchedules {
			kind = strings.TrimSpace(kind)
			if kind == "" {
				continue
			}
			if _, known := s.tasks.Catalog().SpecByKind(kind); !known {
				continue
			}
			current.TaskSchedules[kind] = tasks.NormalizeHours(hours)
			changed["task_schedules"] = current.TaskSchedules
		}
		for kind, hours := range map[string][]int{
			"checkin":   body.CheckinHours,
			"travel":    body.TravelHours,
			"activity":  body.ActivityHours,
			"keepalive": body.KeepaliveHours,
			"blackcat":  body.BlackcatHours,
		} {
			if hours != nil {
				current.TaskSchedules[kind] = tasks.NormalizeHours(hours)
				changed["task_schedules"] = current.TaskSchedules
			}
		}
	})
	if errReconfigure := s.deps.Reconfigure(); errReconfigure != nil {
		return ErrorResponse(errReconfigure)
	}
	s.tasks.Poke()
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": changed, "settings": s.settingsView(),
	})
}

// normalizePromptMode 校验页面提交的提示词过滤模式。
//
// 提示词过滤是全局能力，只有一份模式：空串表示撤销页面覆盖、回到 YAML
// 配置，其余写法必须落在 ParsePromptMode 认可的取值内。
func normalizePromptMode(input *string) (string, error) {
	if input == nil {
		return "", nil
	}
	mode, errMode := ParsePromptMode(*input)
	if errMode != nil {
		return "", core.NewPluginError("prompt_mode_invalid", errMode.Error(), http.StatusBadRequest)
	}
	return mode, nil
}

// normalizeModelNameTagPointer 归一化页面提交的渠道标识模式。
// 空串表示撤销页面覆盖，重新由 YAML 配置决定；其余必须落在合法取值内。
func normalizeModelNameTagPointer(input *string) (string, error) {
	if input == nil {
		return "", nil
	}
	trimmed := strings.ToLower(strings.TrimSpace(*input))
	if trimmed == "" {
		// 空串表示撤销页面覆盖，重新由 YAML 配置决定。
		return "", nil
	}
	tag, errTag := NormalizeModelNameTag(trimmed)
	if errTag != nil {
		// 与 prompt_mode 同形：非法输入是 400，不能落成 500。
		return "", core.NewPluginError("model_name_tag_invalid", errTag.Error(), http.StatusBadRequest)
	}
	return tag, nil
}

// settingsView 是 /status 的 settings 载荷：任务引擎的可读设置 + 提示词过滤现状。
func (s *Service) settingsView() map[string]any {
	settings := s.tasks.EffectiveSettings()
	settings[PromptModeKey] = EffectivePromptMode(s.deps.Config(), s.deps.State().PromptMode)
	settings[ModelNameTagKey] = EffectiveModelNameTag(s.deps.Config(), s.deps.State().ModelNameTag)
	return settings
}

// handleScheduledRun 手动触发一个定时任务。
func (s *Service) handleScheduledRun(_ pluginapi.ManagementRequest, kind string) pluginapi.ManagementResponse {
	s.tasks.RunAsync(kind)
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "started": true, "task": kind,
		"message": "任务已在后台开始执行，进度见插件日志",
	})
}
