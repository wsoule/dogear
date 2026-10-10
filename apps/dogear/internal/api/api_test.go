package api

import (
	"regexp"
	"strings"
	"testing"

	"dogear/internal/auth"
	"dogear/internal/safety"
	"dogear/internal/service"
)

// TestSpecCoversRoutes fails when a registered route is missing from
// openapi.yaml, so the published spec never drifts from the server.
func TestSpecCoversRoutes(t *testing.T) {
	a := New(&service.Service{}, nil, auth.Signer{}, "https://dogear.fyi", safety.ProxyTrust{})
	paths := map[string]map[string]bool{}
	var current string
	pathRe := regexp.MustCompile(`^  (/\S*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|put|post|patch|delete):`)
	for _, line := range strings.Split(openapiYAML, "\n") {
		if m := pathRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			paths[current] = map[string]bool{}
			continue
		}
		if m := methodRe.FindStringSubmatch(line); m != nil && current != "" {
			paths[current][strings.ToUpper(m[1])] = true
		}
	}
	for _, route := range a.Routes {
		method, path, _ := strings.Cut(route, " ")
		if !paths[path][method] {
			t.Errorf("route %s %s is missing from openapi.yaml", method, path)
		}
	}
	if len(a.Routes) < 20 {
		t.Fatalf("expected the full route table, got %d", len(a.Routes))
	}
}

func TestGPTSpecHasAgentOperations(t *testing.T) {
	for _, op := range []string{"listCategories", "searchItems", "addItem", "voteItem", "addToList"} {
		if !strings.Contains(gptYAML, "operationId: "+op) {
			t.Errorf("gpt.yaml missing %s", op)
		}
	}
	if n := strings.Count(gptYAML, "operationId:"); n != 5 {
		t.Errorf("gpt.yaml should have exactly 5 operations, has %d", n)
	}
}
