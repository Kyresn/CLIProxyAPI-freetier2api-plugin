package main

import (
	"unsafe"

	"freetier2api-plugin/cpasdk/pluginabi"
)

// handleBorrowedResponseNormalize may read the host-owned buffer only during
// this synchronous call. The normalizer must not modify it, retain any part of
// it, or use it from a goroutine after returning to the host.
func handleBorrowedResponseNormalize(request unsafe.Pointer, requestLen uintptr) ([]byte, error) {
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = unsafe.Slice((*byte)(request), requestLen)
	}
	return handleMethod(pluginabi.MethodResponseNormalizeBefore, requestBytes)
}
