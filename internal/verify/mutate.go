package verify

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/runlog/verifier/internal/verify/runner"
)

// branchKind enumerates which branch a mutation targets. Stringly-typed
// "failed_approach" / "working_approach" / "both" / "" remain the wire
// representation in YAML, but inside the verify package we route on a typed
// enum so callers can't accidentally mistype the constant.
type branchKind int

const (
	branchUnknown branchKind = iota
	branchFailed
	branchWorking
)

// String renders a branchKind back to the schema-side YAML key. matchOutcome
// and the schema's expected_branch_outcome map keys remain string-valued, so
// the verifier round-trips through the wire form at the boundary.
func (k branchKind) String() string {
	switch k {
	case branchFailed:
		return "failed_approach"
	case branchWorking:
		return "working_approach"
	}
	return "unknown"
}

// parseBranchKind maps a schema-side branch key to its enum. ok is false for
// unknown values (including ""), letting callers degrade gracefully.
func parseBranchKind(s string) (branchKind, bool) {
	switch s {
	case "failed_approach":
		return branchFailed, true
	case "working_approach":
		return branchWorking, true
	}
	return branchUnknown, false
}

// branchBaseline captures one branch's un-mutated execution context so a
// mutation can be applied as a delta against it.
//
// Driver is the runner used to re-run this branch under a mutation. For
// unit-tier and integration-tier replay it's runner.PythonDriver{} (the
// driver is stateless, so the same instance serves baseline + mutations).
// For integration-tier reexecute, the orchestrator constructs a per-branch
// SubprocessDriver pointing at the branch's tmpdir; the reexecute mutation
// runner uses the *baseline* Driver only as a fallback identity — it
// constructs a fresh per-mutation Driver pointing at a fresh per-mutation
// tmpdir for actual mutation re-runs (per-mutation sandbox isolation
// invariant, mirrors F19's per-mutation stub).
type branchBaseline struct {
	Setup  []runner.Step
	Action []runner.Step
	Inputs map[string]any
	Result runner.ExecResult
	Driver runner.Driver
}

// mutationBaseline pairs the per-branch baselines for a unit/function run with
// the differential spec and the per-run timeout. runOneMutation pulls the
// per-branch slice via byBranch.
type mutationBaseline struct {
	Failed  branchBaseline
	Working branchBaseline
	Diff    map[string]any
	Timeout float64
}

// byBranch returns the per-branch baseline for k. ok is false for unknown
// kinds, mirroring the previous selectBranch helper.
func (b mutationBaseline) byBranch(k branchKind) (branchBaseline, bool) {
	switch k {
	case branchFailed:
		return b.Failed, true
	case branchWorking:
		return b.Working, true
	}
	return branchBaseline{}, false
}

// mutationOutcome is the classified result of applying a mutation to one
// branch. Mirrors the schema's expected_result enum (minus inapplicable,
// which is surfaced separately as a skip signal).
type mutationOutcome string

const (
	outcomeFail      mutationOutcome = "fail"
	outcomePass      mutationOutcome = "pass"
	outcomeUnchanged mutationOutcome = "unchanged"
)

// strategy applies one mutation to a per-branch baseline and returns the
// mutated inputs and action that should be re-run. The baseline is read-only;
// implementations MUST NOT mutate b.Inputs or b.Action — they return fresh
// maps/slices so the caller can re-run baseline branches independently.
type strategy interface {
	apply(b branchBaseline, m Mutation) (mutInputs map[string]any, mutAction []runner.Step, err error)
}

// inputSubstStrategy covers set_literal_value and mutate_fixture: the
// mutation rebinds a $-prefixed input key to a new value.
type inputSubstStrategy struct{}

func (inputSubstStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, error) {
	inputs, err := applyInputSubstitution(b.Inputs, m.Target, m.NewValue)
	if err != nil {
		return nil, nil, err
	}
	return inputs, b.Action, nil
}

