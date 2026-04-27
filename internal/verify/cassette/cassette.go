// Package cassette parses the integration-tier cassette block from a runlog
// entry and serves the recorded HTTP exchanges via a local stub server so the
// entry's action subprocess sees a controlled environment instead of the live
// upstream. v0.1 supports replay only; reexecute (DB / compiler / FS) is a
// follow-up slice.
//
// The schema (schema/entry.schema.yaml §cassette + §cassette_step) constrains
// each step's request and response fields to strings. This package decodes
// them as a minimal HTTP wire format:
//
//	request:  "METHOD /path[?query]\nHeader: Value\n\nbody"
//	response: "STATUS\nHeader: Value\n\nbody"
//
// Matching is method + path (+ optional query) in v0.1. Header equality and
// body schema are deliberately out of scope — adding them later is a per-step
// extension that won't break existing fixtures.
//
// Query-string matching is asymmetric: a step that declares no query (e.g.
// "GET /search") matches an incoming request regardless of its query string,
// preserving the v0.1 method+path-only behavior. A step that declares a query
// (e.g. "GET /search?q=foo") enforces an exact-equivalence match — the
// incoming request must produce the same canonical form via url.Values.Encode
// (so "?a=1&b=2" matches "?b=2&a=1").
package cassette

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Cassette is the parsed form of an entry's verification.cassette block.
type Cassette struct {
	Mode  string
	Steps map[string]Step

	// Stable ordering of step keys so error messages are deterministic.
	stepNames []string
}

// Step is one recorded HTTP exchange.
type Step struct {
	Name     string
	Request  Request
	Response Response
}

// Request is the matcher for one cassette step's expected incoming request.
//
// Path holds only the URL path (no query). RawPath preserves the original
// declared "/path[?query]" string for diagnostics. HasQuery is true when the
// declared path contained a "?" — when true, Query holds the canonical
// url.Values.Encode form to compare against incoming requests.
type Request struct {
	Method   string
	Path     string
	RawPath  string
	HasQuery bool
	Query    string
	Headers  map[string]string
	Body     string
}

// Response is the canned reply for one cassette step.
type Response struct {
	Status  int
	Headers map[string]string
	Body    string
}

// StepNames returns step names in their declared order. Used for stable
// error messages.
func (c *Cassette) StepNames() []string {
	out := make([]string, len(c.stepNames))
	copy(out, c.stepNames)
	return out
}

// Clone returns a deep copy of the cassette. Used by the integration-tier
// mutation framework so each cassette-response mutation perturbs an isolated
// copy without corrupting the per-branch baseline that subsequent mutations
// re-run against. Step values are copied by value; the per-step Headers maps
// are cloned so callers can safely mutate the clone in place.
func (c *Cassette) Clone() *Cassette {
	if c == nil {
		return nil
	}
	out := &Cassette{
		Mode:      c.Mode,
		Steps:     make(map[string]Step, len(c.Steps)),
		stepNames: append([]string(nil), c.stepNames...),
	}
	for name, step := range c.Steps {
		out.Steps[name] = Step{
			Name:     step.Name,
			Request:  cloneRequest(step.Request),
			Response: cloneResponse(step.Response),
		}
	}
	return out
}

func cloneRequest(r Request) Request {
	out := r
	out.Headers = cloneHeaderMap(r.Headers)
	return out
}

func cloneResponse(r Response) Response {
	out := r
	out.Headers = cloneHeaderMap(r.Headers)
	return out
}

func cloneHeaderMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Parse reads the verification.cassette map (already YAML-decoded) into a
// Cassette. Returns errors for missing required fields or malformed step
// strings.
func Parse(raw map[string]any) (*Cassette, error) {
	if len(raw) == 0 {
		return nil, errors.New("cassette block is empty")
	}
	mode, _ := raw["mode"].(string)
	if mode == "" {
		return nil, errors.New("cassette.mode is required")
	}

	rawSteps, _ := raw["steps"].(map[string]any)
	c := &Cassette{
		Mode:  mode,
		Steps: make(map[string]Step, len(rawSteps)),
	}

	// yaml.v3 preserves insertion order for map[string]any only when the map
	// itself is decoded via yaml.Node — through interface{} it's a plain map
	// and order is lost. Sort step names alphabetically for stable error
	// reporting; the order steps are *consumed* in is driven by the per-branch
	// replay_sequence list, not by step-name order.
	names := make([]string, 0, len(rawSteps))
	for name := range rawSteps {
		names = append(names, name)
	}
	sort.Strings(names)
	c.stepNames = names

	for _, name := range names {
		entry := rawSteps[name]
		em, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cassette.steps.%s is not a mapping (got %T)", name, entry)
		}
		reqStr, _ := em["request"].(string)
		respStr, _ := em["response"].(string)
		if reqStr == "" {
			return nil, fmt.Errorf("cassette.steps.%s.request is empty", name)
		}
		if respStr == "" {
			return nil, fmt.Errorf("cassette.steps.%s.response is empty", name)
		}
		req, err := parseRequest(reqStr)
		if err != nil {
			return nil, fmt.Errorf("cassette.steps.%s.request: %w", name, err)
		}
		resp, err := parseResponse(respStr)
		if err != nil {
			return nil, fmt.Errorf("cassette.steps.%s.response: %w", name, err)
		}
		c.Steps[name] = Step{
			Name:     name,
			Request:  req,
			Response: resp,
		}
	}
	return c, nil
}

