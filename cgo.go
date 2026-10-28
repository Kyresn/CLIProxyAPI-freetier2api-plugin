package main

// 本文件声明 CPA 插件宿主的 C ABI 与宿主回调的 CGO 实现。
//
// 宿主在 dlopen 之后调用 cliproxy_plugin_init 传入两个函数表；
// 之后的每次 RPC 走 cliproxyPluginCall，插件发起的宿主回调走注释下方
// 的 nativeHostCallScoped。结构体布局必须与宿主 sdk/pluginabi 逐字节一致。

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

// 宿主函数表的访问包装。必须是 static：cgo 会把 preamble 复制进多个
// 生成文件，非 static 定义会导致重复符号链接失败。宿主指针由 Go 侧
// 保存并在每次调用时传入，因此这里不再需要 C 层全局变量。
static int cpa_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (api == NULL || api->call == NULL) {
		return 1;
	}
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void cpa_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	if (api != NULL && api->free_buffer != NULL && ptr != NULL) {
		api->free_buffer(ptr, len);
	}
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"time"
	"unsafe"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// hostAPI 保存宿主在 cliproxy_plugin_init 传入的函数表指针。
// 生命周期由宿主保证：Shutdown 排空后宿主才会 dlclose。
var hostAPI *C.cliproxy_host_api

// cliproxy_plugin_init 由宿主在加载动态库后调用一次。
// 返回 0 表示成功；非 0 会让宿主判定插件加载失败并回滚。
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall 是宿主发起的所有 RPC 的唯一入口。
//
// 注意错误处理约定：业务错误既写进响应信封（让调用方能读到 code/message），
// 也返回非 0 code。宿主两者都会看——信封给上层，code 给 C 层。
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, core.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	methodName := C.GoString(method)
	var raw []byte
	var errHandle error
	if methodName == pluginabi.MethodResponseNormalizeBefore {
		// The normalizer is synchronous and read-only; its input remains valid
		// until this call returns. Avoid copying the complete request per chunk.
		raw, errHandle = handleBorrowedResponseNormalize(unsafe.Pointer(request), uintptr(requestLen))
	} else {
		var requestBytes []byte
		if request != nil && requestLen > 0 {
			requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
		}
		raw, errHandle = handleMethod(methodName, requestBytes)
	}
	if errHandle != nil {
		writeResponse(response, core.ErrorEnvelopeFromError(errHandle))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

// cliproxyPluginFree 释放插件用 C.CBytes 分配的响应缓冲区，由宿主调用。
//
//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

// cliproxyPluginShutdown 由宿主在卸载动态库前调用。
//
// 顺序不可调换：
//  1. 停后台循环（签到调度、任务队列），让它们不再发起新的宿主调用；
//  2. 取消并等待在途流式转发，使其在关闭前完成 host.stream.close；
//  3. 关闭宿主调用准入并等待已进入的 CGO 调用返回；
//  4. 清空 host api。
//
// 宿主在 Shutdown 之后会 dlclose 本动态库：任何仍在执行的 Go/CGO 栈都会崩溃，
// 因此第 3 步必须等待排空（见 hostgate.go 的说明）。
const (
	// shutdownDiagnosticInterval 是关闭排空期打印诊断日志的间隔。
	shutdownDiagnosticInterval = 5 * time.Second
	// shutdownWaitTimeout 只用于等待本插件自己的流式 goroutine。
	shutdownWaitTimeout = 10 * time.Second
)

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pluginApp.StopBackgroundWork()
	pluginApp.WaitPluginStreams(shutdownWaitTimeout)
	hostbridge.WaitForShutdown(shutdownDiagnosticInterval)
	hostAPI = nil
}

// writeResponse 把插件响应复制到 C 堆缓冲区，所有权交给宿主（宿主用 free_buffer 释放）。
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// main 是 c-shared 构建模式的硬性要求：即使产出的是动态库，
// Go 的 main 包仍必须存在一个 main 函数。它永远不会被调用。
func main() {}

// nativeHostCallScoped 是宿主回调的真实实现（由 hostbridge.Configure 注入）。
func nativeHostCallScoped(callbackID string, method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := hostbridge.MarshalPayload(callbackID, payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("%s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("%s: allocate host callback payload", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	if hostAPI == nil {
		return nil, fmt.Errorf("%s: host api not initialized", method)
	}
	callCode := C.cpa_call_host(hostAPI, cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.cpa_free_host_buffer(hostAPI, response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("%s: host callback returned no response, code=%d", method, int(callCode))
	}
	result, errDecode := hostbridge.DecodeEnvelope(rawResponse)
	if errDecode != nil {
		return nil, fmt.Errorf("%s: %w", method, errDecode)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("%s: host callback returned code=%d", method, int(callCode))
	}
	return result, nil
}

// init 注入 CGO 实现：之后 root 与 internal 包统一走 hostbridge.Call。
func init() {
	hostbridge.Configure(nativeHostCallScoped)
}
