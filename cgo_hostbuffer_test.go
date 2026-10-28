//go:build linux || darwin

package main

// 本文件用操作系统匿名映射充当「宿主缓冲区」：这块内存不在 Go 堆里，与真实
// C ABI 传进来的一致（Go 不支持在测试文件里 import "C"，所以不走 cgo）。
// Windows 侧由 cgo_test.go 的 Go 内存用例兜底。

import (
	"bytes"
	"syscall"
	"testing"
	"unsafe"

	"freetier2api-plugin/cpasdk/pluginabi"
)

// hostLikeBuffer 申请一块宿主缓冲区口径的内存（页对齐、不在 Go 堆）。
func hostLikeBuffer(t *testing.T, payload []byte) []byte {
	t.Helper()
	size := (len(payload) + 4095) / 4096 * 4096
	region, errMap := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if errMap != nil {
		t.Fatalf("mmap 失败: %v", errMap)
	}
	t.Cleanup(func() {
		if errUnmap := syscall.Munmap(region); errUnmap != nil {
			t.Errorf("munmap 失败: %v", errUnmap)
		}
	})
	copy(region, payload)
	return region
}

// TestBorrowedNormalizeOnHostLikeMemory 在非 Go 堆内存上跑一遍借用路径：
// 结果正确、且不改写这块内存。
func TestBorrowedNormalizeOnHostLikeMemory(t *testing.T) {
	payload := normalizeRequestJSON(t, "claude", 64<<10)
	want, errWant := handleMethod(pluginabi.MethodResponseNormalizeBefore, payload)
	if errWant != nil {
		t.Fatalf("复制路径: %v", errWant)
	}

	region := hostLikeBuffer(t, payload)
	got := callBorrowedNormalize(t, unsafe.Pointer(unsafe.SliceData(region)), uintptr(len(payload)))
	if !bytes.Equal(got, want) {
		t.Fatal("宿主内存上的归一化结果与复制路径不一致")
	}
	if !bytes.Equal(region[:len(payload)], payload) {
		t.Fatal("归一化改写了宿主缓冲区")
	}
}

// TestBorrowedNormalizeOnReadOnlyHostBuffer 把宿主缓冲区设成 PROT_READ 后再调用：
// 归一化路径只要出现任何写入就会立刻 SIGSEGV，从而守住「只读借用」这条不变式。
func TestBorrowedNormalizeOnReadOnlyHostBuffer(t *testing.T) {
	payload := normalizeRequestJSON(t, "claude", 64<<10)
	want, errWant := handleMethod(pluginabi.MethodResponseNormalizeBefore, payload)
	if errWant != nil {
		t.Fatalf("复制路径: %v", errWant)
	}

	region := hostLikeBuffer(t, payload)
	if errReadOnly := syscall.Mprotect(region, syscall.PROT_READ); errReadOnly != nil {
		t.Skipf("mprotect 不可用，跳过只读页用例: %v", errReadOnly)
	}
	got := callBorrowedNormalize(t, unsafe.Pointer(unsafe.SliceData(region)), uintptr(len(payload)))
	if !bytes.Equal(got, want) {
		t.Fatal("只读宿主缓冲区上的归一化结果与复制路径不一致")
	}
	if errWritable := syscall.Mprotect(region, syscall.PROT_READ|syscall.PROT_WRITE); errWritable != nil {
		t.Fatalf("恢复可写失败: %v", errWritable)
	}
}
