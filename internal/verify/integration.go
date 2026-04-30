package verify

// Integration tier — replay-only cassette runner.
//
// =============================================================================
// Design summary (Phase 2 slice (b), F18 in the verifier slice cadence)
// =============================================================================
//
// The integration tier verifies entries whose action makes HTTP/RPC calls
// against an external service. v0.1 of this slice is **replay only**: the
// verifier stands up a local HTTP stub (net/http/httptest), seeds it from
// the entry's declared cassette steps, and rebinds the entry's $ENDPOINT
// placeholder to the stub's URL so the existing Python runner — unchanged
// — drives traffic through the stub.
//
// What "replay" buys us in v0.1:
//   - Real Python action source (no fake HTTP client injection on the Python
//     side); the entry's code is exercised exactly as a consuming agent
//     would write it.
//   - Stdlib-only on the verifier side (net/http/httptest).
//   - Reuses every existing piece of infrastructure: differential branches,
//     literals merge, python_expr, mutation strategies, signed bundle out.
//
// Cassette declaration shape (schema/entry.schema.yaml §cassette + §cassette_step):
//   cassette:
//     mode: replay
//     artifact: <name>.cassette.yaml         # advisory in v0.1 (inline only)
//     steps:
//       step-name:
//         request:  "METHOD /path\nHeader: Value\n\nbody"
//         response: "STATUS\nHeader: Value\n\nbody"
//     captures: [<prose>]
//     strips:   [<prose>]
//     replay_targets: [<prose>]
//
// The schema constrains request/response to strings, so the wire format
// above is the smallest readable encoding that round-trips through YAML.
// The first line of `request` is "METHOD /path"; subsequent lines until
// the first blank line are "Header: Value"; everything after the blank
// line is the body. `response` mirrors that with a numeric status as the
// first token.
//
// Branch-to-cassette wiring (per existing seed shapes):
//   differential:
//     failed_approach_replay_sequence:  [step-a, step-b]
//     working_approach_replay_sequence: [step-a, step-c, step-a]
//
// The verifier serves cassette steps in the listed order for each branch.
// Each request the action makes consumes the next entry from the active
// branch's sequence; mismatches surface as `cassette_unmatched_request`.
//
// How the action gets pointed at the stub:
//   The verifier injects the stub URL as `$ENDPOINT` in the per-branch
//   inputs map. Entries should write their action as
//       resp = httpx.get(f"{$ENDPOINT}/v1/charges")
//   The verifier substitutes $ENDPOINT="http://127.0.0.1:PORT" before the
//   Python subprocess starts. This is cleaner than env-based proxying and
//   matches the placeholder convention already used by the GitHub seed.
//
// Mutation handling for v0.1:
//   The unit-tier mutation framework (set_literal_value, mutate_fixture,
//   swap_function_call, swap_identifier, remove_kwarg, drop_flag) all
//   apply unchanged at integration tier — they rewrite inputs or action
//   source, both orthogonal to the stub. Each mutation re-runs the
//   targeted branch through a fresh stub server with the same cassette
//   wiring so the cursor state doesn't leak across runs. Cassette-side
//   mutations (perturb a response byte) are **deliberately deferred** to
//   the next slice.
//
// Deliberately out of scope for this slice (follow-ups):
//   - mode: reexecute (DB / compiler / filesystem isolations)
//   - On-disk signed cassette artifacts (today the cassette is inline YAML)
//   - TLS termination at the stub (HTTPS clients in actions must accept
//     http:// URLs via $ENDPOINT for now)
//   - Cassette-response mutation testing (perturb response body / headers)
//   - Header / body matching beyond method+path (matching is method+path
//     only; future slices add header equality, body schema, query params)
//   - Time model simulation (cassette.time_model is parsed but ignored)
//
// =============================================================================

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// isEnvErr reports whether err is an environmental failure (timeout, missing
// interpreter) that should surface as `mutation_runner_error` rather than be
// reclassified as outcomeFail. Mirrors the discrimination in runOneMutation.
func isEnvErr(err error) bool {
	return errors.Is(err, runner.ErrTimeout) || errors.Is(err, runner.ErrInterpreterMissing)
}

