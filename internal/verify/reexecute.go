package verify

// Integration tier — reexecute mode. Companion to integration.go (replay).
//
// =============================================================================
// Design summary (Phase 2 slice F23 in the verifier slice cadence)
// =============================================================================
//
// Reexecute mode covers integration entries whose state is local rather than
// network-traversed: filesystems, embedded databases, shells. Where replay
// mode rebinds $ENDPOINT to a stub HTTP server seeded from cassette.steps,
// reexecute mode allocates a per-branch tmpdir sandbox, executes the
// cassette's declared setup_script to provision it, and dispatches the
// branch's typed setup+action steps through SubprocessDriver.
//
// v0.1 of this slice supports the simplest runtime tools:
//
//   tool: shell    → step.Lang must be "shell"; bodies run via `sh -c`.
//   tool: sqlite   → step.Lang ∈ {"shell", "sql"}; sql bodies run via
//                    `sqlite3 $DB_PATH` with body on stdin.
//   tool: postgres → step.Lang ∈ {"shell", "sql"}; an ephemeral DB is
//                    provisioned per-branch via runner.ProvisionPostgresDB
//                    and exposed as $DATABASE_URL.
//   tool: redis    → step.Lang must be "shell"; an ephemeral DB number is
//                    provisioned per-branch via runner.ProvisionRedisDB and
//                    exposed as $REDIS_URL. Redis interaction is through
//                    redis-cli inside shell step bodies.
//   tool: docker   → step.Lang must be "shell"; a per-branch sandbox
//                    prefix (`runlog-verify-<8-hex>`) is provisioned via
//                    runner.ProvisionDockerSandbox and exposed as
//                    $DOCKER_PREFIX. The seed composes the prefix into its
//                    own resource names; teardown walks prefix-matched
//                    containers / images / networks / buildx builders.
//
// Other declared tools (git) surface as `runtime_unsupported`. The
// slice deliberately keeps tool-specific drivers out of scope per CLAUDE.md
// invariant #10 ("cheap and simple first") — adding a git driver is a
// one-tool follow-up.
//
// Per-branch sandbox lifecycle:
//
//   1. os.MkdirTemp("", "runlog-reexec-...")
//   2. SubprocessDriver{Tool, Workdir}.RunSetupScript(cassette.setup_script)
//   3. SubprocessDriver{Tool, Workdir}.Run(branch.setup, branch.action, inputs)
//   4. SubprocessDriver{Tool, Workdir}.RunTeardownScript(cassette.teardown_script) (best-effort)
//   5. os.RemoveAll(Workdir)
//
// Per-mutation isolation invariant: each mutation re-runs its target branch
// in a fresh tmpdir with the cassette's setup_script re-applied (mirrors
// F19's per-mutation stub re-creation). Setup-script runs use the
// per-mutation inputs so that mutate_fixture / set_literal_value
// perturbations propagate into the sandbox seed (e.g. a perturbed $TABLE
// name lands in the schema-creation SQL).
//
// Cassette-response mutations are NOT supported at reexecute tier — there's
// no HTTP response to perturb. They surface as mutation_strategy_unsupported
// so a seed author who tries to mix them gets a precise diagnostic.
//
// Deliberately out of scope for this slice (follow-ups):
//   - git runtime tool
//   - cassette.runtime.version enforcement (advisory in v0.1)
//   - regex-based stdout matching (failed_branch_must_return: {pattern: ...})
//   - reexecute-mode replay_sequence semantics (per-branch sandboxes are
//     each fully isolated; sequences would only matter if a single sandbox
//     were shared, which v0.1 doesn't do)
//
// =============================================================================

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// reexecuteSupportedTools enumerates the cassette.runtime.tool values this
// build can drive. Entries declaring a tool outside this set surface
// `runtime_unsupported` so authors know the feature is recognised
// but unimplemented (rather than malformed).
var reexecuteSupportedTools = map[string]bool{
	"shell":    true,
	"sqlite":   true,
	"postgres": true,
	"redis":    true,
	"docker":   true,
}

