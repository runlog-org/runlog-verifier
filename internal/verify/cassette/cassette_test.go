package cassette

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"step-a": map[string]any{
				"request":  "GET /foo\nAccept: application/json\n\n",
				"response": "200 OK\nContent-Type: application/json\n\n{\"ok\":true}",
			},
		},
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Mode != "replay" {
		t.Fatalf("mode=%q", c.Mode)
	}
	step, ok := c.Steps["step-a"]
	if !ok {
		t.Fatalf("step-a missing")
	}
	if step.Request.Method != "GET" || step.Request.Path != "/foo" {
		t.Fatalf("request=%+v", step.Request)
	}
	if step.Request.Headers["Accept"] != "application/json" {
		t.Fatalf("headers=%v", step.Request.Headers)
	}
	if step.Response.Status != 200 {
		t.Fatalf("status=%d", step.Response.Status)
	}
	if !strings.Contains(step.Response.Body, "ok") {
		t.Fatalf("body=%q", step.Response.Body)
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatalf("expected error for nil")
	}
	if _, err := Parse(map[string]any{}); err == nil {
		t.Fatalf("expected error for empty map")
	}
}

func TestParseMissingMode(t *testing.T) {
	raw := map[string]any{
		"steps": map[string]any{},
	}
	if _, err := Parse(raw); err == nil {
		t.Fatalf("expected error for missing mode")
	}
}

func TestParseMalformedRequest(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"bad": map[string]any{
				"request":  "no-space-no-path",
				"response": "200\n\n",
			},
		},
	}
	if _, err := Parse(raw); err == nil {
		t.Fatalf("expected error for malformed request")
	}
}

func TestParseMalformedResponseStatus(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"bad": map[string]any{
				"request":  "GET /foo\n\n",
				"response": "not-a-number\n\n",
			},
		},
	}
	if _, err := Parse(raw); err == nil {
		t.Fatalf("expected error for non-numeric status")
	}
}

func TestStubServesMatch(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"only": map[string]any{
				"request":  "GET /ping\n\n",
				"response": "200\nX-Test: hi\n\npong",
			},
		},
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	stub := NewStub(c, []string{"only"})
	defer stub.Close()

	resp, err := http.Get(stub.URL() + "/ping")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Test"); got != "hi" {
		t.Fatalf("X-Test=%q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pong" {
		t.Fatalf("body=%q", string(body))
	}

	if got := stub.RemainingSequence(); len(got) != 0 {
		t.Fatalf("RemainingSequence=%v", got)
	}
	if got := stub.UnmatchedRequests(); len(got) != 0 {
		t.Fatalf("UnmatchedRequests=%v", got)
	}
}

func TestStubMismatchPath(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"only": map[string]any{
				"request":  "GET /ping\n\n",
				"response": "200\n\nok",
			},
		},
	}
	c, _ := Parse(raw)
	stub := NewStub(c, []string{"only"})
	defer stub.Close()

	resp, err := http.Get(stub.URL() + "/wrong-path")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()

	unm := stub.UnmatchedRequests()
	if len(unm) != 1 {
		t.Fatalf("UnmatchedRequests=%v", unm)
	}
	if unm[0].Path != "/wrong-path" || unm[0].ExpectedPath != "/ping" {
		t.Fatalf("UnmatchedRequest=%+v", unm[0])
	}
}

func TestStubSequenceExhausted(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"a": map[string]any{"request": "GET /a\n\n", "response": "200\n\n"},
		},
	}
	c, _ := Parse(raw)
	stub := NewStub(c, []string{"a"})
	defer stub.Close()

	// First request consumes the only step.
	resp, err := http.Get(stub.URL() + "/a")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()

	// Second request — sequence is exhausted; expected unmatched.
	resp2, err := http.Get(stub.URL() + "/a")
	if err != nil {
		t.Fatalf("http.Get 2: %v", err)
	}
	resp2.Body.Close()

	unm := stub.UnmatchedRequests()
	if len(unm) != 1 {
		t.Fatalf("UnmatchedRequests=%v", unm)
	}
	if unm[0].ExpectedStep != "<sequence-exhausted>" {
		t.Fatalf("ExpectedStep=%q", unm[0].ExpectedStep)
	}
}