// sourceSubstStrategy covers swap_function_call and swap_identifier: the
// mutation rewrites a token in the action source via word-boundary
// substitution.
type sourceSubstStrategy struct{}

func (sourceSubstStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, error) {
	action, err := applySourceMutation(b.Action, m)
	if err != nil {
		return nil, nil, err
	}
	return b.Inputs, action, nil
}

// sourceRemoveStrategy covers remove_kwarg and drop_flag: the mutation
// strips every occurrence of a token from the action source.
type sourceRemoveStrategy struct{}

func (sourceRemoveStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, error) {
	action, err := applyRemoveMutation(b.Action, m)
	if err != nil {
		return nil, nil, err
	}
	return b.Inputs, action, nil
}

// strategies is the single source of truth for which strategy names this
// verifier slice supports. Adding a new strategy is a one-line registry edit
// plus one apply method.
//
// Note: cassette-response mutations (mutate_cassette_response, F22) are
// dispatched separately by the integration tier — they don't fit the
// (inputs, action) -> (inputs, action) shape this strategy interface assumes.
// They live in cassetteResponseStrategies below so the registry isn't lying
// about supporting them at unit tier.
var strategies = map[string]strategy{
	"set_literal_value":  inputSubstStrategy{},
	"mutate_fixture":     inputSubstStrategy{},
	"swap_function_call": sourceSubstStrategy{},
	"swap_identifier":    sourceSubstStrategy{},
	"remove_kwarg":       sourceRemoveStrategy{},
	"drop_flag":          sourceRemoveStrategy{},
}

// cassetteResponseStrategies enumerates strategy names that perturb the
// cassette before re-running the branch through a fresh stub. These are only
// valid at integration tier — at unit tier they surface as
// mutation_strategy_unsupported, which is the right error (no cassette exists
// to perturb).
var cassetteResponseStrategies = map[string]bool{
	"mutate_cassette_response": true,
}

// isCassetteResponseStrategy reports whether the named strategy mutates the
// cassette response rather than the action source / inputs. Exported within
// the package for the integration-tier dispatcher.
func isCassetteResponseStrategy(name string) bool {
	return cassetteResponseStrategies[name]
}

// discriminatingStrategies enumerates the strategies whose `unchanged`
// outcome under expected_result: fail signals a tautological / theatrical
// test rather than a real submitter bug. The non-discrimination diagnostic
// (mutation_did_not_discriminate) fires for these — for input-substitution
// strategies, an `unchanged` outcome is a different problem (the new value
// happened to behave identically, not a non-discriminating perturbation).
//
// Source-mutating strategies discriminate by rewriting the action source.
// Cassette-response mutations discriminate by perturbing the upstream's
// reply: if the action's matcher actually consults the response, perturbing
// it must change behaviour; an `unchanged` outcome means the action ignored
// the response field — exactly the integration-tier theatre this strategy
// was added to kill.
var discriminatingStrategies = map[string]bool{
	"swap_function_call":       true,
	"swap_identifier":          true,
	"remove_kwarg":             true,
	"drop_flag":                true,
	"mutate_cassette_response": true,
}

// stepBodiesEqual returns true when two step slices have byte-identical
// bodies in the same order. Used to detect whether a source-mutating
// strategy actually rewrote anything before classifying the outcome.
func stepBodiesEqual(a, b []runner.Step) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Body != b[i].Body {
			return false
		}
	}
	return true
}

