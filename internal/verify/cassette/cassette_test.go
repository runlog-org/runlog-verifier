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