// postgresBaseDSN returns the connection string the verifier uses as the
// admin endpoint for CREATE DATABASE / DROP DATABASE on a postgres
// reexecute branch. Defaults to a localhost dev server when
// RUNLOG_VERIFY_PGURL is unset; the verifier deliberately follows the
// trust-the-host model (same contract as sqlite3) and does not require
// an explicit env var on a developer laptop.
func postgresBaseDSN() string {
	if v := os.Getenv("RUNLOG_VERIFY_PGURL"); v != "" {
		return v
	}
	return "postgres://localhost:5432/postgres"
}

// postgresDBNameFromDSN extracts the database name from a postgres://
// URI. Returns "" on parse failure or if the path is empty — the
// caller's stale-DB sweep prefix-check is the safety net.
func postgresDBNameFromDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// redisBaseURL returns the connection URL the verifier uses as the
// admin endpoint for FLUSHDB on a redis reexecute branch. Defaults to
// a localhost dev server when RUNLOG_VERIFY_REDISURL is unset; the
// verifier follows the same trust-the-host model as sqlite and postgres.
func redisBaseURL() string {
	if v := os.Getenv("RUNLOG_VERIFY_REDISURL"); v != "" {
		return v
	}
	return "redis://localhost:6379"
}

// redisDBNumFromURL extracts the database number from a redis:// URL's
// path component. Returns (num, true) on success, (0, false) on parse
// failure or if the path component is not a valid integer.
func redisDBNumFromURL(rawURL string) (int, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, false
	}
	path := strings.TrimPrefix(u.Path, "/")
	if path == "" {
		return 0, false
	}
	n, err := strconv.Atoi(path)
	if err != nil {
		return 0, false
	}
	return n, true
}

// provisionEphemeralResource dispatches per-branch ephemeral-resource
// provisioning by cassette.runtime.tool. Returns (inputsKey, branchURL, err)
// where inputsKey is the `$<NAME>_URL` placeholder the orchestrator binds
// into the per-branch inputs map (caller writes inputs[inputsKey] =
// branchURL on success). Tools without a server-side resource (shell, sqlite)
// return ("", "", nil) — the no-op case.
//
// This consolidates the previously duplicated postgres / redis / docker
// provisioning blocks in runReexecuteBranch into a single switch. Adding
// a new tool is one new arm here plus one new provisioner pair in runner/.
//
// Note: docker's "branchURL" is actually a sandbox-name prefix
// (`runlog-verify-<8-hex>`), not a URL — the inputsKey conventions vary
// per tool ($DATABASE_URL, $REDIS_URL, $DOCKER_PREFIX). The orchestrator
// treats this opaquely; the seed composes the value into its own
// references.
func provisionEphemeralResource(tool string) (inputsKey, branchURL string, err error) {
	switch tool {
	case "postgres":
		_, dsn, perr := runner.ProvisionPostgresDB(postgresBaseDSN())
		if perr != nil {
			return "", "", perr
		}
		return "$DATABASE_URL", dsn, nil
	case "redis":
		_, url, perr := runner.ProvisionRedisDB(redisBaseURL())
		if perr != nil {
			return "", "", perr
		}
		return "$REDIS_URL", url, nil
	case "docker":
		sandboxID, perr := runner.ProvisionDockerSandbox()
		if perr != nil {
			return "", "", perr
		}
		return "$DOCKER_PREFIX", sandboxID, nil
	}
	return "", "", nil
}

// reexecuteSupportedIsolations enumerates the verification.isolation values
// reexecute mode dispatches under. v0.1 covers `subprocess` (shell-flavored)
// and `database` (sqlite-flavored). Other isolations declared by the schema
// (compiler, docker_daemon) stay tier_unsupported until their drivers land.
var reexecuteSupportedIsolations = map[string]bool{
	"subprocess":    true,
	"database":      true,
	"docker_daemon": true,
}

