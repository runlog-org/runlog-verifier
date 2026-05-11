package verify

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
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

// specKeys returns the schema-side differential keys for this branch's return
// and raise specs. Centralised here so the three tier orchestrators (unit,
// integration replay, integration reexecute) and classifyOutcome don't all
// hand-roll the "failed_branch_must_return" / "working_branch_must_raise"
// strings independently.
func (k branchKind) specKeys() (retKey, raiseKey string) {
	if k == branchFailed {
		return "failed_branch_must_return", "failed_branch_must_raise"
	}
	return "working_branch_must_return", "working_branch_must_raise"
}

// planNodeKeys returns the schema-side differential keys for this branch's
// plan-node containment specs. The single-string variant requires a substring
// match; the `_any` list variant requires any-of substring match. Both keys
// are independent and may both be present (additive semantics).
func (k branchKind) planNodeKeys() (key, anyKey string) {
	if k == branchFailed {
		return "failed_branch_must_contain_plan_node", "failed_branch_must_contain_plan_node_any"
	}
	return "working_branch_must_contain_plan_node", "working_branch_must_contain_plan_node_any"
}

// planNodeTimingKeys returns the schema-side differential keys for this
// branch's plan-node planning-time thresholds. Both keys are independent
// and may both be present (additive semantics — gt and lt can both apply
// to the same branch, e.g. "10 < planning_time < 20").
func (k branchKind) planNodeTimingKeys() (gtKey, ltKey string) {
	if k == branchFailed {
		return "failed_branch_planning_time_seconds_gt", "failed_branch_planning_time_seconds_lt"
	}
	return "working_branch_planning_time_seconds_gt", "working_branch_planning_time_seconds_lt"
}

