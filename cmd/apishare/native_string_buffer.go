package main

import "strings"

// nativeStringKey identifies one independently accumulated text field in a
// buffered native response. Keeping builders outside the response maps avoids
// copying the entire prefix on every SSE delta.
type nativeStringKey struct {
	stream   string
	index    int
	subindex int
	field    string
}

type nativeStringBuffers map[nativeStringKey]*strings.Builder

func (buffers nativeStringBuffers) append(key nativeStringKey, previous, delta string) string {
	if delta == "" {
		return previous
	}
	buffer := buffers[key]
	if buffer == nil {
		buffer = &strings.Builder{}
		buffers[key] = buffer
		buffer.Grow(len(previous) + len(delta))
		buffer.WriteString(previous)
	}
	buffer.WriteString(delta)
	return buffer.String()
}

func (buffers nativeStringBuffers) replace(key nativeStringKey, value string) {
	buffer := buffers[key]
	if buffer == nil {
		buffer = &strings.Builder{}
		buffers[key] = buffer
	} else if buffer.String() == value {
		return
	}
	buffer.Reset()
	buffer.Grow(len(value))
	buffer.WriteString(value)
}
