package admin

// 本文件锁定控制台页面的关键前端不变量（密钥解码、请求闸门、单页布局、折叠区）。
// 页面与断言同包，改动页面时改造这里，不要再散落到插件根层测试。

import (
	"regexp"
	"strings"
	"testing"
)

// unwrapRe 匹配「再剥一层信封」的写法：.result 后面不接 s（区分合法的 .results）。
var unwrapRe = regexp.MustCompile(`\.result\s*[;,)\n]`)

// TestConsolePageDecodesPanelKey 验证控制台页能从 CPA 管理面板的持久化快照里解出管理密钥。
//
// 本页与面板同源，因此可以复用面板已保存的密钥，避免用户重复输入。面板
// （CPAMC）把整份认证状态存在 localStorage 的 "cli-proxy-auth" 键下，值是可逆
// 的 XOR 混淆：
//
//	前缀 "enc::v1::" + base64( utf8(JSON) XOR key )
//	key = utf8("cli-proxy-api-webui::secure-storage|<host>|<userAgent>")
//	JSON = {"state":{...,"managementKey":"<密钥>",...},"version":N}
//
// 密钥由公开值派生，所以同源页面能解出来。这里断言页面确实实现了这套算法
// （算法细节若与面板不一致，用户会看到 401 且原因极难定位）。
func TestConsolePageDecodesPanelKey(t *testing.T) {
	page := Page("/plugins/freetier2api")

	// 混淆格式与快照字段必须与面板一致。
	for _, literal := range []string{
		`"enc::v1::"`,                            // 前缀
		`"cli-proxy-api-webui::secure-storage"`,  // 种子
		`var PANEL_STORE_KEY = "cli-proxy-auth"`, // 面板的存储键名
		`var PANEL_KEY_FIELD = "managementKey"`,  // 密钥在快照里的字段名
		`localStorage.getItem(PANEL_STORE_KEY)`,  // 只读面板快照
		"snapshot.state",                         // zustand persist 的 {state,version}
		"location.host",                          // 密钥派生输入
		"navigator.userAgent",                    // 密钥派生输入
		"atob(",                                  // base64 解码
		"TextDecoder",                            // utf8 解码
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing %s (key decoding would break)", literal)
		}
	}

	// 旧版面板的键名不再读取：读到过期值会让页面带着错密钥去打探针，
	// 白白消耗 CPA 的失败次数配额。
	for _, legacy := range []string{
		`getItem("managementKey")`,
		"detectKey(",
		`localStorage.getItem("cpa-management-key")`,
	} {
		if strings.Contains(page, legacy) {
			t.Fatalf("console page must not read the legacy key source %s", legacy)
		}
	}

	// 密钥必须校验字符集：面板的混淆密钥由 userAgent 派生，浏览器升级后会
	// 解出乱码；把乱码放进 Authorization 头会被浏览器拒绝（Safari 报
	// "The string did not match the expected pattern."），且极难定位。
	if !strings.Contains(page, "KEY_RE") {
		t.Fatal("console page must validate the key charset before using it in a header")
	}

	// 自动复用失败时必须能退回手填，否则用户会面对一片空白。
	for _, literal := range []string{`id="keyBox"`, `id="keyInput"`, `id="btnUseKey"`, "revealKeyInput"} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the manual key fallback (%s)", literal)
		}
	}
}