// supportedStrategiesMessage renders the sorted list of registered strategy
// names for inclusion in the mutation_strategy_unsupported Reason — keeps the
// message stable and self-updating when new strategies land.
func supportedStrategiesMessage() string {
	names := make([]string, 0, len(strategies))
	for name := range strategies {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// runMutations applies each declared mutation and verifies the actual outcome
// matches the declared expectation. Returns (reasons, supported).
//
//   - supported=false → at least one mutation strategy is not yet implemented;
//     caller must surface tier_unsupported. reasons will name the strategy.
//   - supported=true, len(reasons)==0 → all mutations matched their expectations.
//   - supported=true, len(reasons)>0 → at least one mutation produced an
//     unexpected outcome; caller must surface rejected.
func runMutations(e *Entry, b mutationBaseline) ([]Reason, bool) {
	var reasons []Reason
	supported := true

	for i, m := range e.Verification.Mutations {
		mr, ok := runOneMutation(e, b, m, i)
		if !ok {
			supported = false
		}
		reasons = append(reasons, mr...)
	}
	return reasons, supported
}

// runOneMutation applies mutation m (1-indexed via idx) to each target
// branch, re-runs that branch, classifies the outcome, and compares to the
// declared expectation. The bool return is false when the strategy is not
// implemented in this build, signalling tier_unsupported to the caller.
func runOneMutation(e *Entry, b mutationBaseline, m Mutation, idx int) ([]Reason, bool) {
	strat, ok := strategies[m.Strategy]
	if !ok {
		return []Reason{{
			Code: "mutation_strategy_unsupported",
			Message: fmt.Sprintf(
				"mutation #%d strategy %q is not yet implemented; supported in this build: %s",
				idx+1, m.Strategy, supportedStrategiesMessage(),
			),
		}}, false
	}

	var reasons []Reason

	for _, branch := range branchesFor(m) {
		expected, inapplicable, hasExp := expectedOutcomeFor(m, branch)
		if !hasExp {
			// CLI-path defensive check: the schema's oneOf gate enforces
			// "at least one of expected_result / expected_branch_outcome",
			// but not the per-branch case where expected_branch_outcome is
			// declared but the targeted branch is missing from the map.
			// Surface that as a real authoring bug rather than a silent skip
			// (which would let the entry verify green without ever running
			// the mutation against the targeted branch).
			reasons = append(reasons, Reason{
				Code: "mutation_no_expectation",
				Message: fmt.Sprintf(
					"mutation #%d targets %s but declares no expected_result and no expected_branch_outcome.%s",
					idx+1, branch, branch,
				),
			})
			continue
		}
		if inapplicable {
			continue
		}

		baseline, ok := b.byBranch(branch)
		if !ok {
			reasons = append(reasons, Reason{
				Code: "mutation_unknown_branch",
				Message: fmt.Sprintf("mutation #%d targets unknown branch %q",
					idx+1, branch),
			})
			continue
		}

		mutInputs, mutAction, err := strat.apply(baseline, m)
		if err != nil {
			reasons = append(reasons, Reason{
				Code: "mutation_target_invalid",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
					idx+1, m.Strategy, branch, err),
			})
			continue
		}

		// Use the per-branch driver when set (F18+F23 path); fall back to
		// PythonDriver for callers that constructed a baseline before the
		// Driver field landed. Both forms produce byte-identical behaviour
		// for the function/Python tier — fallback is purely defensive.
		drv := baseline.Driver
		if drv == nil {
			drv = runner.PythonDriver{}
		}
		got, err := drv.Run(baseline.Setup, mutAction, mutInputs, b.Timeout)
		if err != nil {
			if errors.Is(err, runner.ErrTimeout) || errors.Is(err, runner.ErrInterpreterMissing) {
				reasons = append(reasons, Reason{
					Code: "mutation_runner_error",
					Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
						idx+1, m.Strategy, branch, err),
				})
				continue
			}
			// Generic subprocess crash (SyntaxError after a remove mutation,
			// runtime segfault, OOM kill). Treat as the test failing —
			// synthesize a raised ExecResult so the existing outcome
			// classifier produces outcomeFail.
			got = runner.ExecResult{
				Raised:    true,
				Exception: "SubprocessError",
				Message:   err.Error(),
			}
		}

		actual := classifyOutcome(branch, got, baseline.Result, b.Diff)
		if actual != expected {
			// Diagnostic refinement: when a source-mutating strategy
			// rewrote the action (so the regex / strings.ReplaceAll did
			// match) but the program's observable behaviour was
			// identical, the submitter picked a token that doesn't
			// actually discriminate — typically a local identifier
			// that's renamed consistently throughout, or a call that
			// has the same return value as the swap target. Replace
			// the generic mutation_outcome_mismatch reason with a
			// targeted hint so the seed author knows what to fix.
			if expected == outcomeFail &&
				actual == outcomeUnchanged &&
				discriminatingStrategies[m.Strategy] &&
				!stepBodiesEqual(baseline.Action, mutAction) {
				token, _ := resolveSwapToken(m)
				reasons = append(reasons, Reason{
					Code: "mutation_did_not_discriminate",
					Message: fmt.Sprintf(
						"mutation #%d (%s) on %s: rewrote source but produced no behavioural change. "+
							"The token %q was substituted in the action source but the program's observable "+
							"output was byte-identical to the baseline. Pick a token that actually discriminates "+
							"(a literal value, a function name with side effects, or a flag).",
						idx+1, m.Strategy, branch, token),
				})
				continue
			}
			reasons = append(reasons, Reason{
				Code: "mutation_outcome_mismatch",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: expected %s, got %s",
					idx+1, m.Strategy, branch, expected, actual),
			})
		}
	}
	return reasons, true
}

