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

func TestCassetteCloneIsolation(t *testing.T) {
	// Clone must produce a deep copy: mutating the clone's response body /
	// status / headers must not affect the original. Used by the F22
	// cassette-response mutation framework so each mutation gets a fresh
	// perturbed cassette without corrupting the per-branch baseline.
	raw := map[string]any{
		"mode": "replay",
		"steps": map[string]any{
			"a": map[string]any{
				"request":  "GET /a\n\n",
				"response": "200\nX-Test: original\n\nbody-original",
			},
		},
	}
	orig, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	clone := orig.Clone()

	// Mutate the clone's body, status, header.
	step := clone.Steps["a"]
	step.Response.Body = "body-mutated"
	step.Response.Status = 500
	step.Response.Headers["X-Test"] = "mutated"
	step.Response.Headers["X-New"] = "added"
	clone.Steps["a"] = step

	// Original step must still observe the parsed values.
	o := orig.Steps["a"]
	if o.Response.Body != "body-original" {
		t.Errorf("orig.body=%q, want body-original (clone mutation leaked)", o.Response.Body)
	}
	if o.Response.Status != 200 {
		t.Errorf("orig.status=%d, want 200 (clone mutation leaked)", o.Response.Status)
	}
	if o.Response.Headers["X-Test"] != "original" {
		t.Errorf("orig.X-Test=%q, want original", o.Response.Headers["X-Test"])
	}
	if _, present := o.Response.Headers["X-New"]; present {
		t.Errorf("orig.X-New leaked from clone")
	}

	// And the clone must observe the perturbations.
	c := clone.Steps["a"]
	if c.Response.Body != "body-mutated" {
		t.Errorf("clone.body=%q, want body-mutated", c.Response.Body)
	}
	if c.Response.Status != 500 {
		t.Errorf("clone.status=%d, want 500", c.Response.Status)
	}
}

func TestCassetteCloneNil(t *testing.T) {
	var c *Cassette
	if got := c.Clone(); got != nil {
		t.Errorf("nil.Clone() = %v, want nil", got)
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

func TestParseRuntimeShareStateDefaultFalse(t *testing.T) {
	raw := map[string]any{
		"mode": "reexecute",
		"runtime": map[string]any{
			"tool": "docker",
		},
	}
	cas, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cas.Runtime == nil {
		t.Fatalf("runtime nil")
	}
	if cas.Runtime.ShareStateAcrossMutations {
		t.Errorf("expected ShareStateAcrossMutations=false by default, got true")
	}
}

func TestParseRuntimeShareStateTrue(t *testing.T) {
	raw := map[string]any{
		"mode": "reexecute",
		"runtime": map[string]any{
			"tool":                        "docker",
			"share_state_across_mutations": true,
		},
	}
	cas, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cas.Runtime.ShareStateAcrossMutations {
		t.Errorf("expected ShareStateAcrossMutations=true")
	}
}

func TestParseRuntimeShareStateNonBool(t *testing.T) {
	raw := map[string]any{
		"mode": "reexecute",
		"runtime": map[string]any{
			"tool":                        "docker",
			"share_state_across_mutations": "true",
		},
	}
	_, err := Parse(raw)
	if err == nil {
		t.Fatalf("expected parse error for non-bool share_state_across_mutations")
	}
	if !strings.Contains(err.Error(), "share_state_across_mutations must be a boolean") {
		t.Errorf("expected typed error, got: %v", err)
	}
}

// TestParseFixtures_DirectoryKind covers the directory fixture arm of
// parseFixtures. The arm is opt-in per kind (mirrors the sidecar_process
// arm), so the test asserts the parsed slice surfaces both file entries
// with their bodies intact and the Kind preserved for downstream coupling.
func TestParseFixtures_DirectoryKind(t *testing.T) {
	raw := map[string]any{
		"mode":    "reexecute",
		"runtime": map[string]any{"tool": "shell"},
		"fixtures": map[string]any{
			"$SOURCE_PATH": map[string]any{
				"kind": "directory",
				"files": map[string]any{
					"src/main.go": "package main",
					"Dockerfile":  "FROM alpine",
				},
			},
		},
	}
	cas, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cas.DirectoryFixtures) != 1 {
		t.Fatalf("DirectoryFixtures len=%d, want 1", len(cas.DirectoryFixtures))
	}
	df := cas.DirectoryFixtures[0]
	if df.Name != "$SOURCE_PATH" {
		t.Errorf("Name=%q, want $SOURCE_PATH", df.Name)
	}
	if df.Kind != "directory" {
		t.Errorf("Kind=%q, want directory", df.Kind)
	}
	if got := df.Files["src/main.go"]; got != "package main" {
		t.Errorf("Files[src/main.go]=%q, want %q", got, "package main")
	}
	if got := df.Files["Dockerfile"]; got != "FROM alpine" {
		t.Errorf("Files[Dockerfile]=%q, want %q", got, "FROM alpine")
	}
}

// TestParseFixtures_DockerContextKind covers the docker_context kind. The
// schema models docker_context with the same file_tree shape as directory;
// the Go model collapses them onto a single DirectoryFixture type with
// Kind preserved so downstream code (context.kind: docker_container
// pairing per the schema) can distinguish.
func TestParseFixtures_DockerContextKind(t *testing.T) {
	raw := map[string]any{
		"mode":    "reexecute",
		"runtime": map[string]any{"tool": "docker"},
		"fixtures": map[string]any{
			"$BUILD_CTX": map[string]any{
				"kind": "docker_context",
				"files": map[string]any{
					"Dockerfile": "FROM alpine\nCMD [\"true\"]\n",
				},
			},
		},
	}
	cas, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cas.DirectoryFixtures) != 1 {
		t.Fatalf("DirectoryFixtures len=%d, want 1", len(cas.DirectoryFixtures))
	}
	df := cas.DirectoryFixtures[0]
	if df.Kind != "docker_context" {
		t.Errorf("Kind=%q, want docker_context", df.Kind)
	}
	if !strings.Contains(df.Files["Dockerfile"], "FROM alpine") {
		t.Errorf("Files[Dockerfile]=%q, want FROM alpine prefix", df.Files["Dockerfile"])
	}
}