// TestConsolePageNeverCallsWithoutKey 验证控制台页在身份确认之前不发任何管理请求。
//
// CPA 按 IP 统计管理密钥失败次数（**未携带密钥也算**），5 次即把整个
// /v0/management 封禁 30 分钟。早期版本在拿不到密钥时仍并发发 5 个首屏请求，
// 因此"打开插件页"会直接把后台锁掉。本测试把修复后的三条不变量锁住：
//
//  1. 没有密钥 → 一个请求都不发；
//  2. 有密钥 → 只发一个探针，成功才加载首屏，且首屏串行；
//  3. 任一 401/403 → 立刻停掉全部自动刷新与轮询定时器。
func TestConsolePageNeverCallsWithoutKey(t *testing.T) {
	page := Page("/plugins/freetier2api")

	// 1) api() 闸门：未认证/已失效不出网，没有密钥也不出网。
	for _, literal := range []string{
		"if (stopped) return Promise.resolve(null);",
		"if (!key) return Promise.resolve(null);",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the api() guard %q", literal)
		}
	}

	// 2) 单探针先于首屏；首屏是串行链而不是并发铺开。
	if !regexp.MustCompile(`probeManagementAuth\(\)[\s\S]{0,800}startConsole\(\)`).MatchString(page) {
		t.Fatal("console page must validate the key with a single probe before loading data")
	}
	if !strings.Contains(page, `loadVendors().then(function () {`) {
		t.Fatal("console page must load the first screen serially (vendors first)")
	}

	// 定时器是失败放大器：只能在密钥验证通过（authReady）之后启动。
	if !strings.Contains(page, "if (authReady && box && box.checked)") {
		t.Fatal("log polling must require a verified key (authReady)")
	}
	if strings.Contains(page, "if (event.target.checked && !stopped)") {
		t.Fatal("log polling must not start from the raw stopped flag")
	}

	// 3) 鉴权失败必须一次就停掉全部定时器，不能只停日志。
	for _, literal := range []string{
		"function haltOnAuthFailure(status) {",
		"stopLoginPolling();",
		"if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the auth-failure stop %q", literal)
		}
	}

	// 手输也必须先探针验证再记忆，不能先写 sessionStorage 再试；
	// 验证通过后要把 authReady 置真，否则日志轮询永远不会启动。
	if !regexp.MustCompile(`probeManagementAuth\(\)[\s\S]{0,900}setItem\(MANUAL_STORE_KEY`).MatchString(page) {
		t.Fatal("manually entered keys must be probe-verified before being remembered")
	}
	if !regexp.MustCompile(`setItem\(MANUAL_STORE_KEY, value\)[\s\S]{0,200}authReady = true`).MatchString(page) {
		t.Fatal("manual login must mark authReady after the probe succeeds")
	}
}

// TestConsolePageDoesNotUnwrapHostBody 验证页面不会误剥响应字段。
//
// 宿主把插件的响应体**原样**透传给浏览器：{ok, result} 那层信封是插件与宿主
// 之间的 RPC 格式，不会到达前端。若页面再剥一层 "result"，所有业务字段都会
// 变成 undefined——表现就是「接口 200 但界面全空」，且极难定位。
func TestConsolePageDoesNotUnwrapHostBody(t *testing.T) {
	page := Page("/plugins/freetier2api")

	// 注意用正则精确匹配：字符串包含 "data.result" 也会命中 "data.results"（合法字段）。
	if unwrapRe.MatchString(page) {
		t.Fatal("console page must not unwrap a second envelope layer (the host passes the body through verbatim)")
	}
	// 业务字段必须是从响应顶层直接取的。
	for _, field := range []string{
		"status.accounts",       // 账号列表
		"payload.entries",       // 日志
		"payload.pending_count", // 扫描结果
		"data.models",           // 模型清单
		"data.results",          // 额度/任务结果
	} {
		if !strings.Contains(page, field) {
			t.Fatalf("console page must read %q directly from the response body", field)
		}
	}
}

// TestConsolePageWiresHostOAuthLogin 验证「添加账号」接的是宿主的 OAuth 端点。
//
// 宿主为插件提供两段式登录：
//
//	GET /v0/management/<provider>-auth-url?realm=...   → 转给 auth.login.start
//	GET /v0/management/get-auth-status?state=...       → 转给 auth.login.poll
//
// 页面必须走这两个端点（而不是插件自己的路由），否则「添加账号」不可用。
func TestConsolePageWiresHostOAuthLogin(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		"/freetier-auth-url", // 与 providerKey 拼接，宿主按此匹配 auth provider
		"get-auth-status",    // 宿主提供的状态轮询端点
		`id="addVendorMenu"`, // 供应商下拉菜单（替代原先的两个固定按钮）
		`id="btnAddAccount"`, // 添加账号入口
		"startLogin",         // 登录流程
		"pollLogin",          // 轮询流程
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the add-account wiring (%s)", literal)
		}
	}
	// 这两个接口在**宿主根路径**下（/v0/management/<provider>-auth-url），
	// 不能带插件前缀：宿主按路径提取 provider，带前缀会得到
	// "plugins/freetier2api/workbuddy"，校验失败直接 404。
	if !strings.Contains(page, "hostPath(") {
		t.Fatal("host-level OAuth endpoints must not be prefixed with the plugin path")
	}
	if strings.Contains(page, `api("GET", "/freetier-auth-url`) {
		t.Fatal("auth-url must go through hostPath() (the host extracts the provider from the path)")
	}
	if !strings.Contains(page, `var HOST_MGMT = "/v0/management"`) {
		t.Fatal("console page must know the host management prefix")
	}

	// 供应商必须可选：页面从 /vendors 拉清单填进下拉，且登录时把 vendor
	// 作为参数传给宿主（宿主只认路径里的 provider，选哪家由插件按参数决定）。
	if !strings.Contains(page, "loadVendors") {
		t.Fatal("console page must load the vendor list from the backend")
	}
	if !strings.Contains(page, "vendor=") {
		t.Fatal("console page must pass the selected vendor to the host login endpoint")
	}
}