// branchesFor returns the branch list a mutation should be applied to. An
// explicit "both" expands to both branches; an explicit single branch is
// returned as-is; an empty branch defaults to working_approach (matches the
// implicit-discrimination rule in checkMutationDiscriminating).
func branchesFor(m Mutation) []branchKind {
	switch m.Branch {
	case "both":
		return []branchKind{branchFailed, branchWorking}
	case "failed_approach":
		return []branchKind{branchFailed}
	case "working_approach":
		return []branchKind{branchWorking}
	default:
		return []branchKind{branchWorking}
	}
}

// expectedOutcomeFor resolves the expected outcome for one branch. Returns
// (expected, isInapplicable, hasExpectation). expected_branch_outcome takes
// precedence over expected_result when both are set on a single mutation.
func expectedOutcomeFor(m Mutation, k branchKind) (mutationOutcome, bool, bool) {
	if v, ok := m.ExpectedBranchOutcome[k.String()]; ok {
		return canonicalizeBranchOutcome(v)
	}
	if m.ExpectedResult != "" {
		return canonicalizeResult(m.ExpectedResult)
	}
	return "", false, false
}

// parseMutationOutcome maps a schema outcome string to the internal enum.
// When allowAssertionMismatch is true (expected_branch_outcome path) the
// broader alphabet that includes "assertion_does_not_match" is accepted and
// folded into outcomeFail. When false (expected_result path) that token is
// rejected — the CLI path lacks an upstream JSON Schema gate, so a
// hand-crafted entry with expected_result: assertion_does_not_match must be
// caught here rather than silently accepted.
// Returns (outcome, isInapplicable, ok).
func parseMutationOutcome(s string, allowAssertionMismatch bool) (mutationOutcome, bool, bool) {
	switch s {
	case "assertion_does_not_match":
		if !allowAssertionMismatch {
			return "", false, false
		}
		return outcomeFail, false, true
	case "fail":
		return outcomeFail, false, true
	case "pass":
		return outcomePass, false, true
	case "unchanged":
		return outcomeUnchanged, false, true
	case "inapplicable":
		return "", true, true
	}
	return "", false, false
}

// canonicalizeBranchOutcome maps the schema's expected_branch_outcome.* enum
// (the broader of the two — accepts assertion_does_not_match) to the internal
// outcome enum.
func canonicalizeBranchOutcome(s string) (mutationOutcome, bool, bool) {
	return parseMutationOutcome(s, true)
}

// canonicalizeResult maps the schema's expected_result enum (narrower — does
// NOT accept assertion_does_not_match) to the internal outcome enum.
func canonicalizeResult(s string) (mutationOutcome, bool, bool) {
	return parseMutationOutcome(s, false)
}

