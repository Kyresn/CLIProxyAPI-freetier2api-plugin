package tasks

// 本文件定义任务引擎需要的最小配置面。
//
// 刻意不复用管理层的 PluginConfig：任务引擎必须与传输层解耦（仓库的分层
// 守卫禁止 tasks → admin）。这里只声明引擎真正读到的四个字段，由组合根把
// 全局配置投影进来——引擎因此可以在没有管理接口的宿主里单测。

// Config 是任务引擎的运行期配置。
type Config struct {
	// EnabledRealms 是启用的域（cn / global）。
	EnabledRealms []string
	// AutoCheckin 报告是否每日自动签到。
	AutoCheckin bool
	// AutoCheckinAt 是自动签到时间（HH:MM）。
	AutoCheckinAt string
	// AutoTasks 报告是否每日自动跑任务闭环。
	AutoTasks bool
	// CheckinHistoryLimit 是每个账号保留的签到历史条数（<1 时用默认值）。
	CheckinHistoryLimit int
}