// collectionPropertyKey returns the schema-side differential key for this
// branch's has_duplicates collection-property check. Single key (no _any
// variant — unlike plan_node) because the property is a yes/no claim about
// the captured stdout parsed as a list of lines.
func (k branchKind) collectionPropertyKey() string {
	if k == branchFailed {
		return "failed_branch_collection_has_duplicates"
	}
	return "working_branch_collection_has_duplicates"
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
//
// Workdir mirrors the SubprocessDriver's Workdir for the reexecute tier so
// fixtureActionStrategy (B21) can write into the F94-materialized fixture
// directory without type-asserting through the Driver interface. Empty
// when the baseline's driver doesn't own a workdir (PythonDriver replay
// tiers, unit tier).
type branchBaseline struct {
	Setup   []runner.Step
	Action  []runner.Step
	Inputs  map[string]any
	Result  runner.ExecResult
	Driver  runner.Driver
	Workdir string
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
// mutated inputs, setup, and action that should be re-run. The baseline is
// read-only; implementations MUST NOT mutate b.Inputs, b.Setup, or b.Action —
// they return fresh maps/slices so the caller can re-run baseline branches
// independently.
//
// B20 widened the return tuple from (inputs, action) to (inputs, setup,
// action) so source-modifying strategies (sourceSubstStrategy,
// sourceRemoveStrategy) can route a mutation at the setup steps when the
// target string explicitly names that scope (e.g.
// `working_approach.setup.dockerfile`). Strategies that don't perturb source
// (inputSubstStrategy) return b.Setup unchanged; strategies that operate on
// a single source slice mutate the targeted slice and pass the sibling
// through unchanged. See mutationTargetScope for the scope-detection rule.
type strategy interface {
	apply(b branchBaseline, m Mutation) (mutInputs map[string]any, mutSetup []runner.Step, mutAction []runner.Step, err error)
}

// mutationTargetScope reports which step slice a source-mutating strategy
// should scan and rewrite for this mutation.
//   - "setup":  target has prefix "<branch>.setup.<type>"
//   - "action": target has prefix "<branch>.action.<type>", or no prefix at
//     all (action is the conventional default scope).
//
// Used by source-mutating strategies (sourceSubstStrategy,
// sourceRemoveStrategy) to decide which step slice to scan and mutate.
//
// Per B20: canonical seeds declare drop_flag targets like
// "working_approach.setup.dockerfile" — the token to drop lives in
// the setup-step body, not the action. Returning "setup" routes the
// mutation to b.Setup; "action" preserves the historical default
// (no-prefix and action-prefixed targets continue to scan b.Action).
func mutationTargetScope(target string) string {
	if strings.HasPrefix(target, "failed_approach.setup.") ||
		strings.HasPrefix(target, "working_approach.setup.") {
		return "setup"
	}
	return "action"
}

// inputSubstStrategy covers set_literal_value and mutate_fixture: the
// mutation rebinds a $-prefixed input key to a new value.
type inputSubstStrategy struct{}

func (inputSubstStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, []runner.Step, error) {
	inputs, err := applyInputSubstitution(b.Inputs, m.Target, m.NewValue)
	if err != nil {
		return nil, nil, nil, err
	}
	return inputs, b.Setup, b.Action, nil
}

// sourceSubstStrategy covers swap_function_call and swap_identifier: the
// mutation rewrites a token in either the setup or action source via
// word-boundary substitution. Scope is decided by mutationTargetScope —
// targets like "working_approach.setup.<type>" rewrite the setup body;
// everything else continues to rewrite the action body.
type sourceSubstStrategy struct{}

func (sourceSubstStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, []runner.Step, error) {
	if mutationTargetScope(m.Target) == "setup" {
		setup, err := applySourceMutation(b.Setup, m)
		if err != nil {
			return nil, nil, nil, err
		}
		return b.Inputs, setup, b.Action, nil
	}
	action, err := applySourceMutation(b.Action, m)
	if err != nil {
		return nil, nil, nil, err
	}
	return b.Inputs, b.Setup, action, nil
}

// sourceRemoveStrategy covers remove_kwarg and drop_flag: the mutation
// strips every occurrence of a token from either the setup or action source.
// Scope is decided by mutationTargetScope — targets like
// "working_approach.setup.<type>" strip from the setup body (B20's canonical
// failure case: Dockerfile-level `--link` drop); everything else continues
// to strip from the action body.
type sourceRemoveStrategy struct{}

func (sourceRemoveStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, []runner.Step, error) {
	if mutationTargetScope(m.Target) == "setup" {
		setup, err := applyRemoveMutation(b.Setup, m)
		if err != nil {
			return nil, nil, nil, err
		}
		return b.Inputs, setup, b.Action, nil
	}
	action, err := applyRemoveMutation(b.Action, m)
	if err != nil {
		return nil, nil, nil, err
	}
	return b.Inputs, b.Setup, action, nil
}

// fixtureActionStrategy performs an in-place action against the workdir-
// materialized fixture directory between baseline and the per-mutation re-
// run. Routed by mutate_fixture when the mutation declares an "action:"
// discriminator (e.g. action: add_new_file); the input-rebind path
// remains for mutate_fixture mutations with new_value (F82's sidecar
// enabled flip).
//
// Per B21: canonical seeds like docker-buildkit-copy-link-cache declare
// mutate_fixture mutations with action: add_new_file (no new_value) — the
// semantic is "add a file to the $SOURCE_PATH directory between the
// baseline build and the per-mutation re-run", which exercises the
// cache-discrimination differential (a working-approach build invalidates
// its cache when a source file changes; a broken-approach build still
// reuses a stale cache). Implementing this in the strategy layer (not
// inputSubstStrategy) keeps each strategy's responsibility crisp.
//
// The action is performed at apply time — strat.apply runs after the
// baseline branch executed and before the per-mutation re-run, so the
// directory exists on disk (the F94 materializer wrote it before
// setup_script). The new file lands under <workdir>/<NAME>/ where NAME
// is m.Target with the leading "$" stripped, matching
// materializeDirectoryFixtures' on-disk layout.
//
// Inputs are passed through unchanged (the input still points at
// "./<NAME>"); setup and action are passed through unchanged (the
// fixture mutation operates on the on-disk tree, not the step bodies).
//
// Scope (v0.1): only mutate_fixture's action: add_new_file is supported.
// Additional actions (remove_file, flip_permission, etc.) belong to F89's
// broader generalisation of fixture-mutation strategies beyond input-rebind.
type fixtureActionStrategy struct{}

func (fixtureActionStrategy) apply(b branchBaseline, m Mutation) (map[string]any, []runner.Step, []runner.Step, error) {
	// The fixture's on-disk path under the baseline workdir. inputs[m.Target]
	// was bound to "./<NAME>" by materializeDirectoryFixtures; the
	// workdir-relative path resolves to <workdir>/<NAME>/. branchBaseline's
	// Workdir mirrors the SubprocessDriver's Workdir so we don't have to
	// type-assert through the Driver interface (and unit-tier baselines
	// that aren't subprocess-driven will surface a clear error rather than
	// silently writing into the wrong place).
	if b.Workdir == "" {
		return nil, nil, nil, fmt.Errorf("fixture mutation needs a workdir-bound baseline; got empty Workdir (strategy %s is only valid at reexecute tier with a materialized directory fixture)", m.Strategy)
	}
	if m.Target == "" {
		return nil, nil, nil, fmt.Errorf("fixture action %q needs a $-prefixed target naming the materialized fixture directory", m.ActionLegacy)
	}
	fixtureName := strings.TrimPrefix(m.Target, "$")
	fixtureDir := filepath.Join(b.Workdir, fixtureName)

	action := m.ActionLegacy
	switch action {
	case "add_new_file":
		// Filename derived from static mutation fields with sanitisation —
		// no seed-author input is interpolated raw. Collision case (two
		// add_new_file mutations against the same target within one branch)
		// is a seed-authoring pathology that will produce a deterministic
		// "file exists" overwrite; we don't try to guard against it because
		// the seed validator will catch the duplicate-mutation shape upstream.
		newName := fmt.Sprintf("runlog_%s_%s_%s.txt",
			sanitizeFixtureFilenamePart(m.Strategy),
			sanitizeFixtureFilenamePart(fixtureName),
			sanitizeFixtureFilenamePart(action))
		newPath := filepath.Join(fixtureDir, newName)
		if err := os.WriteFile(newPath, []byte("runlog mutate_fixture add_new_file\n"), 0o644); err != nil {
			return nil, nil, nil, fmt.Errorf("mutate_fixture add_new_file: write %q: %w", newPath, err)
		}
	case "":
		// No action field — caller should have routed through
		// inputSubstStrategy instead. Defensive guard so a future dispatch
		// bug surfaces clearly rather than silently writing a confused file.
		return nil, nil, nil, fmt.Errorf("fixtureActionStrategy invoked with empty action field — route mutate_fixture without action through inputSubstStrategy instead")
	default:
		return nil, nil, nil, fmt.Errorf("mutate_fixture action %q is not supported (v0.1 supports: add_new_file)", action)
	}
	return b.Inputs, b.Setup, b.Action, nil
}

// sanitizeFixtureFilenamePart rewrites a string to a portable-filename
// alphabet so derived names land safely on every host filesystem regardless
// of what the seed author wrote in m.Strategy / m.Target / m.ActionLegacy.
// Allowed characters: [A-Za-z0-9_-]; everything else becomes "_". Empty
// inputs are replaced with "x" to keep the derived name well-formed.
func sanitizeFixtureFilenamePart(s string) string {
	if s == "" {
		return "x"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
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
//
// B21: mutate_fixture has TWO valid shapes — the static registry routes the
// input-rebind shape (mutate_fixture with new_value, F82's sidecar enabled
// flip) through inputSubstStrategy. The action-discriminated shape
// (mutate_fixture with action:) routes through fixtureActionStrategy via
// resolveMutationStrategy's dynamic dispatch below; the registry stays
// single-keyed so the supported-strategies error message and the
// unsupported-strategy gate continue to read cleanly.
var strategies = map[string]strategy{
	"set_literal_value":  inputSubstStrategy{},
	"mutate_fixture":     inputSubstStrategy{},
	"swap_function_call": sourceSubstStrategy{},
	"swap_identifier":    sourceSubstStrategy{},
	"remove_kwarg":       sourceRemoveStrategy{},
	"drop_flag":          sourceRemoveStrategy{},
}

// resolveMutationStrategy returns the strategy that should handle a
// mutation, after applying any dynamic dispatch based on the mutation's
// shape. The static registry handles the common case; mutate_fixture
// with a non-empty action: field routes to fixtureActionStrategy per
// B21.
//
// Centralised so every tier's per-mutation dispatcher (unit, integration
// replay, integration reexecute share/non-share) routes identically — the
// action-discriminator dispatch must be uniform across tiers or one tier's
// canonical seeds would route to a different strategy than another's.
func resolveMutationStrategy(m Mutation) (strategy, bool) {
	if m.Strategy == "mutate_fixture" && strings.TrimSpace(m.ActionLegacy) != "" {
		return fixtureActionStrategy{}, true
	}
	strat, ok := strategies[m.Strategy]
	return strat, ok
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

// strategyUnsupportedReason builds the canonical "strategy not yet
// implemented; supported in this build: …" mutation_strategy_unsupported
// reason. The same 6-line block formerly lived inline in runOneMutation,
// runOneIntegrationMutation, and runOneReexecuteMutation; centralising it
// here means the message format and the supported-list rendering live
// together. Returned as a single-element slice for symmetry with the
// per-mutation runner contract (which always returns a []Reason).
func strategyUnsupportedReason(idx int, strategyName string) []Reason {
	return []Reason{{
		Code: "mutation_strategy_unsupported",
		Message: fmt.Sprintf(
			"mutation #%d strategy %q is not yet implemented; supported in this build: %s",
			idx+1, strategyName, supportedStrategiesMessage(),
		),
	}}
}

// mutationTargetInvalidReason builds the per-mutation "mutation_target_invalid"
// rejection raised when a strategy's apply() rejects the mutation's target/
// new_value/token shape. The same six-line `Code + Sprintf` block was
// duplicated in runOneMutation (mutate.go), runOneIntegrationMutation
// (integration.go), runOneUnitSubprocessMutation (unit.go), and
// runOneReexecuteMutation (reexecute.go); centralising it here keeps the
// per-mutation prefix ("mutation #N (strategy) on branch:") consistent
// across tiers.
func mutationTargetInvalidReason(idx int, m Mutation, branch branchKind, err error) Reason {
	return Reason{
		Code: "mutation_target_invalid",
		Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
			idx+1, m.Strategy, branch, err),
	}
}

// mutationRunnerErrorReason builds the per-mutation "mutation_runner_error"
// rejection raised when isEnvErr(err) is true (timeout / missing
// interpreter / missing host tool). The error is wrapped with the canonical
// "mutation #N (strategy) on branch: <err>" prefix matching the other
// per-mutation diagnostics. Used by runOneMutation (mutate.go) and
// runOneIntegrationMutation (integration.go), which still hold the
// underlying err. The unit-subprocess and reexecute tiers pass through a
// pre-formatted Reason.Message via mutationRunnerErrorReasonMsg below.
func mutationRunnerErrorReason(idx int, m Mutation, branch branchKind, err error) Reason {
	return Reason{
		Code: "mutation_runner_error",
		Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
			idx+1, m.Strategy, branch, err),
	}
}

// mutationRunnerErrorReasonMsg is the pre-formatted-message form of
// mutationRunnerErrorReason. The unit-subprocess and reexecute mutation
// runners surface a Reason whose Message already carries the
// "mutation #N (strategy) on branch: <err>" prefix (composed by the
// per-branch helper that allocates the sandbox); this helper just wraps
// that pre-formatted string in the canonical Reason{Code: ...} shape so
// the env-error early-return uses the same builder as the underlying-error
// form.
func mutationRunnerErrorReasonMsg(msg string) Reason {
	return Reason{Code: "mutation_runner_error", Message: msg}
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
	return iterateMutations(e, func(m Mutation, i int) ([]Reason, bool) {
		return runOneMutation(e, b, m, i)
	})
}

// iterateMutations is the shared mutation-loop scaffold used by all three
// tiers (unit/function, integration/replay, integration/reexecute). It walks
// e.Verification.Mutations, dispatches each mutation through the supplied
// per-mutation runner, accumulates Reasons in declaration order, and reports
// supported=false when any mutation's strategy is unimplemented in the build.
//
// The per-tier runOne* helpers carry the tier-specific re-run mechanics
// (PythonDriver baseline, per-mutation HTTP stub, per-mutation tmpdir
// sandbox); this loop owns only the supported-flag aggregation that was
// duplicated identically in three places.
func iterateMutations(e *Entry, runOne func(m Mutation, i int) ([]Reason, bool)) ([]Reason, bool) {
	var reasons []Reason
	supported := true
	for i, m := range e.Verification.Mutations {
		mr, ok := runOne(m, i)
		if !ok {
			supported = false
		}
		reasons = append(reasons, mr...)
	}
	return reasons, supported
}

// forEachMutationBranch walks the per-branch targets of one mutation and
// dispatches the tier-specific body via run for each runnable branch. The
// shared prologue (no-expectation guard, inapplicable skip, unknown-branch
// guard) was identical across runOneMutation, runOneIntegrationMutation, and
// runOneReexecuteMutation; centralising it here means the per-tier bodies
// only carry the tier-specific re-run mechanics.
//
// run is called with (branch, baseline, expected) once per branch that has a
// declared expectation and a registered baseline. Reasons returned by run are
// concatenated in branch order.
func forEachMutationBranch(
	m Mutation,
	idx int,
	b mutationBaseline,
	run func(branch branchKind, baseline branchBaseline, expected mutationOutcome) []Reason,
) []Reason {
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
					"mutation #%d targets %s but declares no expected_result and no expected_branch_outcome[%s]",
					idx+1, branch, branch),
			})
			continue
		}
		if inapplicable {
			continue
		}
		baseline, ok := b.byBranch(branch)
		if !ok {
			reasons = append(reasons, Reason{
				Code:    "mutation_unknown_branch",
				Message: fmt.Sprintf("mutation #%d targets unknown branch %q", idx+1, branch),
			})
			continue
		}
		reasons = append(reasons, run(branch, baseline, expected)...)
	}
	return reasons
}

