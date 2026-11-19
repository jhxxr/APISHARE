package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenCodeUnnamedProjectIDsAreFresh(t *testing.T) {
	seen := map[string]bool{}
	for _, legacy := range []bool{false, true} {
		for i := 0; i < 32; i++ {
			id := openCodeProjectID(" \t", legacy)
			if !openCodeProjectPattern.MatchString(id) || seen[id] {
				t.Fatalf("unnamed project reused an id or changed shape: %q", id)
			}
			seen[id] = true
		}
		for _, project := range []string{"global", "prj_customer", strings.Repeat("a", 40)} {
			id := openCodeProjectID(project, legacy)
			if id != openCodeProjectID(project, legacy) {
				t.Fatalf("named project became unstable: %q", project)
			}
			if project == strings.Repeat("a", 40) && id != project || project == "global" && legacy && id != project {
				t.Fatalf("official project was replaced: %q -> %q", project, id)
			}
		}
	}
}

func TestOpenCodeProjectHeaderStaysStableThroughOverridesAndClones(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			lane := "inference"
			if legacy {
				lane = "zen"
			}
			up := &Upstream{Type: "opencode", APIKey: "key", HeaderOverrides: `{"*":true}`}
			seen := map[string]bool{}
			for i := 0; i < 8; i++ {
				req := httptest.NewRequest(http.MethodPost, "https://opencode.ai/"+lane+"/v1/chat/completions", nil)
				setOpenCodeHeaders(req, "key", FormatOpenAIChat, "", nil, nil, nil)
				id := req.Header.Get("x-opencode-project")
				if !openCodeProjectPattern.MatchString(id) || seen[id] {
					t.Fatalf("unnamed headers reused a bucket: %q", id)
				}
				seen[id] = true
				incoming := http.Header{"X-Opencode-Project": {"foreign-project"}}
				if err := up.applyHeaders(req, incoming); err != nil {
					t.Fatal(err)
				}
				if got := req.Header.Get("x-opencode-project"); got != id {
					t.Fatalf("broad passthrough changed the generated project: %q -> %q", id, got)
				}
				clone := req.Clone(req.Context())
				if clone.Header.Get("x-opencode-project") != id {
					t.Fatal("retry clone changed project id")
				}
			}
		})
	}
}
