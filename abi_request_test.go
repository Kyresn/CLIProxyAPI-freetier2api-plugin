package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
	"unsafe"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
)

// The response hook must finish reading before the host can reuse its buffer.
// Exercise the borrowed entry point with both handled and no-op responses.
func TestBorrowedResponseNormalizePreservesInputAndOwnsOutput(t *testing.T) {
	for _, target := range []string{"claude", "openai"} {
		t.Run(target, func(t *testing.T) {
			rpc := pluginapi.ResponseTransformRequest{
				FromFormat: "openai", ToFormat: target, Stream: true,
				OriginalRequest:   bytes.Repeat([]byte("a"), 256<<10),
				TranslatedRequest: bytes.Repeat([]byte("b"), 256<<10),
				Body:              []byte("{\"id\":\"c1\",\"choices\":[]}"),
			}
			raw, err := json.Marshal(rpc)
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(raw)
			want, err := handleMethod(pluginabi.MethodResponseNormalizeBefore, before)
			if err != nil {
				t.Fatal(err)
			}
			got, err := handleBorrowedResponseNormalize(unsafe.Pointer(unsafe.SliceData(raw)), uintptr(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("normalizer modified the host buffer")
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("borrowed response %q differs from copied response %q", got, want)
			}
			for i := range raw {
				raw[i] = 0
			}
			if !bytes.Equal(got, want) {
				t.Fatal("response depends on the host buffer after return")
			}
			runtime.KeepAlive(raw)
		})
	}
}

func TestBorrowedResponseNormalizeEmptyRequest(t *testing.T) {
	want, err := handleMethod(pluginabi.MethodResponseNormalizeBefore, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []uintptr{0, 10} {
		got, err := handleBorrowedResponseNormalize(nil, n)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("nil request of size %d returned %q, want %q", n, got, want)
		}
	}
}

func BenchmarkResponseNormalizeABI(b *testing.B) {
	for _, target := range []string{"openai", "claude"} {
		rpc := pluginapi.ResponseTransformRequest{
			FromFormat: "openai", ToFormat: target, Stream: true,
			OriginalRequest: make([]byte, 4<<20), TranslatedRequest: make([]byte, 4<<20),
			Body: []byte("{\"id\":\"c1\",\"choices\":[]}"),
		}
		raw, err := json.Marshal(rpc)
		if err != nil {
			b.Fatal(err)
		}
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
				if _, err := handleBorrowedResponseNormalize(unsafe.Pointer(unsafe.SliceData(raw)), uintptr(len(raw))); err != nil {
					b.Fatal(err)
				}
			}
			runtime.KeepAlive(raw)
		})
	}
}