// synthesizeMutationCrash converts a non-env runner error into a synthesized
// "raised exception" ExecResult so the existing outcome classifier produces
// outcomeFail. Used when the mutation re-run crashed in a way that's
// indistinguishable from the entry's claimed action raising — a remove
// mutation that produces a SyntaxError, a runtime segfault, an OOM kill —
// because the mutation-as-test framework treats those as "the mutated
// program failed" rather than "the verifier host couldn't run the test".
func synthesizeMutationCrash(err error) runner.ExecResult {
	return synthesizeMutationCrashMessage(err.Error())
}

// synthesizeMutationCrashMessage is the message-string form of
// synthesizeMutationCrash. The reexecute mutation runner already has a
// pre-formatted "branch: err" message it wants to surface verbatim instead of
// re-stringifying the underlying error; both forms produce the same shape so
// the outcome classifier path stays uniform.
func synthesizeMutationCrashMessage(msg string) runner.ExecResult {
	return runner.ExecResult{
		Raised:    true,
		Exception: "SubprocessError",
		Message:   msg,
	}
}

// mutationOutcomeMismatchReason builds the canonical "expected X, got Y"
// rejection for a mutation whose actual outcome diverged from the declared
// expected. Used by every per-tier runOneMutation* dispatcher as the default
// reason when the more-specific did_not_discriminate hint doesn't apply.
func mutationOutcomeMismatchReason(idx int, m Mutation, branch branchKind, expected, actual mutationOutcome) Reason {
	return Reason{
		Code: "mutation_outcome_mismatch",
		Message: fmt.Sprintf("mutation #%d (%s) on %s: expected %s, got %s",
			idx+1, m.Strategy, branch, expected, actual),
	}
}

