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

	"github.com/runlog/verifier/internal/verify/cassette"
	"github.com/runlog/verifier/internal/verify/runner"
)

// isEnvErr reports whether err is an environmental failure (timeout, missing
// interpreter) that should surface as `mutation_runner_error` rather than be
// reclassified as outcomeFail. Mirrors the discrimination in runOneMutation.
func isEnvErr(err error) bool {
	return errors.Is(err, runner.ErrTimeout) || errors.Is(err, runner.ErrInterpreterMissing)
}

// runIntegration handles tier == "integration" with isolation: http_client.
// Mirrors runUnit's structure: shape checks, parse cassette, spin up stub
// server, drive both branches through it, run mutations, sign on success.
func runIntegration(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "integration"}

	if e.Verification.Isolation != "http_client" {
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "isolation_not_yet_implemented",
			Message: fmt.Sprintf(
				"integration isolation %q is not implemented in this verifier "+
					"build — integration tier ships with isolation: http_client first; "+
					"database / docker_daemon / subprocess land in follow-up commits",
				e.Verification.Isolation,
			),
		}}
		return res
	}

	// ── Cassette parse ────────────────────────────────────────────────
	cas, err := cassette.Parse(e.Verification.Cassette)
	if err != nil {
		return rejected(res, "cassette_malformed", err.Error())
	}
	if cas.Mode == "reexecute" {
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "cassette_mode_not_yet_implemented",
			Message: "cassette.mode: reexecute is not implemented in this " +
				"verifier build — replay lands first; reexecute (DB / compiler / " +
				"filesystem isolations) follows in a later slice",
		}}
		return res
	}
	if cas.Mode != "replay" {
		return rejected(res, "cassette_mode_invalid",
			fmt.Sprintf("cassette.mode must be replay or reexecute, got %q", cas.Mode))
	}

	// ── Branch step shape ────────────────────────────────────────────
	failedSetup, err := stepsFromAny(e.FailedApproach.Setup)
	if err != nil {
		return rejected(res, "malformed_failed_setup", err.Error())
	}
	failedAction, err := stepsFromAny(e.FailedApproach.Action)
	if err != nil {
		return rejected(res, "malformed_failed_action", err.Error())
	}
	workingSetup, err := stepsFromAny(e.WorkingApproach.Setup)
	if err != nil {
		return rejected(res, "malformed_working_setup", err.Error())
	}
	workingAction, err := stepsFromAny(e.WorkingApproach.Action)
	if err != nil {
		return rejected(res, "malformed_working_action", err.Error())
	}

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

	// Path extractor (reused from unit tier — same semantics).
	failedPath, err := returnPathFromDifferential(e.Verification.Differential, "failed_branch_must_return")
	if err != nil {
		return rejected(res, "malformed_return_path", err.Error())
	}
	workingPath, err := returnPathFromDifferential(e.Verification.Differential, "working_branch_must_return")
	if err != nil {
		return rejected(res, "malformed_return_path", err.Error())
	}
	if failedPath != "" {
		failedAction = append(failedAction, pathExtractStep(failedPath))
	}
	if workingPath != "" {
		workingAction = append(workingAction, pathExtractStep(workingPath))
	}

	failedInputs, workingInputs, err := splitInputs(e.Verification.Differential)
	if err != nil {
		return rejected(res, "malformed_inputs", err.Error())
	}
	failedInputs = mergeLiterals(e.Literals, failedInputs)
	workingInputs = mergeLiterals(e.Literals, workingInputs)

	timeout := e.Verification.TimeoutSeconds

	// ── Failed branch run (if a sequence is declared) ─────────────────
	var failedRes runner.ExecResult
	failedHasRun := false
	if len(failedSeq) > 0 {
		stub := cassette.NewStub(cas, failedSeq)
		defer stub.Close()
		failedInputs = withEndpoint(failedInputs, stub.URL())

		failedRes, err = runner.RunPython(failedSetup, failedAction, failedInputs, timeout)
		if err != nil {
			return runnerError(res, "failed_approach", err)
		}
		if reasons := stubReasons("failed_approach", stub); len(reasons) > 0 {
			res.Status = "rejected"
			res.Reasons = reasons
			return res
		}
		failedHasRun = true
	}

	// ── Working branch run (if a sequence is declared) ────────────────
	var workingRes runner.ExecResult
	workingHasRun := false
	if len(workingSeq) > 0 {
		stub := cassette.NewStub(cas, workingSeq)
		defer stub.Close()
		workingInputs = withEndpoint(workingInputs, stub.URL())

		workingRes, err = runner.RunPython(workingSetup, workingAction, workingInputs, timeout)
		if err != nil {
			return runnerError(res, "working_approach", err)
		}
		if reasons := stubReasons("working_approach", stub); len(reasons) > 0 {
			res.Status = "rejected"
			res.Reasons = reasons
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
		reasons = append(reasons, matchOutcome("failed_approach", failedRes,
			e.Verification.Differential, "failed_branch_must_return", "failed_branch_must_raise")...)
	}
	if workingHasRun {
		reasons = append(reasons, matchOutcome("working_approach", workingRes,
			e.Verification.Differential, "working_branch_must_return", "working_branch_must_raise")...)
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
		},
		Working: branchBaseline{
			Setup:  workingSetup,
			Action: workingAction,
			Inputs: workingInputs,
			Result: workingRes,
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

// runMutationsWithCtx is the integration-tier wrapper around runMutations.
// It intercepts the per-branch baseline at apply-time to replace inputs with a
// stub-aware copy, runs the mutation, and tears down the stub. The mutation
// framework itself (mutate.go) is unchanged.
func runMutationsWithCtx(e *Entry, b mutationBaseline, ctx integrationMutationCtx) ([]Reason, bool) {
	var reasons []Reason
	supported := true

	for i, m := range e.Verification.Mutations {
		mr, ok := runOneIntegrationMutation(e, b, m, i, ctx)
		if !ok {
			supported = false
		}
		reasons = append(reasons, mr...)
	}
	return reasons, supported
}

// runOneIntegrationMutation mirrors runOneMutation but re-runs the mutated
// branch through a fresh stub server so each mutation sees a clean cassette
// cursor. The strategy.apply call still returns mutated inputs/action; we
// rebind $ENDPOINT before handing them to the runner.
func runOneIntegrationMutation(e *Entry, b mutationBaseline, m Mutation, idx int, ctx integrationMutationCtx) ([]Reason, bool) {
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

		mutInputs, mutAction, err := strat.apply(baseline, m)
		if err != nil {
			reasons = append(reasons, Reason{
				Code: "mutation_target_invalid",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
					idx+1, m.Strategy, branch, err),
			})
			continue
		}

		stub := cassette.NewStub(ctx.cassette, seq)
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
			reasons = append(reasons, Reason{
				Code: "mutation_outcome_mismatch",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: expected %s, got %s",
					idx+1, m.Strategy, branch, expected, actual),
			})
		}
	}
	return reasons, true
}
