package cassette

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
)

// Stub is a local httptest.Server seeded with a per-branch replay sequence.
// Each incoming request consumes the next entry from the sequence; the cursor
// is monotonic so the request order in the test action must match the
// declared sequence exactly.
//
// One Stub serves one branch's run. After the branch run ends, callers
// inspect UnmatchedRequests and RemainingSequence to surface drift.
type Stub struct {
	server   *httptest.Server
	cassette *Cassette
	sequence []string

	mu        sync.Mutex
	cursor    int
	unmatched []UnmatchedRequest
}

// UnmatchedRequest describes one request that arrived at the stub but did not
// align with the cassette step the cursor pointed at. Used to build the
// `cassette_unmatched_request` rejection reason.
type UnmatchedRequest struct {
	Method         string
	Path           string
	ExpectedStep   string
	ExpectedMethod string
	ExpectedPath   string
}

// NewStub starts a new local HTTP server seeded with the given replay sequence.
// The server is bound to 127.0.0.1 on a random ephemeral port (httptest's
// default). Callers MUST call Close() when done.
func NewStub(c *Cassette, sequence []string) *Stub {
	s := &Stub{
		cassette: c,
		sequence: sequence,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// URL returns the base URL the action should use as $ENDPOINT.
func (s *Stub) URL() string {
	return s.server.URL
}

// Close shuts down the underlying server. Safe to call multiple times.
func (s *Stub) Close() {
	if s.server != nil {
		s.server.Close()
		s.server = nil
	}
}

// UnmatchedRequests returns the list of requests that arrived but did not
// match the cassette step the cursor pointed at. Empty when every request
// aligned with the declared sequence.
func (s *Stub) UnmatchedRequests() []UnmatchedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UnmatchedRequest, len(s.unmatched))
	copy(out, s.unmatched)
	return out
}

// RemainingSequence returns the unconsumed tail of the replay sequence — i.e.
// cassette steps the action was expected to hit but never did. Empty when the
// action consumed the whole sequence.
func (s *Stub) RemainingSequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursor >= len(s.sequence) {
		return nil
	}
	out := make([]string, len(s.sequence)-s.cursor)
	copy(out, s.sequence[s.cursor:])
	return out
}

// serve is the http.HandlerFunc the stub registers. It reads the next
// expected step from the cursor, matches the incoming request, and either
// emits the canned response or records the mismatch and serves a 599 so the
// action surfaces a real error rather than hanging.
func (s *Stub) serve(w http.ResponseWriter, r *http.Request) {
	// Drain the body to avoid leaking sockets even if we don't inspect it.
	_, _ = io.Copy(io.Discard, r.Body)
	defer r.Body.Close()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Sequence exhausted: every further request is unexpected. Record the
	// mismatch with empty expected fields so the verifier reason is precise.
	if s.cursor >= len(s.sequence) {
		s.unmatched = append(s.unmatched, UnmatchedRequest{
			Method:         r.Method,
			Path:           pathWithQuery(r),
			ExpectedStep:   "<sequence-exhausted>",
			ExpectedMethod: "",
			ExpectedPath:   "",
		})
		http.Error(w, "cassette sequence exhausted", 599)
		return
	}

	stepName := s.sequence[s.cursor]
	step, ok := s.cassette.Steps[stepName]
	if !ok {
		// Defensive — Parse + validateSequenceNames should have caught this,
		// but if it somehow slips through we want a deterministic 599 rather
		// than a panic.
		s.unmatched = append(s.unmatched, UnmatchedRequest{
			Method:         r.Method,
			Path:           pathWithQuery(r),
			ExpectedStep:   stepName,
			ExpectedMethod: "<unknown>",
			ExpectedPath:   "<unknown>",
		})
		http.Error(w, "cassette step undefined: "+stepName, 599)
		s.cursor++
		return
	}

	if !matchRequest(step.Request, r) {
		s.unmatched = append(s.unmatched, UnmatchedRequest{
			Method:         r.Method,
			Path:           pathWithQuery(r),
			ExpectedStep:   step.Name,
			ExpectedMethod: step.Request.Method,
			ExpectedPath:   expectedPathFor(step.Request),
		})
		// Still advance the cursor so a later request lining up with a later
		// step has a chance to match; the unmatched list captures the drift.
		s.cursor++
		http.Error(w, "cassette mismatch", 599)
		return
	}

	// Match: emit the canned response. Headers first, then status, then body.
	for k, v := range step.Response.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(step.Response.Status)
	if step.Response.Body != "" {
		_, _ = io.WriteString(w, step.Response.Body)
	}
	s.cursor++
}

// matchRequest compares an incoming http.Request against a cassette Request.
// v0.1 matches on method + path (+ optional query) — header / body matching
// is left for a follow-up slice.
//
// Query semantics are asymmetric: a spec with no declared query matches any
// incoming query string; a spec that declares a query enforces canonical
// equivalence (url.Values.Encode form) so key order is irrelevant.
func matchRequest(spec Request, r *http.Request) bool {
	if !pathsMatch(spec.Path, r.URL.Path) {
		return false
	}
	if spec.Method != "" && spec.Method != r.Method {
		return false
	}
	if spec.HasQuery && !queriesMatch(spec.Query, r.URL.RawQuery) {
		return false
	}
	return true
}

// pathsMatch compares the cassette path against the request path. Both are
// stripped of trailing slashes (idempotent treatment of "/foo" and "/foo/")
// before comparison.
func pathsMatch(spec, got string) bool {
	// Cassette path may include a leading "/", a trailing "/", or neither.
	// Normalize both sides.
	return trimSlash(spec) == trimSlash(got)
}

// queriesMatch compares a cassette spec's already-canonical query (produced
// at parse time via url.Values.Encode) against the incoming request's raw
// query, which is canonicalized the same way before comparison so key order
// and percent-encoding differences don't cause false negatives.
func queriesMatch(specCanonical, gotRaw string) bool {
	got, err := url.ParseQuery(gotRaw)
	if err != nil {
		// Malformed incoming query — fail closed so the unmatched diagnostic
		// surfaces rather than silently passing.
		return false
	}
	return specCanonical == got.Encode()
}

func trimSlash(p string) string {
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}

// pathWithQuery renders the path including the raw query string, used in
// unmatched-request error reports.
func pathWithQuery(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + r.URL.RawQuery
}

// expectedPathFor renders a cassette Request's path for unmatched-request
// diagnostics: includes the declared query string when one was present so
// the operator sees the full declared form.
func expectedPathFor(spec Request) string {
	if spec.RawPath != "" {
		return spec.RawPath
	}
	return spec.Path
}