// TestConsolePageIsSingleScrollLayout 验证页面是单页滚动而非标签页。
//
// 标签页会把额度、模型、任务藏起来，管理页应当一眼看全。
func TestConsolePageIsSingleScrollLayout(t *testing.T) {
	page := Page("/plugins/freetier2api")

	// 不应再有导航与视图切换。
	for _, forbidden := range []string{"<nav>", "data-view=", "switchView", `id="view-accounts" hidden`} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("console page must be a single scrolling page, found %q", forbidden)
		}
	}
	// 各区块都必须在页面上直接可见（不带 hidden）。
	for _, section := range []string{"账号", "额度", "模型", "任务", "设置", "运行日志"} {
		if !strings.Contains(page, section) {
			t.Fatalf("console page is missing the %q section", section)
		}
	}
	// 额度与模型必须有自己的容器与按钮。
	for _, literal := range []string{
		`id="btnLoadQuotas"`, // 额度：按钮 + 并入账号表的列
		`id="models"`, `id="btnLoadModels"`, `id="modelFilter"`,
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing %s", literal)
		}
	}
	// 任务执行进度不再单独一张表：并入各账号的任务清单。
	if strings.Contains(page, `id="queue"`) {
		t.Fatal("queue progress must be merged into the per-account task lists")
	}
	if !strings.Contains(page, `id="queueProgress"`) {
		t.Fatal("console page must show a compact queue progress summary")
	}

	// 额度不再单独占一张表：结果并入账号表的「剩余 / 总额」与「使用率」两列。
	if strings.Contains(page, `id="quotas"`) {
		t.Fatal("quota must be merged into the accounts table, not a separate section")
	}
	if !strings.Contains(page, "剩余 / 总额") || !strings.Contains(page, "使用率") {
		t.Fatal("accounts table must carry the quota columns")
	}
}

// TestConsolePageHasBulkTaskButton 验证「一键完成」是批量按钮而非每账号一个。
//
// 逐账号一个按钮在账号多时既难用也容易误点；统一成「全部账号依次执行」。
func TestConsolePageHasBulkTaskButton(t *testing.T) {
	page := Page("/plugins/freetier2api")

	if !strings.Contains(page, `id="btnAutoAll"`) {
		t.Fatal("console page must expose a bulk task button")
	}
	if !strings.Contains(page, "/tasks/auto_all") {
		t.Fatal("bulk task button must call the /tasks/auto_all endpoint")
	}
	// 表格里不应再逐账号渲染「一键完成」按钮（签到按钮保留）。
	if strings.Contains(page, `el("button", "一键完成"`) {
		t.Fatal("per-account task buttons must be replaced by the bulk button")
	}
}

// TestConsolePageStopsPollingOnAuthFailure 验证鉴权失败会停止轮询。
//
// CPA 按客户端 IP 统计管理鉴权失败次数，连续失败会封禁该 IP
// （连正确密钥也会被拒），因此必须停。
func TestConsolePageStopsPollingOnAuthFailure(t *testing.T) {
	page := Page("/plugins/freetier2api")
	if !strings.Contains(page, "haltOnAuthFailure") {
		t.Fatal("console page must stop polling on auth failure to avoid an IP ban")
	}
	if !strings.Contains(page, "401") || !strings.Contains(page, "403") {
		t.Fatal("console page must treat both 401 and 403 as auth failures")
	}
}

// TestConsolePageIsStaticAndSelfContained 验证页面不含任何凭证数据。
//
// 资源页不做管理鉴权，因此页面本身绝不能带账号或令牌。
func TestConsolePageIsStaticAndSelfContained(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, forbidden := range []string{"accessToken", "refreshToken", "device_token", "Authorization: Bearer sk-"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("console page must not embed credential material (%q found)", forbidden)
		}
	}
	// 唯一的网络目标必须是 CPA 自身。
	if strings.Contains(page, "http://") && !strings.Contains(page, "http://127.0.0.1") {
		t.Fatal("console page must not reference external origins")
	}
	// 页面必须能停止自动刷新（避免触发 CPA 的按 IP 鉴权封禁）。
	if !strings.Contains(page, "haltOnAuthFailure") {
		t.Fatal("console page must stop polling on auth failure")
	}
}