// runReexecute handles tier == "integration" with cassette.mode == "reexecute".
// Mirrors runIntegration's replay path: shape checks, parse cassette, drive
// both branches through a per-branch SubprocessDriver, run mutations with
// per-mutation fresh sandboxes, sign on success.
func runReexecute(e *Entry, cas *cassette.Cassette) Result {
	res := Result{UnitID: e.UnitID, Tier: "integration"}

	if !reexecuteSupportedIsolations[e.Verification.Isolation] {
		return tierUnsupported(res, "isolation_unsupported", fmt.Sprintf(
			"reexecute-mode isolation %q is not implemented in this verifier "+
				"build — subprocess + database + docker_daemon ship first; "+
				"compiler lands in follow-up commits",
			e.Verification.Isolation,
		))
	}

	if cas.Runtime == nil {
		// Defensive: schema's allOf gate enforces this, but the CLI path has
		// no upstream JSON Schema validator, so a hand-crafted entry that
		// omits `runtime` must be caught here rather than silently degrading.
		return rejected(res, "cassette_runtime_missing",
			"cassette.mode: reexecute requires cassette.runtime.tool")
	}
	if !reexecuteSupportedTools[cas.Runtime.Tool] {
		return tierUnsupported(res, "runtime_unsupported", fmt.Sprintf(
			"cassette.runtime.tool %q is not implemented in this verifier "+
				"build — shell + sqlite + postgres + redis + docker ship first; "+
				"git lands in follow-up commits",
			cas.Runtime.Tool,
		))
	}

	// ── Branch step shape + inputs ────────────────────────────────────
	// reexecute mode skips path-extract appending: setup/action steps run as
	// shell or sql, and the captured $RESULT is stdout (already a string), so
	// dotted-key dict path extraction does not apply.
	prep, prepReason := prepareBranches(e, false)
	if prepReason != nil {
		return rejectedReasons(res, []Reason{*prepReason})
	}

	if r := validateTimeoutSeconds(e); r != nil {
		return rejectedReasons(res, []Reason{*r})
	}

	timeout := e.Verification.TimeoutSeconds

	// ── Failed branch run ─────────────────────────────────────────────
	failedRes, failedDriver, failedReason, _ := runReexecuteBranch(
		"failed_approach", cas, prep.FailedSetup, prep.FailedAction, prep.FailedInputs, timeout)
	if failedReason != nil {
		return rejectedReasons(res, []Reason{*failedReason})
	}
	defer cleanupReexecuteSandbox(failedDriver, cas, prep.FailedInputs, timeout)

	// ── Working branch run ────────────────────────────────────────────
	workingRes, workingDriver, workingReason, _ := runReexecuteBranch(
		"working_approach", cas, prep.WorkingSetup, prep.WorkingAction, prep.WorkingInputs, timeout)
	if workingReason != nil {
		return rejectedReasons(res, []Reason{*workingReason})
	}
	defer cleanupReexecuteSandbox(workingDriver, cas, prep.WorkingInputs, timeout)

	// ── Outcome matching ──────────────────────────────────────────────
	var reasons []Reason
	reasons = append(reasons, matchOutcome(branchFailed, failedRes, e.Verification.Differential)...)
	reasons = append(reasons, matchOutcome(branchWorking, workingRes, e.Verification.Differential)...)
	reasons = append(reasons, matchActionPlanNodeTiming("failed_approach", e.FailedApproach.Assertion, failedRes)...)
	reasons = append(reasons, matchActionPlanNodeTiming("working_approach", e.WorkingApproach.Assertion, workingRes)...)
	if len(reasons) > 0 {
		return rejectedReasons(res, reasons)
	}

	// ── Mutation testing ──────────────────────────────────────────────
	baseline := mutationBaseline{
		Failed: branchBaseline{
			Setup:  prep.FailedSetup,
			Action: prep.FailedAction,
			Inputs: prep.FailedInputs,
			Result: failedRes,
			Driver: failedDriver,
		},
		Working: branchBaseline{
			Setup:  prep.WorkingSetup,
			Action: prep.WorkingAction,
			Inputs: prep.WorkingInputs,
			Result: workingRes,
			Driver: workingDriver,
		},
		Diff:    e.Verification.Differential,
		Timeout: timeout,
	}

	mutReasons, supported := runReexecuteMutations(e, baseline, cas)
	if !supported {
		return tierUnsupportedReasons(res, mutReasons)
	}
	if len(mutReasons) > 0 {
		return rejectedReasons(res, mutReasons)
	}

	res.Status = "verified"
	return res
}