// classifyOutcome compares a re-run ExecResult against the un-mutated baseline
// and the differential spec for the same branch. Returns:
//   - outcomeUnchanged when the re-run is byte-equivalent to the baseline.
//   - outcomeFail when the re-run violates the branch's spec (raised when
//     should return, returned when should raise, wrong type/value/exception).
//   - outcomePass otherwise (re-run still satisfies the spec but produced a
//     different concrete value than the baseline).
func classifyOutcome(k branchKind, got, baseline runner.ExecResult, diff map[string]any) mutationOutcome {
	if execResultsEqual(got, baseline) {
		return outcomeUnchanged
	}
	var retKey, raiseKey string
	if k == branchFailed {
		retKey, raiseKey = "failed_branch_must_return", "failed_branch_must_raise"
	} else {
		retKey, raiseKey = "working_branch_must_return", "working_branch_must_raise"
	}
	if reasons := matchOutcome(k.String(), got, diff, retKey, raiseKey); len(reasons) > 0 {
		return outcomeFail
	}
	return outcomePass
}

// execResultsEqual returns true when two ExecResults are observationally
// identical at the granularity the verifier compares against — both must be
// raised-or-not, exception fields equal if raised, type+canonical-JSON or
// repr equal if returned.
func execResultsEqual(a, b runner.ExecResult) bool {
	if a.Raised != b.Raised {
		return false
	}
	if a.Raised {
		return a.Exception == b.Exception && a.Message == b.Message
	}
	if a.TypeName != b.TypeName || a.Serializable != b.Serializable {
		return false
	}
	if !a.Serializable {
		return a.Repr == b.Repr
	}
	aBytes, errA := canonicalizeJSON(a.JSONValue)
	bBytes, errB := canonicalizeJSON(b.JSONValue)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(aBytes, bBytes)
}

// normalizeInputTarget reduces target paths down to the bare $TOKEN.
// Accepts shapes seen in seeds: "$LITERAL_2", "$ITEMS",
// "differential.inputs.$PAYLOAD", "failed_approach.$INPUT".
// Returns the bare $TOKEN, or the original string if no $-prefixed token is
// found.
func normalizeInputTarget(target string) string {
	if i := strings.LastIndex(target, "$"); i >= 0 {
		return target[i:]
	}
	return target
}

// applyInputSubstitution returns a new inputs map with the placeholder set
// to newValue. The map is copied so concurrent re-runs see independent
// inputs. Both bare ($KEY) and stripped-prefix (KEY) are checked; whichever
// exists in inputs is overwritten. If neither exists, the bare $-prefixed
// key is added so the runner's TrimPrefix logic can still bind it.
//
// F17: newValue may be the {python_expr: "<expr>"} opt-in shape introduced
// by F12. yaml.v3 decodes nested maps into map[string]any, which is exactly
// the type the runner's pythonExprFromValue checks — so no conversion is
// needed here. The value is stored as-is and the runner evaluates the
// expression in Python instead of JSON-decoding it as a literal string.
func applyInputSubstitution(inputs map[string]any, target string, newValue any) (map[string]any, error) {
	if target == "" {
		return nil, errors.New("mutation target is empty")
	}
	key := normalizeInputTarget(target)
	if !strings.HasPrefix(key, "$") {
		return nil, fmt.Errorf("input target must include a $-prefixed placeholder, got %q", target)
	}
	out := make(map[string]any, len(inputs)+1)
	for k, v := range inputs {
		out[k] = v
	}
	bare := strings.TrimPrefix(key, "$")
	switch {
	case mapHas(out, key):
		out[key] = newValue
	case mapHas(out, bare):
		out[bare] = newValue
	default:
		out[key] = newValue
	}
	return out, nil
}

