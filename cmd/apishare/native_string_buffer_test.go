package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestNativeStringBuffersMatchIncrementalConcatenation(t *testing.T) {
	for _, parts := range [][]string{
		{"", "first", " second", ""},
		{"Hel", "lo", "", "!"},
		{"工具", "参数", "😀"},
	} {
		buffers := nativeStringBuffers{}
		key := nativeStringKey{stream: "test", index: 4, subindex: 2, field: "text"}
		var want strings.Builder
		for _, part := range parts {
			want.WriteString(part)
			buffers.append(key, "", part)
			got := ""
			if buffer := buffers[key]; buffer != nil {
				got = buffer.String()
			}
			if got != want.String() {
				t.Fatalf("parts=%q: got %q, want %q", parts, got, want.String())
			}
		}
	}
}

func TestNativeStringBuffersRespectAuthoritativeSnapshots(t *testing.T) {
	key := nativeStringKey{stream: "test", index: 1, field: "text"}
	buffers := nativeStringBuffers{}
	buffers.append(key, "", "partial")
	buffers.replace(key, "complete snapshot")
	buffers.append(key, "", " + tail")
	if got := buffers[key].String(); got != "complete snapshot + tail" {
		t.Fatalf("snapshot was not used as the new append base: %q", got)
	}
}

func TestNativeStringBuffersIncrementalAllocationsGrowSublinearly(t *testing.T) {
	const chunks = 4096
	chunk := strings.Repeat("x", 32)
	allocs := testing.AllocsPerRun(5, func() {
		buffers := nativeStringBuffers{}
		key := nativeStringKey{stream: "test", index: 1, field: "text"}
		for i := 0; i < chunks; i++ {
			buffers.append(key, "", chunk)
		}
	})
	if allocs > 64 {
		t.Fatalf("incremental buffers allocated %v times for %d chunks", allocs, chunks)
	}
	t.Logf("%d chunks use %.0f allocations", chunks, allocs)
}

func BenchmarkNativeStringBuffersAppend(b *testing.B) {
	for _, chunks := range []int{1024, 4096} {
		b.Run(fmt.Sprintf("chunks=%d", chunks), func(b *testing.B) {
			chunk := strings.Repeat("x", 32)
			key := nativeStringKey{stream: "test", index: 1, field: "text"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buffers := nativeStringBuffers{}
				for j := 0; j < chunks; j++ {
					buffers.append(key, "", chunk)
				}
			}
		})
	}
}