// runIntegration handles tier == "integration". Cassette mode dispatches:
//
//	mode: replay    → this function (HTTP stub + per-branch sequence)
//	mode: reexecute → runReexecute in reexecute.go (per-branch tmpdir sandbox)
//
// Each mode validates its own isolation set: replay accepts only `http_client`;
// reexecute accepts `subprocess` + `database`. Mismatched (mode, isolation)
// pairs surface as `isolation_not_yet_implemented` in the mode-specific
// runner — they're not malformed per se (each value is in the schema enum),
// just pointed at the wrong runtime path.
func runIntegration(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "integration"}

	// ── Cassette parse ────────────────────────────────────────────────
	cas, err := cassette.Parse(e.Verification.Cassette)
	if err != nil {
		return rejected(res, "cassette_malformed", err.Error())
	}
	if cas.Mode == "reexecute" {
		return runReexecute(e, cas)
	}
	if cas.Mode != "replay" {
		return rejected(res, "cassette_mode_invalid",
			fmt.Sprintf("cassette.mode must be replay or reexecute, got %q", cas.Mode))
	}

	// Replay mode only accepts http_client. Reexecute-mode isolations
	// (subprocess, database) routed via runReexecute above never reach here;
	// other isolations (compiler, docker_daemon) are tier_unsupported until
	// their drivers land regardless of mode.
	if e.Verification.Isolation != "http_client" {
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "isolation_not_yet_implemented",
			Message: fmt.Sprintf(
				"replay-mode integration isolation %q is not implemented — "+
					"replay supports http_client only; subprocess + database "+
					"under cassette.mode: reexecute; compiler / docker_daemon "+
					"land in follow-up commits",
				e.Verification.Isolation,
			),
		}}
		return res
	}

	// ── Branch step shape + path extractor + inputs ──────────────────
	prep, prepReason := prepareBranches(e, true)
	if prepReason != nil {
		res.Status = "rejected"
		res.Reasons = []Reason{*prepReason}
		return res
	}
	failedSetup, failedAction := prep.FailedSetup, prep.FailedAction
	workingSetup, workingAction := prep.WorkingSetup, prep.WorkingAction
	failedInputs, workingInputs := prep.FailedInputs, prep.WorkingInputs

	// ── Replay sequences (per docs/03 §5.4 and existing seed shape) ──
	failedSeq, err := stringList(e.Verification.Differential, "failed_approach_replay_sequence")
	if err != nil {
		return rejected(res, "malformed_replay_sequence", err.Error())
	}
	workingSeq, err := stringList(e.Verification.Differential, "working_approach_replay_sequence")
	if err != nil {
		return rejected(res, "malformed_replay_sequence", err.Error())
	}
	if len(failedSeq) == 0 && len(workingSeq) == 0 {
		return rejected(res, "missing_replay_sequence",
			"integration cassette requires at least one of "+
				"failed_approach_replay_sequence / working_approach_replay_sequence")
	}
	if reasons := validateSequenceNames(cas, failedSeq, "failed_approach_replay_sequence"); len(reasons) > 0 {
		res.Status = "rejected"
		res.Reasons = reasons
		return res
	}
	if reasons := validateSequenceNames(cas, workingSeq, "working_approach_replay_sequence"); len(reasons) > 0 {
		res.Status = "rejected"
		res.Reasons = reasons
		return res
	}

	timeout := e.Verification.TimeoutSeconds

	// ── Failed branch run (if a sequence is declared) ─────────────────
	//
	// Each branch run uses runStubbedBranch so the per-branch stub server is
	// closed immediately after the branch returns. A function-scope `defer
	// stub.Close()` would keep the failed-branch stub (port + handler
	// goroutine) alive across the working-branch run and the entire mutation
	// loop — a real resource leak in the long-running case.
	var failedRes runner.ExecResult
	failedHasRun := false
	if len(failedSeq) > 0 {
		var (
			runErr error
			rsns   []Reason
		)
		failedRes, runErr, rsns, failedInputs = runStubbedBranch(
			"failed_approach", cas, failedSeq, failedSetup, failedAction, failedInputs, timeout)
		if runErr != nil {
			return runnerError(res, "failed_approach", runErr)
		}
		if len(rsns) > 0 {
			res.Status = "rejected"
			res.Reasons = rsns
			return res
		}
		failedHasRun = true
	}

	// ── Working branch run (if a sequence is declared) ────────────────
	var workingRes runner.ExecResult
	workingHasRun := false
	if len(workingSeq) > 0 {
		var (
			runErr error
			rsns   []Reason
		)
		workingRes, runErr, rsns, workingInputs = runStubbedBranch(
			"working_approach", cas, workingSeq, workingSetup, workingAction, workingInputs, timeout)
		if runErr != nil {
			return runnerError(res, "working_approach", runErr)
		}
		if len(rsns) > 0 {
			res.Status = "rejected"
			res.Reasons = rsns
			return res
		}
		workingHasRun = true
	}

	// ── Cassette-step coverage: every declared step must be hit ───────
	if reasons := unusedStepReasons(cas, failedSeq, workingSeq); len(reasons) > 0 {
		res.Status = "rejected"
		res.Reasons = reasons
		return res
	}

	// ── Outcome matching ──────────────────────────────────────────────
	var reasons []Reason
	if failedHasRun {
		reasons = append(reasons, matchOutcome(branchFailed, failedRes, e.Verification.Differential)...)
	}
	if workingHasRun {
		reasons = append(reasons, matchOutcome(branchWorking, workingRes, e.Verification.Differential)...)
	}
	if len(reasons) > 0 {
		res.Status = "rejected"
		res.Reasons = reasons
		return res
	}

	// ── Mutation testing ──────────────────────────────────────────────
	//
	// Mutations re-use the unit-tier baseline shape but each mutation re-runs
	// against a fresh stub server (so the cursor state from the baseline run
	// doesn't leak). We hand the mutation framework an integration-aware
	// per-branch baseline; the stub/endpoint substitution is wrapped by
	// integrationStrategy below.
	baseline := mutationBaseline{
		Failed: branchBaseline{
			Setup:  failedSetup,
			Action: failedAction,
			Inputs: failedInputs,
			Result: failedRes,
			Driver: runner.PythonDriver{},
		},
		Working: branchBaseline{
			Setup:  workingSetup,
			Action: workingAction,
			Inputs: workingInputs,
			Result: workingRes,
			Driver: runner.PythonDriver{},
		},
		Diff:    e.Verification.Differential,
		Timeout: timeout,
	}

	mutCtx := integrationMutationCtx{
		cassette:   cas,
		failedSeq:  failedSeq,
		workingSeq: workingSeq,
	}
	mutReasons, supported := runMutationsWithCtx(e, baseline, mutCtx)
	if !supported {
		res.Status = "tier_unsupported"
		res.Reasons = mutReasons
		return res
	}
	if len(mutReasons) > 0 {
		res.Status = "rejected"
		res.Reasons = mutReasons
		return res
	}

	res.Status = "verified"
	return res
}

