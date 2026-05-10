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

	// Reexecute-mode fields. Nil/empty when Mode != "reexecute". Validated
	// shape-wise here (tool present, slices well-typed); semantic validation
	// (tool ∈ supported set, setup_script exit codes) lives in the reexecute
	// orchestrator.
	Runtime        *Runtime
	SetupScript    []string
	TeardownScript []string

	// Sidecar fixtures, parsed in declaration order. Only `kind: sidecar_process`
	// entries are populated; other fixture kinds in cassette.fixtures are
	// silently skipped (the schema validates their shape, but the verifier
	// has no runtime use for them yet).
	SidecarFixtures []SidecarFixture

	// Stable ordering of step keys so error messages are deterministic.
	stepNames []string
}

// SidecarFixture is one parsed entry from cassette.fixtures with
// `kind: sidecar_process`. Other fixture kinds (sparse_file, redis_instance,
// docker_context, etc.) are not parsed by this verifier today; they are
// schema-validated at submit time but the verifier has no runtime use for
// them. Adding a typed model for another kind is a one-arm-per-kind
// extension here.
type SidecarFixture struct {
	Name              string // the $FIXTURE_* token (key in cassette.fixtures)
	Command           []string
	Against           string // optional $FIXTURE_* ref; narrative only
	ReadyStderrRegex  string // exactly one of these three is set
	ReadyPathExists   string
	ReadyDelaySeconds float64
	Enabled           bool // schema default true; we materialize the default at parse time
	StopGraceSeconds  int  // schema default 5; materialize at parse time
	ExposePID         bool
}

// Runtime describes the host CLI a reexecute-mode cassette drives. tool is
// required; version is advisory in v0.1 (logged, not enforced).
type Runtime struct {
	Tool    string
	Version string
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
		Mode:           c.Mode,
		Steps:          make(map[string]Step, len(c.Steps)),
		SetupScript:    append([]string(nil), c.SetupScript...),
		TeardownScript: append([]string(nil), c.TeardownScript...),
		stepNames:      append([]string(nil), c.stepNames...),
	}
	if c.Runtime != nil {
		rt := *c.Runtime
		out.Runtime = &rt
	}
	out.SidecarFixtures = append([]SidecarFixture(nil), c.SidecarFixtures...)
	for i := range out.SidecarFixtures {
		out.SidecarFixtures[i].Command = append([]string(nil), c.SidecarFixtures[i].Command...)
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

	// Reexecute-mode fields. Parsed unconditionally so a malformed shape on
	// a replay-mode cassette still surfaces a precise error rather than being
	// silently ignored. Replay-mode entries that don't declare these fields
	// just leave them nil/empty.
	if rawRuntime, ok := raw["runtime"]; ok {
		rt, err := parseRuntime(rawRuntime)
		if err != nil {
			return nil, err
		}
		c.Runtime = rt
	}
	if rawSetup, ok := raw["setup_script"]; ok {
		setup, err := parseScriptLines(rawSetup, "setup_script")
		if err != nil {
			return nil, err
		}
		c.SetupScript = setup
	}
	if rawTeardown, ok := raw["teardown_script"]; ok {
		teardown, err := parseScriptLines(rawTeardown, "teardown_script")
		if err != nil {
			return nil, err
		}
		c.TeardownScript = teardown
	}
	if rawFixtures, ok := raw["fixtures"]; ok {
		sf, err := parseFixtures(rawFixtures)
		if err != nil {
			return nil, err
		}
		c.SidecarFixtures = sf
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

// parseRuntime decodes the cassette.runtime block. tool is required; version
// is optional and advisory.
func parseRuntime(raw any) (*Runtime, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cassette.runtime must be a mapping (got %T)", raw)
	}
	tool, _ := m["tool"].(string)
	if tool == "" {
		return nil, errors.New("cassette.runtime.tool is required and must be a non-empty string")
	}
	version, _ := m["version"].(string)
	return &Runtime{Tool: tool, Version: version}, nil
}

// parseScriptLines decodes a setup_script / teardown_script field. Each
// element must be a non-empty string; nil/missing input yields a nil slice.
func parseScriptLines(raw any, fieldName string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("cassette.%s must be an array of strings (got %T)", fieldName, raw)
	}
	out := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("cassette.%s[%d] must be a string (got %T)", fieldName, i, item)
		}
		if s == "" {
			return nil, fmt.Errorf("cassette.%s[%d] is empty — drop the entry rather than declaring an empty command", fieldName, i)
		}
		out = append(out, s)
	}
	return out, nil
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

// parseFixtures decodes the cassette.fixtures map. The map is name-keyed
// by $FIXTURE_* tokens; only `kind: sidecar_process` entries are returned
// in declaration-order. Other kinds are skipped silently (the schema
// validates their shape; the verifier just has no runtime use for them).
//
// The map is iterated in sorted key order for determinism — yaml.v3
// loses map order when decoding through interface{}, so the seed's
// declaration order is not recoverable. Sorted-name order is the
// stable replacement; tests document this by using $FIXTURE_A,
// $FIXTURE_B if order matters.
func parseFixtures(raw any) ([]SidecarFixture, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cassette.fixtures must be a mapping (got %T)", raw)
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]SidecarFixture, 0, len(names))
	for _, name := range names {
		entry, ok := m[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cassette.fixtures.%s must be a mapping (got %T)", name, m[name])
		}
		kind, _ := entry["kind"].(string)
		if kind != "sidecar_process" {
			// schema covers the rest; runtime ignores them.
			continue
		}

		sf := SidecarFixture{
			Name:             name,
			Enabled:          true, // schema default
			StopGraceSeconds: 5,    // schema default
		}

		// Required: command (array of strings, length 1..64)
		rawCmd, ok := entry["command"].([]any)
		if !ok || len(rawCmd) == 0 {
			return nil, fmt.Errorf("cassette.fixtures.%s.command must be a non-empty array of strings", name)
		}
		sf.Command = make([]string, len(rawCmd))
		for i, c := range rawCmd {
			s, ok := c.(string)
			if !ok {
				return nil, fmt.Errorf("cassette.fixtures.%s.command[%d] must be a string (got %T)", name, i, c)
			}
			sf.Command[i] = s
		}

		// Optional: against
		if v, ok := entry["against"].(string); ok {
			sf.Against = v
		}

		// Optional: ready_when (discriminated by property name)
		if rw, ok := entry["ready_when"].(map[string]any); ok {
			switch {
			case len(rw) > 1:
				return nil, fmt.Errorf("cassette.fixtures.%s.ready_when must declare exactly one of stderr_regex / path_exists / delay_seconds", name)
			case rw["stderr_regex"] != nil:
				if s, ok := rw["stderr_regex"].(string); ok {
					sf.ReadyStderrRegex = s
				}
			case rw["path_exists"] != nil:
				if s, ok := rw["path_exists"].(string); ok {
					sf.ReadyPathExists = s
				}
			case rw["delay_seconds"] != nil:
				switch v := rw["delay_seconds"].(type) {
				case float64:
					sf.ReadyDelaySeconds = v
				case int:
					sf.ReadyDelaySeconds = float64(v)
				}
			}
		}

		// Optional: enabled (default true)
		if v, ok := entry["enabled"].(bool); ok {
			sf.Enabled = v
		}
		// Optional: stop_grace_seconds (default 5)
		if v, ok := entry["stop_grace_seconds"].(int); ok {
			sf.StopGraceSeconds = v
		}
		// Optional: expose_pid (default false)
		if v, ok := entry["expose_pid"].(bool); ok {
			sf.ExposePID = v
		}

		out = append(out, sf)
	}
	return out, nil
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