// TestTaskScheduleFoldedByDefault 验证「任务排程」默认折叠。
//
// 排程有十来个任务族，展开后会把设置卡片撑得很长；默认折叠是刻意的交互
// 选择。这里同时锁住三件事：用原生 <details>（无JS 也能开合）、默认不带
// open 属性、以及折叠时仍能在 summary 上看到当前排程摘要。
func TestTaskScheduleFoldedByDefault(t *testing.T) {
	page := Page("/plugins/freetier2api")

	if !strings.Contains(page, `<details class="fold" id="scheduleFold">`) {
		t.Fatal(`任务排程未用 <details class="fold" id="scheduleFold"> 包裹`)
	}
	if strings.Contains(page, `<details class="fold" id="scheduleFold" open`) {
		t.Fatal("任务排程必须默认折叠，不应带 open 属性")
	}
	// summary 只留标题：曾经在这里拼「每日签到 9,21 · 猫猫旅行 9,21 · ...」，
	// 默认折叠时也占满一行，且排程改动后这段摘要极易与真实配置不一致。
	if strings.Contains(page, "scheduleSummary") || strings.Contains(page, "foldcount") {
		t.Error("任务排程的折叠标题不应再展示排程明细摘要")
	}
	if !strings.Contains(page, "<summary>任务排程</summary>") {
		t.Error("任务排程折叠标题应为纯文本「任务排程」")
	}
	// 折叠箭头放大到 22px（原 11px），避免在大标题行里点不准。
	if !strings.Contains(page, `content: "\25B8"; font-size: 22px`) {
		t.Error("任务排程折叠箭头未放大到 22px")
	}
}

// TestConsolePageWiresLoginCallbackPasteBox 验证控制台把「粘贴回调链接」接到了插件
// 自己的管理路由上。
//
// 远程 / Docker 部署时上游只认 localhost 回调，浏览器到不了插件的监听；页面若
// 没有这条兜底通道，Trae / CodeArts 的登录就永远停在「等待授权中…」。这里同时
// 锁住三件事：折叠块存在、提交按钮存在、请求打的是插件自有路由（不能是宿主路由
// ——宿主没有这个端点，打过去只会 404）。
func TestConsolePageWiresLoginCallbackPasteBox(t *testing.T) {
	page := Page("/plugins/freetier2api")
	for _, marker := range []string{
		`id="loginPasteBox"`,
		`id="loginPasteUrl"`,
		`id="btnSubmitCallback"`,
		`api("POST", "/login/callback"`,
		"paste_callback",
		"callback_hint",
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("console page missing %q", marker)
		}
	}
}

// TestConsoleQuotaCellUsesCapability 锁定账号表额度列的状态判定。
//
// 「查询中…」只能表示**确实在查**。上游没有额度接口的供应商（OpenCode ZEN）
// 永远不会有结果，若页面把「没有结果」一律当成查询中，那一行就会永久卡在
// 「查询中…」，而正确展示是「-- / --」。因此分支必须读后端给的能力标志
// supports_quota，而不是靠结果是否存在反推。
func TestConsoleQuotaCellUsesCapability(t *testing.T) {
	page := Page("/plugins/freetier2api")

	if !strings.Contains(page, "account.supports_quota") {
		t.Fatal("quota cell must branch on account.supports_quota")
	}
	// 页面不得再维护自己的额度缓存：它是后端缓存的第二份副本，键还靠猜测。
	if strings.Contains(page, "quotaByAuth") {
		t.Fatal("console must not keep a second quota cache (quotaByAuth); /status is the source of truth")
	}
	// 「查询中…」必须出现在 supports_quota 为真的分支里。
	if !regexp.MustCompile(`supports_quota[\s\S]{0,400}查询中`).MatchString(page) {
		t.Fatal("「查询中…」must be shown only when the vendor supports quota")
	}
}

