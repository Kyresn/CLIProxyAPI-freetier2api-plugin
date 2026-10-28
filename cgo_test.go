package main

// 本文件覆盖 C ABI 入口的请求缓冲区处理：借用 / 复制的分派、借用期间不修改
// 宿主缓冲区、响应不依赖宿主缓冲区，以及真实 C 堆内存上的同一路径。

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
	"unsafe"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// normalizeRequestJSON 造一份与宿主同构的归一化请求（含两份大请求体）。
func normalizeRequestJSON(tb testing.TB, target string, bodySize int) []byte {
	tb.Helper()
	rpc := pluginapi.ResponseTransformRequest{
		FromFormat: "openai", ToFormat: target, Stream: true,
		OriginalRequest:   bytes.Repeat([]byte("a"), bodySize),
		TranslatedRequest: bytes.Repeat([]byte("b"), bodySize),
		Body:              []byte(`{"id":"c1","choices":[]}`),
	}
	raw, errMarshal := json.Marshal(rpc)
	if errMarshal != nil {
		tb.Fatal(errMarshal)
	}
	return raw
}

// callBorrowedNormalize 走与 cliproxyPluginCall 完全相同的顺序：先取请求切片，再交给 handleMethod。
func callBorrowedNormalize(tb testing.TB, request unsafe.Pointer, requestLen uintptr) []byte {
	tb.Helper()
	requestBytes, errRequest := hostRequestBytes(pluginabi.MethodResponseNormalizeBefore, request, requestLen)
	if errRequest != nil {
		tb.Fatal(errRequest)
	}
	raw, errHandle := handleMethod(pluginabi.MethodResponseNormalizeBefore, requestBytes)
	if errHandle != nil {
		tb.Fatal(errHandle)
	}
	return raw
}

// TestHostRequestBytesBorrowsOnlyReadOnlyMethods 守住分派本身：
// 归一化钩子借用宿主缓冲区，其余 RPC 必须复制。
func TestHostRequestBytesBorrowsOnlyReadOnlyMethods(t *testing.T) {
	payload := []byte(`{"stream":true}`)
	hostPtr := unsafe.Pointer(&payload[0])

	borrowed, errBorrow := hostRequestBytes(pluginabi.MethodResponseNormalizeBefore, hostPtr, uintptr(len(payload)))
	if errBorrow != nil {
		t.Fatalf("借用路径报错: %v", errBorrow)
	}
	if unsafe.Pointer(unsafe.SliceData(borrowed)) != hostPtr {
		t.Fatal("response.normalize_before 应借用宿主缓冲区，实际发生了复制")
	}

	for _, method := range []string{
		pluginabi.MethodExecutorExecute,
		pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodAuthRefresh,
		pluginabi.MethodPluginRegister,
	} {
		copied, errCopy := hostRequestBytes(method, hostPtr, uintptr(len(payload)))
		if errCopy != nil {
			t.Fatalf("%s 复制路径报错: %v", method, errCopy)
		}
		if unsafe.Pointer(unsafe.SliceData(copied)) == hostPtr {
			t.Fatalf("%s 必须复制宿主缓冲区，不能借用", method)
		}
		if !bytes.Equal(copied, payload) {
			t.Fatalf("%s 复制的字节不一致: %q", method, copied)
		}
	}
}

// TestHostRequestBytesRequestLengthContract 覆盖空请求与宿主违约（nil + 非零长度）。
func TestHostRequestBytesRequestLengthContract(t *testing.T) {
	payload := []byte(`{"a":1}`)
	ptr := unsafe.Pointer(&payload[0])

	for _, tc := range []struct {
		name  string
		ptr   unsafe.Pointer
		len   uintptr
		wantE bool
	}{
		{name: "nil_nil", ptr: nil, len: 0},
		{name: "ptr_zero", ptr: ptr, len: 0},
		{name: "nil_with_length", ptr: nil, len: 10, wantE: true},
	} {
		for _, method := range []string{pluginabi.MethodResponseNormalizeBefore, pluginabi.MethodExecutorExecute} {
			got, errGot := hostRequestBytes(method, tc.ptr, tc.len)
			if tc.wantE {
				if errGot == nil {
					t.Fatalf("%s/%s: nil 指针配非零长度必须报错", tc.name, method)
				}
				continue
			}
			if errGot != nil {
				t.Fatalf("%s/%s: 空请求不应报错: %v", tc.name, method, errGot)
			}
			if len(got) != 0 {
				t.Fatalf("%s/%s: 空请求应得到空切片，实际 %d 字节", tc.name, method, len(got))
			}
		}
	}
}

// TestBorrowedNormalizePreservesHostBuffer 验证借用路径不修改宿主缓冲区，
// 且响应与宿主缓冲区脱钩（清空后仍然一致）。
func TestBorrowedNormalizePreservesHostBuffer(t *testing.T) {
	for _, target := range []string{"claude", "openai"} {
		t.Run(target, func(t *testing.T) {
			raw := normalizeRequestJSON(t, target, 256<<10)
			before := bytes.Clone(raw)

			want, errWant := handleMethod(pluginabi.MethodResponseNormalizeBefore, before)
			if errWant != nil {
				t.Fatalf("复制路径: %v", errWant)
			}
			got := callBorrowedNormalize(t, unsafe.Pointer(unsafe.SliceData(raw)), uintptr(len(raw)))
			if !bytes.Equal(got, want) {
				t.Fatalf("借用响应 %q 与复制响应 %q 不一致", got, want)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("归一化修改了宿主缓冲区")
			}
			for i := range raw {
				raw[i] = 0
			}
			if !bytes.Equal(got, want) {
				t.Fatal("响应依赖宿主缓冲区，调用返回后仍可被改写")
			}
			runtime.KeepAlive(raw)
		})
	}
}

// BenchmarkResponseNormalizeABI 对照「整包复制」与「借用」两条入口路径。
func BenchmarkResponseNormalizeABI(b *testing.B) {
	for _, target := range []string{"openai", "claude"} {
		raw := normalizeRequestJSON(b, target, 4<<20)
		hostPtr := unsafe.Pointer(unsafe.SliceData(raw))
		b.Run(target+"/copy", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cloned := make([]byte, len(raw))
				copy(cloned, raw)
				if _, err := handleMethod(pluginabi.MethodResponseNormalizeBefore, cloned); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(target+"/borrow", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := callBorrowedNormalize(b, hostPtr, uintptr(len(raw))); len(got) == 0 {
					b.Fatal("借用路径返回空响应")
				}
			}
			runtime.KeepAlive(raw)
		})
	}
}
