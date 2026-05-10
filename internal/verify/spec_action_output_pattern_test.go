package verify

import (
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// TestMatchActionOutputPatternTypeNotOutputMatchIgnored — defensive type-
// gate: when a.Type != "output_match" the pattern fields are inert and the
// helper does not even attempt to compile or match, so nonsense Repr is
// fine.
func TestMatchActionOutputPatternTypeNotOutputMatchIgnored(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "irrelevant"}
	a := Assertion{Type: "returns", PatternAbsent: "anything", PatternPresent: "[unclosed"}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons (type-gate suppresses check), got %v", reasons)
	}
}

// TestMatchActionOutputPatternBothFieldsEmpty — both pattern fields empty
// is the no-constraint sentinel and returns nil even when type is
// output_match.
func TestMatchActionOutputPatternBothFieldsEmpty(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything goes"}
	a := Assertion{Type: "output_match"}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons when both pattern fields empty, got %v", reasons)
	}
}

// TestMatchActionOutputPatternAbsentMatches — pattern_absent set, regex
// matches got.Repr → one pattern_unexpectedly_present reason naming the
// branch.
func TestMatchActionOutputPatternAbsentMatches(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `#5 [3/4] COPY . .\n#5 CACHED [3/4] COPY . .\n`,
	}
	a := Assertion{Type: "output_match", PatternAbsent: `CACHED \[.*COPY`}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "pattern_unexpectedly_present" {
		t.Fatalf("expected pattern_unexpectedly_present, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "failed_approach") {
		t.Fatalf("expected branch name in message, got %q", reasons[0].Message)
	}
	if !strings.Contains(reasons[0].Message, `CACHED \[.*COPY`) {
		t.Fatalf("expected pattern in message, got %q", reasons[0].Message)
	}
}

// TestMatchActionOutputPatternAbsentNoMatch — pattern_absent set, regex
// does NOT match → zero reasons (the absence claim is satisfied).
func TestMatchActionOutputPatternAbsentNoMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `#5 [3/4] COPY . .\n#5 DONE 0.1s`,
	}
	a := Assertion{Type: "output_match", PatternAbsent: `CACHED \[.*COPY`}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchActionOutputPatternPresentMatches — pattern_present set, regex
// matches → zero reasons (the presence claim is satisfied).
func TestMatchActionOutputPatternPresentMatches(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `#5 CACHED [3/4] COPY . .`,
	}
	a := Assertion{Type: "output_match", PatternPresent: `CACHED \[.*COPY`}
	reasons := matchActionOutputPattern("working_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchActionOutputPatternPresentNoMatch — pattern_present set, regex
// does NOT match → one pattern_unexpectedly_absent reason naming the
// branch.
func TestMatchActionOutputPatternPresentNoMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `#5 [3/4] COPY . .\n#5 DONE 0.1s`,
	}
	a := Assertion{Type: "output_match", PatternPresent: `CACHED \[.*COPY`}
	reasons := matchActionOutputPattern("working_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "pattern_unexpectedly_absent" {
		t.Fatalf("expected pattern_unexpectedly_absent, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "working_approach") {
		t.Fatalf("expected branch name in message, got %q", reasons[0].Message)
	}
	if !strings.Contains(reasons[0].Message, `CACHED \[.*COPY`) {
		t.Fatalf("expected pattern in message, got %q", reasons[0].Message)
	}
}

// TestMatchActionOutputPatternBothFieldsGateIndependently — when both
// pattern_absent AND pattern_present are set on the same assertion, each
// gates on its own. Here the absent pattern matches (reason fires) and the
// present pattern also matches (no reason); exactly one reason from the
// absent gate.
func TestMatchActionOutputPatternBothFieldsGateIndependently(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `BANNED-TOKEN here, also CACHED`,
	}
	a := Assertion{
		Type:           "output_match",
		PatternAbsent:  `BANNED-TOKEN`,
		PatternPresent: `CACHED`,
	}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected exactly 1 reason from absent gate, got %v", reasons)
	}
	if reasons[0].Code != "pattern_unexpectedly_present" {
		t.Fatalf("expected pattern_unexpectedly_present, got %v", reasons)
	}
}

// TestMatchActionOutputPatternAbsentMalformedRegex — pattern_absent is
// invalid Go regex → one pattern_compile_failed reason naming the
// pattern_absent field.
func TestMatchActionOutputPatternAbsentMalformedRegex(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	a := Assertion{Type: "output_match", PatternAbsent: `[unclosed`}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "pattern_compile_failed" {
		t.Fatalf("expected pattern_compile_failed, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "pattern_absent") {
		t.Fatalf("expected message to name 'pattern_absent' field, got %q", reasons[0].Message)
	}
	if !strings.Contains(reasons[0].Message, "failed_approach") {
		t.Fatalf("expected branch name in message, got %q", reasons[0].Message)
	}
}

// TestMatchActionOutputPatternPresentMalformedRegex — pattern_present is
// invalid Go regex → one pattern_compile_failed reason naming the
// pattern_present field.
func TestMatchActionOutputPatternPresentMalformedRegex(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	a := Assertion{Type: "output_match", PatternPresent: `(?P<bad`}
	reasons := matchActionOutputPattern("working_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "pattern_compile_failed" {
		t.Fatalf("expected pattern_compile_failed, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "pattern_present") {
		t.Fatalf("expected message to name 'pattern_present' field, got %q", reasons[0].Message)
	}
}

// TestMatchActionOutputPatternEmptyReprAgainstPresent — empty got.Repr
// against a non-empty pattern_present surfaces pattern_unexpectedly_absent
// (an empty string can never match a non-empty literal pattern).
func TestMatchActionOutputPatternEmptyReprAgainstPresent(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: ""}
	a := Assertion{Type: "output_match", PatternPresent: `CACHED`}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "pattern_unexpectedly_absent" {
		t.Fatalf("expected pattern_unexpectedly_absent, got %v", reasons)
	}
}

// TestMatchActionOutputPatternSpecialCharsBody — got.Repr containing
// regex-meta characters (newlines, parens, brackets) still matches against
// an unanchored pattern — confirms we're not accidentally anchoring or
// escaping the haystack.
func TestMatchActionOutputPatternSpecialCharsBody(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "line one\n[bracket] (paren) {brace}\nfinal line",
	}
	a := Assertion{Type: "output_match", PatternPresent: `\(paren\)`}
	reasons := matchActionOutputPattern("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons (pattern matches in haystack), got %v", reasons)
	}
}

// TestMatchActionOutputPatternBranchArgPropagatesWorking — the branch
// name passed to the helper appears verbatim in the reason message
// (working_approach variant).
func TestMatchActionOutputPatternBranchArgPropagatesWorking(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "no match for the pattern"}
	a := Assertion{Type: "output_match", PatternPresent: `CACHED`}
	reasons := matchActionOutputPattern("working_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "working_approach") {
		t.Fatalf("expected message to contain 'working_approach', got %q", reasons[0].Message)
	}
	if strings.Contains(reasons[0].Message, "failed_approach") {
		t.Fatalf("expected message NOT to contain 'failed_approach', got %q", reasons[0].Message)
	}
}
