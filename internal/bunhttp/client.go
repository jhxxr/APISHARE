// Copyright (C) 2026 Jason Xu <jasonxu114514@163.com>
// Copyright (C) 2026 API Share contributors (modifications)
// SPDX-License-Identifier: GPL-3.0-or-later

package bunhttp

// openCodeVersionV2 is the OpenCode 2.x CLI release the gateway claims to be on
// the v2 lanes (/inference and /zen/go). Upstream accepts a range of releases
// (the version number itself is not validated), so this only needs bumping
// when it starts rejecting older ones.
const openCodeVersionV2 = "2.0.32"

// openCodeVersionV1 is the OpenCode 1.x release the gateway claims to be on the
// v1 lane (/zen), and openCodeBunVersion is the Bun runtime that release ships with.
// Bump them together with the provider-utils versions below, all taken from a
// captured request of the same release.
const (
	openCodeVersionV1  = "1.19.3"
	openCodeBunVersion = "1.3.14"
)

// The AI SDK provider packages of OpenCode 1.19.3 each bundle their own
// provider-utils, whose version ends up in the User-Agent of every v1 model
// request, so the version depends on the wire protocol.
const (
	// OpenCodeV1ChatProviderUtils is bundled by @ai-sdk/openai-compatible (chat completions).
	OpenCodeV1ChatProviderUtils = "4.0.23"
	// OpenCodeV1ResponsesProviderUtils is bundled by @ai-sdk/openai (responses).
	OpenCodeV1ResponsesProviderUtils = "4.0.40"
	// OpenCodeV1AnthropicProviderUtils is bundled by @ai-sdk/anthropic (messages).
	OpenCodeV1AnthropicProviderUtils = "4.0.46"
)

// OpenCodeV2UserAgent reports the gateway as an OpenCode 2.x CLI using the shape the
// v2 gate expects: "opencode/<channel>/<version>/<name>".
func OpenCodeV2UserAgent() string {
	return "opencode/latest/" + openCodeVersionV2 + "/cli"
}

// OpenCodeV1UserAgent is the OpenCode 1.x client's own User-Agent, a bare
// "opencode/<version>", which it sends as-is on requests that bypass the AI
// SDK. The earlier "(<os> <arch>; <runtime>)" suffix carried the gateway's Go
// runtime, which no real OpenCode client sends.
func OpenCodeV1UserAgent() string {
	return "opencode/" + openCodeVersionV1
}

// OpenCodeV1ModelUserAgent is the User-Agent of a v1 model request: the AI SDK appends
// the provider-utils version of the package serving the request and the
// runtime to the client's own User-Agent, e.g.
// "opencode/1.19.3 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14".
func OpenCodeV1ModelUserAgent(providerUtils string) string {
	return OpenCodeV1UserAgent() + " ai-sdk/provider-utils/" + providerUtils + " runtime/bun/" + openCodeBunVersion
}