// mutationDidNotDiscriminateReason builds the non-discrimination hint Reason
// for both the source-mutation and cassette-response forms. Caller is
// responsible for the gating check (expected == outcomeFail, actual ==
// outcomeUnchanged, plus per-form additional gates — see callsites). scope
// selects the message variant:
//
//	"source"           — source-mutating strategy rewrote the action but the
//	                     program's output was byte-identical to the baseline.
//	"cassette_response" — cassette-response perturbation didn't change the
//	                     action's observable behaviour (the action ignored
//	                     the perturbed field).
func mutationDidNotDiscriminateReason(idx int, m Mutation, branch branchKind, scope string) Reason {
	var body string
	switch scope {
	case "cassette_response":
		body = fmt.Sprintf(
			"perturbed cassette response (target=%q field=%s) but produced no behavioural change. "+
				"The action did not consult the perturbed response field — pick a field the action "+
				"actually reads, or assert expected_result: unchanged if this tolerance is the claim.",
			m.Target, mutationField(m))
	default: // "source"
		token, _ := resolveSwapToken(m)
		body = fmt.Sprintf(
			"rewrote source but produced no behavioural change. "+
				"The token %q was substituted in the action source but the program's observable "+
				"output was byte-identical to the baseline. Pick a token that actually discriminates "+
				"(a literal value, a function name with side effects, or a flag).",
			token)
	}
	return Reason{
		Code:    "mutation_did_not_discriminate",
		Message: fmt.Sprintf("mutation #%d (%s) on %s: %s", idx+1, m.Strategy, branch, body),
	}
}