// runStubbedBranch creates a per-branch HTTP stub, rebinds $ENDPOINT to the
// stub's URL on a fresh inputs copy, runs the branch through the Python
// driver, and closes the stub before returning. The stub's lifetime is bounded
// by this function so the goroutine + listener do not outlive the branch run
// (a function-scope defer in the caller would keep them alive across
// subsequent branches and the mutation loop).
//
// Returns the exec result, an environmental error (timeout / missing
// interpreter — surfaced via runnerError by the caller), any cassette-shape
// rejection reasons (unmatched request / sequence underrun), and the
// $ENDPOINT-bound inputs map for re-use by the mutation loop.
func runStubbedBranch(
	branchName string,
	cas *cassette.Cassette,
	seq []string,
	setup, action []runner.Step,
	inputs map[string]any,
	timeout float64,
) (runner.ExecResult, error, []Reason, map[string]any) {
	stub := cassette.NewStub(cas, seq)
	defer stub.Close()
	inputs = withEndpoint(inputs, stub.URL())

	res, err := runner.RunPython(setup, action, inputs, timeout)
	if err != nil {
		return runner.ExecResult{}, err, nil, inputs
	}
	if reasons := stubReasons(branchName, stub); len(reasons) > 0 {
		return res, nil, reasons, inputs
	}
	return res, nil, nil, inputs
}