func mapHas(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// applySourceMutation rewrites the target branch's action source according
// to a swap_function_call or swap_identifier mutation. Both strategies share
// the same mechanic: word-boundary substitution of a token with new_value
// across every step's Body. Returns a fresh []runner.Step; the input is not
// mutated.
//
// Token resolution: m.Token if non-empty; else m.Target if it does not start
// with a branch-path prefix (`failed_approach.` / `working_approach.`); else
// error. new_value must be a string.
//
// Zero-match guard: the `\b` word-boundary metacharacter only anchors between
// a word character ([A-Za-z0-9_]) and a non-word character. Tokens whose
// endpoints are non-word characters (e.g. "== 204") compile fine but never
// match — the rewrite silently no-ops and the mutation classifies as
// `unchanged`. To prevent that silent failure, count actual substitutions and
// return an error if the token did not match anywhere in any step body. The
// caller surfaces this as `mutation_target_invalid`, naming the offending
// token so the seed author can pick a discriminating one.
func applySourceMutation(steps []runner.Step, m Mutation) ([]runner.Step, error) {
	token, err := resolveSwapToken(m)
	if err != nil {
		return nil, err
	}
	replacement, ok := m.NewValue.(string)
	if !ok {
		return nil, fmt.Errorf("new_value must be a string for %s, got %T", m.Strategy, m.NewValue)
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(token) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("compile token regex %q: %w", token, err)
	}
	out := make([]runner.Step, len(steps))
	matches := 0
	for i, s := range steps {
		matches += len(re.FindAllStringIndex(s.Body, -1))
		out[i] = runner.Step{
			Type: s.Type,
			Lang: s.Lang,
			Body: re.ReplaceAllString(s.Body, replacement),
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf(
			"token %q did not match anywhere in the action source — "+
				"note that \\b word boundaries only anchor on [A-Za-z0-9_]; "+
				"a token with leading/trailing non-word characters (e.g. operators, spaces) will never match. "+
				"Pick a bare identifier or function name", token)
	}
	return out, nil
}

// applyRemoveMutation removes every occurrence of the resolved token from
// the target branch's action source. Both remove_kwarg and drop_flag share
// the same mechanic — v0.1 is intentionally not syntax-aware about commas
// or whitespace cleanup. Submitters pick tokens that produce the intended
// post-removal source; tokens that leave invalid syntax cause the subprocess
// to crash, which the runner-error classifier in runOneMutation surfaces as
// outcomeFail (the mutation broke the test, which is what expected_result:
// fail is asserting).
//
// Token resolution reuses resolveSwapToken (m.Token if non-empty; else
// m.Target when not branch-path-prefixed; else error).
//
// Zero-match guard: like applySourceMutation, count substitutions and surface
// a typed error if the token never appears in the action source. Without this
// the strings.ReplaceAll call silently no-ops and the mutation classifies as
// `unchanged`, hiding the typo from the seed author.
func applyRemoveMutation(steps []runner.Step, m Mutation) ([]runner.Step, error) {
	token, err := resolveSwapToken(m)
	if err != nil {
		return nil, err
	}
	matches := 0
	out := make([]runner.Step, len(steps))
	for i, s := range steps {
		matches += strings.Count(s.Body, token)
		out[i] = runner.Step{
			Type: s.Type,
			Lang: s.Lang,
			Body: strings.ReplaceAll(s.Body, token, ""),
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf(
			"token %q did not appear anywhere in the action source — "+
				"check for typos or punctuation/whitespace mismatches (the verifier matches verbatim)",
			token)
	}
	return out, nil
}

// resolveSwapToken implements the swap-mutation token resolution rule:
//  1. If m.Token is non-empty, use it.
//  2. Else if m.Target does not start with a branch-path prefix, use it.
//  3. Else error — the mutation has no usable find-pattern.
func resolveSwapToken(m Mutation) (string, error) {
	if m.Token != "" {
		return m.Token, nil
	}
	if m.Target == "" {
		return "", fmt.Errorf("%s needs a token field or a non-empty target", m.Strategy)
	}
	if strings.HasPrefix(m.Target, "failed_approach.") || strings.HasPrefix(m.Target, "working_approach.") {
		return "", fmt.Errorf("%s needs a token field or a non-path target — got %q with empty token", m.Strategy, m.Target)
	}
	return m.Target, nil
}
