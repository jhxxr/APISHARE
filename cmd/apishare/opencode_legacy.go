package main

import (
	"io"

	"apishare/internal/opencodecompat"
)

func newOpenCodeAliasBody(source io.ReadCloser, format clientFormat, compatibility openCodeToolCompatibility, sse bool) io.ReadCloser {
	return opencodecompat.NewResponseBody(source, opencodecompat.Protocol(format), compatibility.Renames, sse)
}