// runReexecuteBranch allocates a fresh tmpdir, runs the cassette's
// setup_script in it, then runs the branch's setup+action through a
// SubprocessDriver pointed at the same tmpdir. Returns the action's
// ExecResult, the driver instance (for teardown + mutation re-runs), the
// underlying runner sentinel error when relevant (nil on success and on
// verifier-internal early returns like sandbox_alloc_failed or
// sandbox_symlink_rejected), and a non-nil Reason if any pre-action step
// failed.
//
// The sandbox is *not* cleaned up here on success — the caller defers
// cleanupReexecuteSandbox after both branches succeed so that a panic
// mid-run still triggers teardown. On every error return,
// cleanupReexecuteSandbox is called immediately so the teardown sequence
// stays in one place rather than being duplicated at each error site.
func runReexecuteBranch(branchName string, cas *cassette.Cassette, setup, action []runner.Step, inputs map[string]any, timeout float64) (runner.ExecResult, runner.SubprocessDriver, *Reason, error) {
	workdir, err := os.MkdirTemp("", "runlog-reexec-")
	if err != nil {
		r := Reason{
			Code:    "sandbox_alloc_failed",
			Message: fmt.Sprintf("%s: %v", branchName, err),
		}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r, nil
	}
	driver := runner.SubprocessDriver{Tool: cas.Runtime.Tool, Workdir: workdir}

	// Database-tool branches need an ephemeral resource before setup_script can
	// run. Provisioning is per-branch (and per-mutation, since
	// runOneReexecuteMutation calls into this same function), so each run gets
	// a clean resource. Adding a third tool (docker, F71) is one new entry in
	// the switch + one new provisioner pair in runner/.
	if urlKey, branchURL, perr := provisionEphemeralResource(cas.Runtime.Tool); perr != nil {
		cleanupReexecuteSandbox(driver, cas, inputs, timeout)
		code := reexecuteRunErrorCode(perr, "runtime_provision_failed")
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, perr)}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r, perr
	} else if urlKey != "" {
		// The inputs map flows through into RunSetupScript and driver.Run; the
		// teardown path reads the resource URL back out of inputs to derive
		// the per-tool drop target (postgresDBNameFromDSN / redisDBNumFromURL).
		if inputs == nil {
			inputs = map[string]any{}
		}
		inputs[urlKey] = branchURL
	}

	if err := driver.RunSetupScript(cas.SetupScript, inputs, timeout); err != nil {
		cleanupReexecuteSandbox(driver, cas, inputs, timeout)
		code := reexecuteRunErrorCode(err, "setup_script_failed")
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, err)}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r, err
	}

	// db.sqlite Lstat guard: if the cassette's setup_script created a
	// symlink at <workdir>/db.sqlite, subsequent sqlite3 invocations would
	// follow it and write outside the sandbox. Refuse to proceed.
	if cas.Runtime.Tool == "sqlite" {
		dbPath := filepath.Join(workdir, "db.sqlite")
		if info, err := os.Lstat(dbPath); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				cleanupReexecuteSandbox(driver, cas, inputs, timeout)
				r := Reason{
					Code: "sandbox_symlink_rejected",
					Message: fmt.Sprintf("%s: db.sqlite is a symlink; refusing to follow it",
						branchName),
				}
				return runner.ExecResult{}, runner.SubprocessDriver{}, &r, nil
			}
		}
	}

	res, err := driver.Run(setup, action, inputs, timeout)
	if err != nil {
		cleanupReexecuteSandbox(driver, cas, inputs, timeout)
		code := reexecuteRunErrorCode(err, "branch_runner_error")
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, err)}
		return runner.ExecResult{}, driver, &r, err
	}
	return res, driver, nil, nil
}