// TestConsoleModelChangesTriggerHostReload 锁定「模型变更自动生效」的接线。
//
// 宿主只在 plugin.register / plugin.reconfigure 时重建模型注册表，而插件自己的
// /models/toggle 与 /models/alias 不会通知宿主。宿主也没有 reload 端点，唯一可用
// 的是写插件配置触发的隐含重载：PATCH /v0/management/plugins/:id/config。
//
// 该重载按配置文件 hash 判断是否需要执行，内容不变会被跳过，所以必须写入一个
// 会变化的键（config_reloaded_at）。页面不得再显示「重启 CPA 生效」的提示条，
// 也不得提供手动重载按钮——变更是自动同步的。
func TestConsoleModelChangesTriggerHostReload(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		"syncModelConfig",                          // 同步入口
		"config_reloaded_at",                       // 制造配置内容变化的键
		`hostPath("/plugins/freetier2api/config")`, // 宿主插件配置路由
		`api("PATCH", configPath`,                  // 浅合并写配置，触发宿主重载
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing model-sync wiring (%s)", literal)
		}
	}

	// 变更成功后必须同步，而不是留给用户手动操作。
	for _, hook := range []string{"syncModelConfig();"} {
		if strings.Count(page, hook) < 2 {
			t.Fatalf("console must call %s from both model toggle and alias paths", hook)
		}
	}

	// 旧的「待生效提示条 / 重启」说法必须彻底消失。
	for _, stale := range []string{"pendingBar", "setPendingBar", "需重启CPA生效", "应用变更"} {
		if strings.Contains(page, stale) {
			t.Fatalf("console must not keep the restart-pending UI anymore (%s)", stale)
		}
	}
}

// TestConsoleLoadModelsButtonRefreshesUpstream 锁定「读取模型清单」按钮的真实行为。
//
// 事故：该按钮曾绑定 loadModels（只重读插件内存缓存）。缓存一旦为空（插件刚
// 重载、探测失败），点它永远没反应，用户也就无从恢复整家供应商的模型列表。
// 按钮必须 POST /models/refresh 去上游拉一次，失败时把上游错误显示出来——
// 401 与超时的处理方式完全不同，藏进日志等于让用户自己猜。
func TestConsoleLoadModelsButtonRefreshesUpstream(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		`api("POST", "/models/refresh"`, // 真正触发上游拉取
		`refreshModels(false);`,         // 按钮绑定到刷新入口
		"从上游刷新失败：",                      // 上游错误原样呈现
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing model-refresh wiring (%s)", literal)
		}
	}
	if strings.Contains(page, `addEventListener("click", loadModels`) {
		t.Fatal("the load-models button must not just re-read the local cache")
	}
}

// TestConsoleModelTableMergesIDAndCredits 锁定模型表的列重构：
// 「模型 ID」与「别名」合并为一列，原「别名」位置改为「费用倍率」。
func TestConsoleModelTableMergesIDAndCredits(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		"MODEL_COLUMNS",    // 表头与单元格同源的列描述表
		"modelIDCell",      // 合并列（生效 ID + ✎）
		"modelCreditsCell", // 费用倍率列
		"model.credits",    // 读后端下发的 credits
		`"模型 ID"`,
		`"费用倍率"`,
		`"✎"`,              // 铅笔图标（字面量，不引入 SVG/innerHTML）
		"creditsSortOrder", // 倍率列点击排序
		"modelCreditValue", // 倍率值解析（无倍率沉底）
		"th.sortable",      // 表头可排序样式
		"sort-arrows",      // 双三角排序指示器容器
		"sort-arrow asc",   // 向上三角
		"sort-arrow desc",  // 向下三角
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing model-table marker %q", literal)
		}
	}

	// 旧的独立「别名」表头必须消失：它现在是模型 ID 列的一部分。
	if strings.Contains(page, `"名称", "别名"`) {
		t.Fatal("alias column must be merged into the model-id column")
	}

	// 编辑仍走原接口与同步入口（回归保护）。
	for _, literal := range []string{
		`api("POST", "/models/alias"`,
		"syncModelConfig();",
		"scope_id",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("alias editing wiring lost: %s", literal)
		}
	}
}