// TestParseFixtures_DirectoryRejectsUnsafePath ensures a path containing
// a ".." segment is rejected at parse time. The verifier materializes
// these files under the per-branch workdir; an escape segment in a
// hostile cassette would let it write outside the sandbox.
func TestParseFixtures_DirectoryRejectsUnsafePath(t *testing.T) {
	raw := map[string]any{
		"mode":    "reexecute",
		"runtime": map[string]any{"tool": "shell"},
		"fixtures": map[string]any{
			"$ESCAPE": map[string]any{
				"kind": "directory",
				"files": map[string]any{
					"../escape": "x",
				},
			},
		},
	}
	_, err := Parse(raw)
	if err == nil {
		t.Fatalf("expected parse error for ../escape path")
	}
	if !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("err=%v, want error mentioning \"unsafe path\"", err)
	}
}

// TestParseFixtures_DirectoryRejectsAbsolutePath covers the absolute-path
// safety check. An absolute path key would let a cassette write outside
// the workdir regardless of base; reject at parse time.
func TestParseFixtures_DirectoryRejectsAbsolutePath(t *testing.T) {
	raw := map[string]any{
		"mode":    "reexecute",
		"runtime": map[string]any{"tool": "shell"},
		"fixtures": map[string]any{
			"$ROOTED": map[string]any{
				"kind": "directory",
				"files": map[string]any{
					"/etc/passwd": "x",
				},
			},
		},
	}
	_, err := Parse(raw)
	if err == nil {
		t.Fatalf("expected parse error for absolute path")
	}
	if !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("err=%v, want error mentioning \"unsafe path\"", err)
	}
}

// TestParseFixtures_MixedFixtureKinds asserts the dispatcher routes each
// supported kind to its own slice and silently drops unknown kinds. The
// schema's tagged-union is wider than the Go model needs (per-kind
// opt-in convention).
func TestParseFixtures_MixedFixtureKinds(t *testing.T) {
	raw := map[string]any{
		"mode":    "reexecute",
		"runtime": map[string]any{"tool": "shell"},
		"fixtures": map[string]any{
			"$SIDECAR": map[string]any{
				"kind":    "sidecar_process",
				"command": []any{"sh", "-c", "sleep 0.1"},
			},
			"$DIR": map[string]any{
				"kind": "directory",
				"files": map[string]any{
					"hello.txt": "world",
				},
			},
			"$UNKNOWN": map[string]any{
				"kind": "sparse_file",
				"size": 1024,
			},
		},
	}
	cas, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cas.SidecarFixtures) != 1 {
		t.Errorf("SidecarFixtures len=%d, want 1", len(cas.SidecarFixtures))
	}
	if len(cas.DirectoryFixtures) != 1 {
		t.Errorf("DirectoryFixtures len=%d, want 1", len(cas.DirectoryFixtures))
	}
	if cas.SidecarFixtures[0].Name != "$SIDECAR" {
		t.Errorf("SidecarFixtures[0].Name=%q, want $SIDECAR", cas.SidecarFixtures[0].Name)
	}
	if cas.DirectoryFixtures[0].Name != "$DIR" {
		t.Errorf("DirectoryFixtures[0].Name=%q, want $DIR", cas.DirectoryFixtures[0].Name)
	}
}

// TestCassetteCloneDeepCopiesDirectoryFixtures covers the Clone deep-copy
// for the new DirectoryFixtures slice. The integration-tier mutation
// framework calls Clone before each cassette-response mutation, so a
// shared Files map would corrupt the baseline cassette across mutations.
func TestCassetteCloneDeepCopiesDirectoryFixtures(t *testing.T) {
	raw := map[string]any{
		"mode":    "reexecute",
		"runtime": map[string]any{"tool": "shell"},
		"fixtures": map[string]any{
			"$DIR": map[string]any{
				"kind": "directory",
				"files": map[string]any{
					"a.txt": "alpha",
				},
			},
		},
	}
	orig, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	clone := orig.Clone()
	if len(clone.DirectoryFixtures) != 1 {
		t.Fatalf("clone.DirectoryFixtures len=%d, want 1", len(clone.DirectoryFixtures))
	}
	clone.DirectoryFixtures[0].Files["a.txt"] = "MUTATED"
	clone.DirectoryFixtures[0].Files["new.txt"] = "added"
	if orig.DirectoryFixtures[0].Files["a.txt"] != "alpha" {
		t.Errorf("orig Files[a.txt]=%q, want alpha (clone mutation leaked)", orig.DirectoryFixtures[0].Files["a.txt"])
	}
	if _, present := orig.DirectoryFixtures[0].Files["new.txt"]; present {
		t.Errorf("orig Files[new.txt] leaked from clone")
	}
}