// cleanupReexecuteSandbox runs the cassette's teardown_script (best-effort,
// non-zero exits ignored) and os.RemoveAll's the workdir. Safe to call with a
// zero-value SubprocessDriver — the os.RemoveAll on an empty path is a no-op.
func cleanupReexecuteSandbox(driver runner.SubprocessDriver, cas *cassette.Cassette, inputs map[string]any, timeout float64) {
	if driver.Workdir == "" {
		return
	}
	_ = driver.RunTeardownScript(cas.TeardownScript, inputs, timeout)
	_ = os.RemoveAll(driver.Workdir)

	// Ephemeral-DB cleanup per tool. Best-effort: failures only matter for
	// observability — a manual sweep can find any leaks.
	if cas.Runtime == nil {
		return
	}
	switch cas.Runtime.Tool {
	case "postgres":
		if dsn, ok := inputs["$DATABASE_URL"].(string); ok && dsn != "" {
			if dbName := postgresDBNameFromDSN(dsn); strings.HasPrefix(dbName, "runlog_verify_") {
				_ = runner.DropPostgresDB(postgresBaseDSN(), dbName)
			}
		}
	case "redis":
		if redisURL, ok := inputs["$REDIS_URL"].(string); ok && redisURL != "" {
			if dbNum, ok := redisDBNumFromURL(redisURL); ok {
				_ = runner.DropRedisDB(redisBaseURL(), dbNum)
			}
		}
	case "docker":
		if sandboxID, ok := inputs["$DOCKER_PREFIX"].(string); ok && sandboxID != "" {
			_ = runner.CleanDockerSandbox(sandboxID)
		}
	}
}

// reexecuteRunErrorCode maps a SubprocessDriver error to the Reason.Code
// reexecute mode emits. defaultCode is the fall-through used when no
// sentinel matches — varies between setup-script failure
// ("setup_script_failed") and run failure ("branch_runner_error"). The
// run-failure call site additionally surfaces ErrLanguageUnsupported and
// ErrSubprocessTool as "language_not_yet_implemented" via this same
// function — setup-script never raises those, so the extra arm is a
// no-op for the setup call but keeps the err→code map authoritative
// in one place.
func reexecuteRunErrorCode(err error, defaultCode string) string {
	switch {
	case errors.Is(err, runner.ErrInputInvalidName):
		return "cassette_input_invalid_name"
	case errors.Is(err, runner.ErrInterpreterMissing):
		return "runtime_unavailable"
	case errors.Is(err, runner.ErrTimeout):
		return "branch_timeout"
	case errors.Is(err, runner.ErrLanguageUnsupported), errors.Is(err, runner.ErrSubprocessTool):
		return "language_not_yet_implemented"
	case errors.Is(err, runner.ErrPostgresProvision):
		return "runtime_provision_failed"
	case errors.Is(err, runner.ErrRedisProvision):
		return "runtime_provision_failed"
	case errors.Is(err, runner.ErrDockerProvision):
		return "runtime_provision_failed"
	}
	return defaultCode
}