// TestConsoleModelOptionalColumnsHiddenByDefault 锁定「输出上限/推理档位/能力」
// 可切换且默认显隐符合规范，偏好按浏览器持久化。
func TestConsoleModelOptionalColumnsHiddenByDefault(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		`id="btnModelCols"`,
		`id="modelColsMenu"`,
		"cols-setter",
		`"freetier-model-optional-columns"`,
		"localStorage.getItem(MODEL_COLUMNS_STORE_KEY)",
		"localStorage.setItem(MODEL_COLUMNS_STORE_KEY",
		"function readColumnPrefs()",
		"function visibleColumns()",
		"function toggleOptionalColumn(",
		"列展示 ▾",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing column-visibility wiring %q", literal)
		}
	}

	// 共有三列可切换：输出上限、推理档位、能力——其余列必须恒显。
	if got := strings.Count(page, "optional: true"); got != 3 {
		t.Fatalf("optional columns = %d, want 3 (输出上限/推理档位/能力)", got)
	}
	for _, key := range []string{"output", "efforts", "caps"} {
		pattern := regexp.MustCompile(`key: "` + key + `"[\s\S]{0,120}optional: true`)
		if !pattern.MatchString(page) {
			t.Fatalf("column %q must be optional", key)
		}
	}

	// 输出上限、推理档位默认隐藏，能力默认展示
	for _, key := range []string{"output", "efforts"} {
		pattern := regexp.MustCompile(`key: "` + key + `"[\s\S]{0,120}defaultShow: false`)
		if !pattern.MatchString(page) {
			t.Fatalf("column %q must have defaultShow: false", key)
		}
	}
	if !regexp.MustCompile(`key: "caps"[\s\S]{0,120}defaultShow: true`).MatchString(page) {
		t.Fatal(`column "caps" must have defaultShow: true`)
	}

	// 缺省/损坏/存储不可用时必须回到空对象。
	if !regexp.MustCompile(`catch \(e\) \{\s*return \{\};`).MatchString(page) {
		t.Fatal("column preferences must default to an empty object")
	}
}

func TestConsolePageSettingsLabelsAndDisclaimer(t *testing.T) {
	page := Page("/plugins/freetier2api")
	for _, required := range []string{
		"每日自动签到",
		"每日自动任务",
		"合规与免责声明",
		"MAX_ACCOUNTS_PER_VENDOR = 10",
	} {
		if !strings.Contains(page, required) {
			t.Fatalf("console page missing %q", required)
		}
	}
	if strings.Contains(page, "自动每日任务") {
		t.Fatal("console page must replace 自动每日任务 with 每日自动任务")
	}
}

// TestConsoleSettingsSyncsModelRegistryImmediately 锁定渠道标识保存后立即生效：
// 响应带 model_registry_dirty 时复用 syncModelConfig 触发宿主重载，页面不再
// 让用户「等模型清单刷新（或重启宿主）」。
func TestConsoleSettingsSyncsModelRegistryImmediately(t *testing.T) {
	page := Page("/plugins/freetier2api")
	if !strings.Contains(page, "if (data && data.model_registry_dirty) { syncModelConfig(); }") {
		t.Fatal("保存设置后必须按 model_registry_dirty 触发 syncModelConfig")
	}
	for _, stale := range []string{"等模型清单刷新", "给各供应商的模型名自动加渠道标识"} {
		if strings.Contains(page, stale) {
			t.Fatalf("渠道标识即时生效，页面不应再有旁注：%s", stale)
		}
	}
}

// TestConsoleAccountAliasReusesInlineEditor 锁定账号别名的接线与「只有一份」。
//
// 账号别名与模型别名是同一套交互（点击 ✎、回车/失焦保存、Esc 取消），实现
// 必须只有 editableCell 一份：复制第二份意味着键盘行为、回退策略、错误提示
// 迟早分叉。账号别名按账号身份 file_id 提交，不用昵称/邮箱。
func TestConsoleAccountAliasReusesInlineEditor(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		"function editableCell(opts)", // 共用编辑器
		"function accountNameCell(account)",
		"accountNameCell(account)", // 账号列确实用它渲染
		`api("POST", "/accounts/alias"`,
		"account.file_id",
		"account.alias",
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page missing account-alias wiring %q", literal)
		}
	}

	if count := strings.Count(page, `input.addEventListener("blur"`); count != 1 {
		t.Fatalf("inline editing must have exactly one implementation, found %d blur handlers", count)
	}
}

// TestConsolePageShowsCheckinHistory 验证签到历史在控制台可展开查看。
//
// 横幅只显示几秒钟，失败账号与报错原文必须能在页面上事后回溯；历史入口挂在
// 每个账号自己的签到列上（同一张表、同一份 /status 数据，不再发额外请求）。
func TestConsolePageShowsCheckinHistory(t *testing.T) {
	page := Page("/plugins/freetier2api")

	for _, literal := range []string{
		"account.checkin_history",      // 历史来自 /status 的账号条目
		"buildCheckinHistoryRow",       // 展开行的构造点
		`entry.trigger === "schedule"`, // 触发方式要在页面上区分定时/手动
		"checkinStatusPill",            // 三种状态各自着色
	} {
		if !strings.Contains(page, literal) {
			t.Fatalf("console page is missing the checkin history UI (%s)", literal)
		}
	}
}