// runOneMutation applies mutation m (1-indexed via idx) to each target
// branch, re-runs that branch, classifies the outcome, and compares to the
// declared expectation. The bool return is false when the strategy is not
// implemented in this build, signalling tier_unsupported to the caller.
func runOneMutation(e *Entry, b mutationBaseline, m Mutation, idx int) ([]Reason, bool) {
	strat, ok := resolveMutationStrategy(m)
	if !ok {
		return strategyUnsupportedReason(idx, m.Strategy), false
	}

	reasons := forEachMutationBranch(m, idx, b, func(branch branchKind, baseline branchBaseline, expected mutationOutcome) []Reason {
		mutInputs, mutSetup, mutAction, err := strat.apply(baseline, m)
		if err != nil {
			return []Reason{mutationTargetInvalidReason(idx, m, branch, err)}
		}

		// Use the per-branch driver when set (F18+F23 path); fall back to
		// PythonDriver for callers that constructed a baseline before the
		// Driver field landed. Both forms produce byte-identical behaviour
		// for the function/Python tier — fallback is purely defensive.
		drv := baseline.Driver
		if drv == nil {
			drv = runner.PythonDriver{}
		}
		got, err := drv.Run(mutSetup, mutAction, mutInputs, b.Timeout)
		if err != nil {
			if isEnvErr(err) {
				return []Reason{mutationRunnerErrorReason(idx, m, branch, err)}
			}
			// Generic subprocess crash (SyntaxError after a remove mutation,
			// runtime segfault, OOM kill). Treat as the test failing —
			// synthesize a raised ExecResult so the existing outcome
			// classifier produces outcomeFail.
			got = synthesizeMutationCrash(err)
		}

		actual := classifyOutcome(branch, got, baseline.Result, b.Diff)
		if actual == expected {
			return nil
		}
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
			return []Reason{mutationDidNotDiscriminateReason(idx, m, branch, "source")}
		}
		return []Reason{mutationOutcomeMismatchReason(idx, m, branch, expected, actual)}
	})
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
//
// The boolean passed to parseMutationOutcome distinguishes the two schema
// alphabets: expected_branch_outcome.* is the broader enum (accepts
// "assertion_does_not_match", folded into outcomeFail); expected_result is
// narrower (rejects "assertion_does_not_match") so a hand-crafted CLI-path
// entry can't smuggle that token through without an upstream JSON Schema gate.
func expectedOutcomeFor(m Mutation, k branchKind) (mutationOutcome, bool, bool) {
	if v, ok := m.ExpectedBranchOutcome[k.String()]; ok {
		return parseMutationOutcome(v, true)
	}
	if m.ExpectedResult != "" {
		return parseMutationOutcome(m.ExpectedResult, false)
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
	if reasons := matchOutcome(k, got, diff); len(reasons) > 0 {
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