// runReexecuteMutations applies each declared mutation in a fresh per-mutation
// sandbox and verifies the outcome matches the declared expectation. Cassette-
// response mutations (mutate_cassette_response) are unsupported at reexecute
// tier — they surface as mutation_strategy_unsupported so authors don't mix
// them into a non-HTTP cassette. The shared aggregation loop lives in
// iterateMutations (mutate.go); this wrapper just closes over cas.
func runReexecuteMutations(e *Entry, b mutationBaseline, cas *cassette.Cassette) ([]Reason, bool) {
	return iterateMutations(e, func(m Mutation, i int) ([]Reason, bool) {
		return runOneReexecuteMutation(e, b, m, i, cas)
	})
}

// runOneReexecuteMutation mirrors runOneIntegrationMutation but allocates a
// fresh tmpdir + replays the cassette's setup_script per mutation re-run, so
// state from the baseline + prior mutations doesn't leak.
func runOneReexecuteMutation(e *Entry, b mutationBaseline, m Mutation, idx int, cas *cassette.Cassette) ([]Reason, bool) {
	if isCassetteResponseStrategy(m.Strategy) {
		return []Reason{{
			Code: "mutation_strategy_unsupported",
			Message: fmt.Sprintf(
				"mutation #%d strategy %q targets a cassette response, but reexecute "+
					"mode has no HTTP responses to perturb. Use mutate_fixture / "+
					"set_literal_value / swap_* / remove_kwarg / drop_flag at "+
					"reexecute tier",
				idx+1, m.Strategy),
		}}, false
	}
	strat, ok := strategies[m.Strategy]
	if !ok {
		return strategyUnsupportedReason(idx, m.Strategy), false
	}

	reasons := forEachMutationBranch(m, idx, b, func(branch branchKind, baseline branchBaseline, expected mutationOutcome) []Reason {
		mutInputs, mutAction, err := strat.apply(baseline, m)
		if err != nil {
			return []Reason{mutationTargetInvalidReason(idx, m, branch, err)}
		}

		// ── Per-mutation fresh sandbox ────────────────────────────────
		// Wrap the run + classify in a closure so the per-iteration cleanup
		// (cassette teardown_script + os.RemoveAll) runs deterministically at
		// the end of *this* mutation. A function-scope `defer` would
		// accumulate across iterations and keep N tmpdirs + teardown_script
		// processes live until the whole mutation loop returned.
		return func() []Reason {
			got, mutDriver, runReason, runErr := runReexecuteBranch(
				fmt.Sprintf("mutation #%d (%s) on %s", idx+1, m.Strategy, branch),
				cas, baseline.Setup, mutAction, mutInputs, b.Timeout)
			defer cleanupReexecuteSandbox(mutDriver, cas, mutInputs, b.Timeout)

			if runReason != nil {
				// Environmental failures (timeout, missing tool) stay as-is;
				// in-band step crashes have already been folded into the
				// ExecResult.Raised path by the driver and won't reach here.
				if runErr != nil && isEnvErr(runErr) {
					return []Reason{mutationRunnerErrorReasonMsg(runReason.Message)}
				}
				// setup_script_failed under a mutation: synthesize a raised
				// ExecResult so the outcome classifier produces outcomeFail.
				// The setup script is part of the test surface — if a mutation
				// breaks it, that's a real fail, not a runner error. Reuse the
				// shared helper so the synthesized-crash shape stays uniform
				// with replay/unit tier; the message form keeps the pre-formatted
				// "branch: err" wrapping that runReason already carries.
				got = synthesizeMutationCrashMessage(runReason.Message)
			}

			actual := classifyOutcome(branch, got, baseline.Result, b.Diff)
			if actual == expected {
				return nil
			}
			if expected == outcomeFail &&
				actual == outcomeUnchanged &&
				discriminatingStrategies[m.Strategy] &&
				!stepBodiesEqual(baseline.Action, mutAction) {
				return []Reason{mutationDidNotDiscriminateReason(idx, m, branch, "source")}
			}
			return []Reason{mutationOutcomeMismatchReason(idx, m, branch, expected, actual)}
		}()
	})
	return reasons, true
}