// withEndpoint returns a fresh inputs map with $ENDPOINT bound to url. Existing
// $ENDPOINT in inputs is overwritten — the verifier's stub URL takes precedence
// over any literal/declared endpoint so the action is forced through the stub.
func withEndpoint(inputs map[string]any, url string) map[string]any {
	out := make(map[string]any, len(inputs)+1)
	for k, v := range inputs {
		out[k] = v
	}
	out["$ENDPOINT"] = url
	return out
}

// stringList extracts a []string from the differential map at the given key.
// Missing key returns (nil, nil); wrong shape returns an error so submitters
// see a precise rejection reason.
func stringList(diff map[string]any, key string) ([]string, error) {
	raw, ok := diff[key]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("differential.%s must be a list, got %T", key, raw)
	}
	out := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("differential.%s[%d] must be a string, got %T", key, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}

// validateSequenceNames checks that every step name referenced in a replay
// sequence has a matching definition in cassette.steps. Returns a single
// rejection reason naming the first unknown step (further unknowns are listed
// in the same message so submitters fix the whole batch in one pass).
func validateSequenceNames(c *cassette.Cassette, seq []string, fieldName string) []Reason {
	var unknown []string
	for _, name := range seq {
		if _, ok := c.Steps[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return []Reason{{
		Code: "cassette_step_unknown",
		Message: fmt.Sprintf(
			"differential.%s references undeclared cassette step(s) %v; "+
				"declared steps: %v",
			fieldName, unknown, c.StepNames()),
	}}
}

// stubReasons inspects a stub's post-run state and emits one or more rejection
// reasons covering: an unmatched live request, a sequence shorter than the
// requests issued, or a sequence longer than the requests issued.
func stubReasons(branch string, stub *cassette.Stub) []Reason {
	var out []Reason
	for _, req := range stub.UnmatchedRequests() {
		out = append(out, Reason{
			Code: "cassette_unmatched_request",
			Message: fmt.Sprintf(
				"%s: action made request %s %s but the next cassette step %q expects %s %s",
				branch, req.Method, req.Path, req.ExpectedStep, req.ExpectedMethod, req.ExpectedPath),
		})
	}
	if leftover := stub.RemainingSequence(); len(leftover) > 0 {
		out = append(out, Reason{
			Code: "cassette_sequence_underrun",
			Message: fmt.Sprintf(
				"%s: replay sequence has %d unconsumed step(s) — action stopped before reaching %v",
				branch, len(leftover), leftover),
		})
	}
	return out
}

// unusedStepReasons reports cassette steps that were declared but never
// referenced by any replay sequence. Per docs/03 §5.3 spirit (no dead
// fixtures), a cassette step that no branch consumes is dead weight and
// must be flagged so submitters either remove it or wire it into a branch.
func unusedStepReasons(c *cassette.Cassette, sequences ...[]string) []Reason {
	used := make(map[string]bool, len(c.Steps))
	for _, seq := range sequences {
		for _, name := range seq {
			used[name] = true
		}
	}
	var unused []string
	for _, name := range c.StepNames() {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	if len(unused) == 0 {
		return nil
	}
	return []Reason{{
		Code: "cassette_unused",
		Message: fmt.Sprintf(
			"cassette step(s) %v are declared but never referenced by any "+
				"replay_sequence — remove them or add to a branch's sequence",
			unused),
	}}
}

// integrationMutationCtx threads cassette + per-branch sequences through the
// mutation framework so each mutation re-run gets a fresh stub server seeded
// from the original cassette declaration.
type integrationMutationCtx struct {
	cassette   *cassette.Cassette
	failedSeq  []string
	workingSeq []string
}

// runMutationsWithCtx is the integration-tier wrapper that threads the
// per-branch cassette + replay sequences through the shared mutation loop. The
// dispatcher itself lives in iterateMutations (mutate.go); this wrapper only
// closes over ctx so the tier-specific per-mutation runner can see it.
func runMutationsWithCtx(e *Entry, b mutationBaseline, ctx integrationMutationCtx) ([]Reason, bool) {
	return iterateMutations(e, func(m Mutation, i int) ([]Reason, bool) {
		return runOneIntegrationMutation(e, b, m, i, ctx)
	})
}

// runOneIntegrationMutation mirrors runOneMutation but re-runs the mutated
// branch through a fresh stub server so each mutation sees a clean cassette
// cursor. The strategy dispatcher splits two cases:
//
//   - input/source mutators (set_literal_value, mutate_fixture, swap_*,
//     remove_kwarg, drop_flag): rewrite inputs or action source, then re-run
//     against a fresh stub seeded from the un-mutated cassette baseline.
//   - cassette-response mutators (mutate_cassette_response): leave inputs
//     and action untouched, perturb a clone of the cassette, then re-run
//     against a fresh stub seeded from the perturbed clone.
//
// Per-mutation cassette cloning is required so mutation N doesn't corrupt
// mutation N+1's baseline — see cassette.Cassette.Clone.
func runOneIntegrationMutation(e *Entry, b mutationBaseline, m Mutation, idx int, ctx integrationMutationCtx) ([]Reason, bool) {
	cassetteResponse := isCassetteResponseStrategy(m.Strategy)

	if !cassetteResponse {
		if _, ok := strategies[m.Strategy]; !ok {
			return []Reason{{
				Code: "mutation_strategy_unsupported",
				Message: fmt.Sprintf(
					"mutation #%d strategy %q is not yet implemented; supported in this build: %s",
					idx+1, m.Strategy, supportedStrategiesMessage(),
				),
			}}, false
		}
	}

	var reasons []Reason

	for _, branch := range branchesFor(m) {
		expected, inapplicable, hasExp := expectedOutcomeFor(m, branch)
		if !hasExp {
			reasons = append(reasons, Reason{
				Code: "mutation_no_expectation",
				Message: fmt.Sprintf(
					"mutation #%d targets %s but declares no expected_result and no expected_branch_outcome.%s",
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

		var seq []string
		switch branch {
		case branchFailed:
			seq = ctx.failedSeq
		case branchWorking:
			seq = ctx.workingSeq
		}
		if len(seq) == 0 {
			// No replay sequence for this branch → no stub required, but
			// mutation expects a re-run. Skip with a clear reason rather than
			// silently passing.
			reasons = append(reasons, Reason{
				Code: "mutation_no_branch_sequence",
				Message: fmt.Sprintf(
					"mutation #%d targets %s but no %s_replay_sequence is declared",
					idx+1, branch, branch),
			})
			continue
		}

		// ── Per-mutation stub setup ───────────────────────────────────
		// Cassette-response mutations clone + perturb the cassette before
		// the stub is created; everything else uses the un-mutated baseline.
		var mutInputs map[string]any
		var mutAction []runner.Step
		stubCassette := ctx.cassette

		if cassetteResponse {
			// Cassette-response: inputs and action are untouched; cassette
			// is cloned + perturbed.
			mutInputs = baseline.Inputs
			mutAction = baseline.Action

			perturbed, err := applyCassetteResponseMutation(ctx.cassette, m, seq)
			if err != nil {
				reasons = append(reasons, Reason{
					Code:    err.code,
					Message: fmt.Sprintf("mutation #%d (%s) on %s: %v", idx+1, m.Strategy, branch, err.msg),
				})
				continue
			}
			stubCassette = perturbed
		} else {
			strat := strategies[m.Strategy]
			var err error
			mutInputs, mutAction, err = strat.apply(baseline, m)
			if err != nil {
				reasons = append(reasons, Reason{
					Code: "mutation_target_invalid",
					Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
						idx+1, m.Strategy, branch, err),
				})
				continue
			}
		}

		stub := cassette.NewStub(stubCassette, seq)
		mutInputs = withEndpoint(mutInputs, stub.URL())

		got, err := runner.RunPython(baseline.Setup, mutAction, mutInputs, b.Timeout)
		stub.Close()

		if err != nil {
			// Same crash-as-fail logic as runOneMutation: timeout/missing
			// interpreter are environmental; everything else is a real fail.
			if isEnvErr(err) {
				reasons = append(reasons, Reason{
					Code: "mutation_runner_error",
					Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
						idx+1, m.Strategy, branch, err),
				})
				continue
			}
			got = runner.ExecResult{
				Raised:    true,
				Exception: "SubprocessError",
				Message:   err.Error(),
			}
		}

		actual := classifyOutcome(branch, got, baseline.Result, b.Diff)
		if actual != expected {
			// F22: mirror the F21 hint for cassette-response mutations.
			// A perturbed cassette that produces no behavioural change is
			// integration-tier theatre — the action ignored the response
			// field the submitter perturbed.
			if cassetteResponse &&
				expected == outcomeFail &&
				actual == outcomeUnchanged {
				reasons = append(reasons, Reason{
					Code: "mutation_did_not_discriminate",
					Message: fmt.Sprintf(
						"mutation #%d (%s) on %s: perturbed cassette response (target=%q field=%s) but produced no behavioural change. "+
							"The action did not consult the perturbed response field — pick a field the action actually reads, "+
							"or assert expected_result: unchanged if this tolerance is the claim.",
						idx+1, m.Strategy, branch, m.Target, mutationField(m)),
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

// cassetteMutationError pairs a typed reason code with a human-readable
// message. The cassette-response mutator surfaces several distinct failure
// modes (unknown step ID, unparseable target, invalid field, malformed
// new_value, malformed status) — distinguishing them at the reason level
// gives seed authors a precise diagnostic.
type cassetteMutationError struct {
	code string
	msg  string
}

func (e *cassetteMutationError) Error() string { return e.msg }

func newCassetteMutationError(code, msg string) *cassetteMutationError {
	return &cassetteMutationError{code: code, msg: msg}
}

// applyCassetteResponseMutation clones the cassette and perturbs the response
// declared by the mutation's target step. Returns a new *cassette.Cassette;
// the input is not modified.
//
// Mutation shape:
//
//	strategy: mutate_cassette_response
//	target:   <step_id>          # references a key in cassette.steps
//	field:    body | status | header.<NAME>   # carried in m.Action (see
//	                                            mutationField below)
//	new_value: <replacement>     # string for body/header.*; int|string for status
//	branch:   failed_approach | working_approach   # required (cassette
//	                                                   responses are per-branch
//	                                                   in the v0.1 wire shape)
//
// The Mutation struct carries `field` in the existing `action` slot —
// reusing the slot rather than expanding the wire shape keeps the
// schema delta to one line. mutationField below is the canonical
// extractor.
//
// Validation:
//   - target must name a declared cassette step, AND that step must appear in
//     the targeted branch's replay sequence (else the perturbation is dead
//     weight).
//   - field must be body, status, or header.<NAME>.
//   - new_value type must match the field: string for body/header.*, int or
//     numeric string for status.
func applyCassetteResponseMutation(c *cassette.Cassette, m Mutation, branchSeq []string) (*cassette.Cassette, *cassetteMutationError) {
	stepID := strings.TrimSpace(m.Target)
	if stepID == "" {
		return nil, newCassetteMutationError("mutation_target_invalid",
			"mutate_cassette_response requires target: <cassette_step_id>")
	}

	step, ok := c.Steps[stepID]
	if !ok {
		return nil, newCassetteMutationError("mutation_step_id_unknown", fmt.Sprintf(
			"target %q does not name any declared cassette step (declared: %v)",
			stepID, c.StepNames()))
	}

	// Belt-and-braces: the targeted branch must actually consume this step,
	// else the perturbation is unobservable and the mutation can't possibly
	// discriminate. Surface as step_id_unknown with a more precise message
	// rather than letting the run silently produce an `unchanged` outcome.
	consumed := false
	for _, name := range branchSeq {
		if name == stepID {
			consumed = true
			break
		}
	}
	if !consumed {
		return nil, newCassetteMutationError("mutation_step_id_unknown", fmt.Sprintf(
			"target %q is declared but the targeted branch's replay sequence %v does not consume it — "+
				"the perturbation would be unobservable",
			stepID, branchSeq))
	}

	field := mutationField(m)
	if field == "" {
		return nil, newCassetteMutationError("mutation_field_invalid",
			"mutate_cassette_response requires field: body | status | header.<NAME> "+
				"(carried in the mutation's `action` field)")
	}

	clone := c.Clone()
	target := clone.Steps[stepID]

	switch {
	case field == "body":
		newBody, err := stringNewValue(m.NewValue)
		if err != nil {
			return nil, newCassetteMutationError("mutation_target_invalid", err.Error())
		}
		target.Response.Body = newBody

	case field == "status":
		newStatus, err := statusNewValue(m.NewValue)
		if err != nil {
			return nil, newCassetteMutationError("mutation_target_invalid", err.Error())
		}
		target.Response.Status = newStatus

	case strings.HasPrefix(field, "header."):
		hdrName := strings.TrimPrefix(field, "header.")
		if hdrName == "" {
			return nil, newCassetteMutationError("mutation_field_invalid",
				"field header. requires a header name (e.g. header.Retry-After)")
		}
		newVal, err := stringNewValue(m.NewValue)
		if err != nil {
			return nil, newCassetteMutationError("mutation_target_invalid", err.Error())
		}
		if target.Response.Headers == nil {
			target.Response.Headers = make(map[string]string, 1)
		}
		target.Response.Headers[hdrName] = newVal

	default:
		return nil, newCassetteMutationError("mutation_field_invalid", fmt.Sprintf(
			"unsupported field %q — supported: body, status, header.<NAME>", field))
	}

	// Step is a value type — the clone-then-mutate pattern requires re-storing.
	// Preserve the step Name so downstream diagnostics still reference the
	// original key.
	step.Response = target.Response
	clone.Steps[stepID] = step
	return clone, nil
}

// mutationField extracts the cassette-response mutation's field selector. The
// wire shape carries it in the existing `action` slot (Mutation.Action) so the
// schema delta is just a one-line strategy enum addition. Whitespace is
// trimmed; an empty result means the submitter didn't declare a field.
func mutationField(m Mutation) string {
	return strings.TrimSpace(m.Action)
}

// stringNewValue coerces m.NewValue into a string for body / header
// perturbations. yaml.v3 may decode numeric / boolean / nil literals into
// the matching Go types; we accept any of those as long as a stable string
// rendering exists.
func stringNewValue(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case []byte:
		return string(val), nil
	case bool, int, int64, float64:
		return fmt.Sprintf("%v", val), nil
	case nil:
		return "", nil
	}
	return "", fmt.Errorf("new_value must be a string for body / header.* perturbations, got %T", v)
}

// statusNewValue coerces m.NewValue into an HTTP status code. Accepts
// numeric YAML scalars (200) and numeric strings ("200") for symmetry with
// the rest of the verifier's loose type acceptance.
func statusNewValue(v any) (int, error) {
	switch val := v.(type) {
	case int:
		return val, nil
	case int64:
		return int(val), nil
	case float64:
		// yaml.v3 decodes integer scalars as int when small; this path
		// catches the rare big-integer / float case.
		return int(val), nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil {
			return 0, fmt.Errorf("new_value for status must be a numeric HTTP status, got %q", val)
		}
		return n, nil
	}
	return 0, fmt.Errorf("new_value for status must be an integer (e.g. 500) or numeric string, got %T", v)
}