// parseRequest decodes a "METHOD /path\nHeader: Value\n\nbody" string into a
// Request. The first line is mandatory; everything else is optional.
func parseRequest(s string) (Request, error) {
	headers, body, err := parseHeadersAndBody(s)
	if err != nil {
		return Request{}, err
	}
	startLine := headers[0]
	headers = headers[1:]
	parts := strings.SplitN(startLine, " ", 2)
	if len(parts) != 2 {
		return Request{}, fmt.Errorf(
			"first line must be \"METHOD /path\", got %q", startLine)
	}
	method := strings.TrimSpace(parts[0])
	rawPath := strings.TrimSpace(parts[1])
	if method == "" || rawPath == "" {
		return Request{}, fmt.Errorf(
			"method and path must both be non-empty, got method=%q path=%q",
			method, rawPath)
	}
	hdrMap, err := headersFromLines(headers)
	if err != nil {
		return Request{}, err
	}

	// Split off an optional query string. If the declared path contains "?",
	// the query is parsed and re-encoded into canonical form so callers can
	// compare key-order-independent against the incoming request's canonical
	// query. A declared path with no "?" matches any incoming query —
	// preserving the v0.1 method+path-only semantics.
	pathOnly := rawPath
	queryStr := ""
	hasQuery := false
	if i := strings.Index(rawPath, "?"); i >= 0 {
		hasQuery = true
		pathOnly = rawPath[:i]
		q, err := url.ParseQuery(rawPath[i+1:])
		if err != nil {
			return Request{}, fmt.Errorf(
				"malformed query string in %q: %w", rawPath, err)
		}
		queryStr = q.Encode()
	}

	return Request{
		Method:   strings.ToUpper(method),
		Path:     pathOnly,
		RawPath:  rawPath,
		HasQuery: hasQuery,
		Query:    queryStr,
		Headers:  hdrMap,
		Body:     body,
	}, nil
}

// parseResponse decodes a "STATUS [text]\nHeader: Value\n\nbody" string into a
// Response. Status is mandatory; headers and body are optional.
func parseResponse(s string) (Response, error) {
	headers, body, err := parseHeadersAndBody(s)
	if err != nil {
		return Response{}, err
	}
	startLine := headers[0]
	headers = headers[1:]
	statusToken := strings.SplitN(strings.TrimSpace(startLine), " ", 2)[0]
	status, err := strconv.Atoi(statusToken)
	if err != nil {
		return Response{}, fmt.Errorf(
			"first line must start with a numeric status, got %q", startLine)
	}
	hdrMap, err := headersFromLines(headers)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Status:  status,
		Headers: hdrMap,
		Body:    body,
	}, nil
}

// parseHeadersAndBody splits a wire-format string into the start-line plus
// header lines (returned together as the first slice) and the body (everything
// after the first blank line). Returns the header lines in declaration order
// so the start line is at index 0.
func parseHeadersAndBody(s string) (lines []string, body string, err error) {
	if s == "" {
		return nil, "", errors.New("string is empty")
	}
	// Normalize CRLF → LF for tolerance.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.SplitN(s, "\n\n", 2)
	headerBlock := parts[0]
	if len(parts) == 2 {
		body = parts[1]
	}
	for _, line := range strings.Split(headerBlock, "\n") {
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, "", errors.New("no start line")
	}
	return lines, body, nil
}

// headersFromLines parses "Name: Value" lines into a canonicalized map.
// Empty input returns nil (callers tolerate a nil header map).
func headersFromLines(lines []string) (map[string]string, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(lines))
	for _, line := range lines {
		idx := strings.Index(line, ":")
		if idx < 0 {
			return nil, fmt.Errorf("malformed header line %q (expected Name: Value)", line)
		}
		name := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		out[name] = value
	}
	return out, nil
}