// queryStub is a tiny helper: build a one-step cassette with the given
// declared request line and verify whether an incoming GET against `reqURL`
// matches or lands in UnmatchedRequests.
func queryStub(t *testing.T, declaredRequest, reqPath string) (matched bool) {
	t.Helper()
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"only": map[string]any{
				"request":  declaredRequest,
				"response": "200\n\nok",
			},
		},
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", declaredRequest, err)
	}
	stub := NewStub(c, []string{"only"})
	defer stub.Close()

	resp, err := http.Get(stub.URL() + reqPath)
	if err != nil {
		t.Fatalf("http.Get(%q): %v", reqPath, err)
	}
	resp.Body.Close()

	return len(stub.UnmatchedRequests()) == 0 && resp.StatusCode == 200
}

func TestStubQueryMatchingNoQueryDeclared(t *testing.T) {
	// Declared "/foo" matches "/foo" — existing behavior, must not regress.
	if !queryStub(t, "GET /foo\n\n", "/foo") {
		t.Fatalf("expected match for declared /foo against /foo")
	}
	// Declared "/foo" matches "/foo?bar=1" — backward-compat for migrated seeds.
	if !queryStub(t, "GET /foo\n\n", "/foo?bar=1") {
		t.Fatalf("expected match for declared /foo against /foo?bar=1 (no-query cassette ignores query)")
	}
}

func TestStubQueryMatchingDeclaredQueryExact(t *testing.T) {
	// Declared "/foo?bar=1" matches "/foo?bar=1".
	if !queryStub(t, "GET /foo?bar=1\n\n", "/foo?bar=1") {
		t.Fatalf("expected match for declared /foo?bar=1 against /foo?bar=1")
	}
}

func TestStubQueryMatchingDeclaredQueryDifferentValue(t *testing.T) {
	// Declared "/foo?bar=1" must NOT match "/foo?bar=2".
	if queryStub(t, "GET /foo?bar=1\n\n", "/foo?bar=2") {
		t.Fatalf("expected mismatch for /foo?bar=1 vs /foo?bar=2")
	}
}

func TestStubQueryMatchingDeclaredQueryAbsentInRequest(t *testing.T) {
	// Declared "/foo?bar=1" must NOT match "/foo" (request has no query).
	if queryStub(t, "GET /foo?bar=1\n\n", "/foo") {
		t.Fatalf("expected mismatch for /foo?bar=1 vs /foo (no query)")
	}
}

func TestStubQueryMatchingCanonicalEquivalence(t *testing.T) {
	// Declared "/foo?a=1&b=2" must match "/foo?b=2&a=1" (key order irrelevant
	// after url.Values.Encode canonicalization).
	if !queryStub(t, "GET /foo?a=1&b=2\n\n", "/foo?b=2&a=1") {
		t.Fatalf("expected match across reordered query keys")
	}
}

func TestParseRequestStoresQueryFields(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"only": map[string]any{
				"request":  "GET /search?q=foo&page=2\n\n",
				"response": "200\n\n",
			},
		},
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	step := c.Steps["only"]
	if step.Request.Path != "/search" {
		t.Fatalf("Path=%q, want /search", step.Request.Path)
	}
	if !step.Request.HasQuery {
		t.Fatalf("HasQuery=false, want true")
	}
	if step.Request.RawPath != "/search?q=foo&page=2" {
		t.Fatalf("RawPath=%q", step.Request.RawPath)
	}
	// Query must be canonical (sorted by key).
	if step.Request.Query != "page=2&q=foo" {
		t.Fatalf("Query=%q, want canonical page=2&q=foo", step.Request.Query)
	}
}

func TestStubRemainingSequence(t *testing.T) {
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"a": map[string]any{"request": "GET /a\n\n", "response": "200\n\n"},
			"b": map[string]any{"request": "GET /b\n\n", "response": "200\n\n"},
		},
	}
	c, _ := Parse(raw)
	stub := NewStub(c, []string{"a", "b"})
	defer stub.Close()

	resp, err := http.Get(stub.URL() + "/a")
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	resp.Body.Close()

	rem := stub.RemainingSequence()
	if len(rem) != 1 || rem[0] != "b" {
		t.Fatalf("RemainingSequence=%v, want [b]", rem)
	}
}
