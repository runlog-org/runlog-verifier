package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// matchBranchOutcome is the all-in-one per-branch outcome check used by every
// tier orchestrator (unit, unit-tier subprocess, integration replay, integration
// reexecute). Composes the three independent per-branch outcome checks —
// differential matching (matchOutcome), action-level planning-time
// thresholds (matchActionPlanNodeTiming), and action-level output-pattern
// assertions (matchActionOutputPattern) — into a single call so each tier's
// orchestrator only loops over branches once.
//
// Pre-F95 the three calls + per-branch append shape was duplicated across
// runUnit / runIntegration / runReexecute / runUnitSubprocess as six near-
// identical lines (three calls × two branches). Centralising here also
// guarantees the call order stays uniform — important for stable Reason
// ordering when an entry violates multiple per-branch checks at once.
func matchBranchOutcome(k branchKind, got runner.ExecResult, a Assertion, diff map[string]any) []Reason {
	branch := k.String()
	var out []Reason
	out = append(out, matchOutcome(k, got, diff)...)
	out = append(out, matchActionPlanNodeTiming(branch, a, got)...)
	out = append(out, matchActionOutputPattern(branch, a, got)...)
	return out
}

// matchOutcome compares one branch's ExecResult against the relevant
// differential keys. Returns the (possibly empty) list of mismatch reasons.
//
// The per-branch return/raise spec keys are derived from k.specKeys() so the
// schema-side strings ("failed_branch_must_return", …) live in exactly one
// place — formerly duplicated across runUnit, runIntegration, runReexecute,
// and classifyOutcome.
func matchOutcome(k branchKind, got runner.ExecResult, diff map[string]any) []Reason {
	branch := k.String()
	retKey, raiseKey := k.specKeys()
	planKey, planAnyKey := k.planNodeKeys()
	retSpec, hasRet := diff[retKey]
	raiseSpec, hasRaise := diff[raiseKey]
	planSpec, hasPlan := diff[planKey]
	planAnySpec, hasPlanAny := diff[planAnyKey]
	gtKey, ltKey := k.planNodeTimingKeys()
	gtSpec, hasGt := diff[gtKey]
	ltSpec, hasLt := diff[ltKey]
	collKey := k.collectionPropertyKey()
	collSpec, hasColl := diff[collKey]

	if !hasRet && !hasRaise && !hasPlan && !hasPlanAny && !hasGt && !hasLt && !hasColl {
		// No expectation declared for this branch — accept silently. The
		// branch's success/raise alone is the test surface; differential
		// constraints are opt-in per branch.
		return nil
	}

	if got.Raised {
		if !hasRaise {
			return []Reason{{
				Code: "unexpected_exception",
				Message: fmt.Sprintf("%s raised %s (%s) but spec required a return value",
					branch, got.Exception, got.Message),
			}}
		}
		wantTypes, wantPattern, err := exceptionFromSpec(raiseSpec)
		if err != nil {
			return []Reason{{Code: "malformed_raise_spec", Message: err.Error()}}
		}
		if len(wantTypes) > 0 {
			matched := false
			for _, t := range wantTypes {
				if t == got.Exception {
					matched = true
					break
				}
			}
			if !matched {
				return []Reason{{
					Code: "wrong_exception",
					Message: fmt.Sprintf("%s raised %s, spec required %s",
						branch, got.Exception, formatExpectedExceptions(wantTypes)),
				}}
			}
		}
		if wantPattern != "" {
			re, err := regexp.Compile(wantPattern)
			if err != nil {
				return []Reason{{
					Code:    "malformed_raise_spec",
					Message: fmt.Sprintf("message_pattern %q is not a valid regex: %v", wantPattern, err),
				}}
			}
			if !re.MatchString(got.Message) {
				return []Reason{{
					Code: "wrong_exception_message",
					Message: fmt.Sprintf("%s raised %s with message %q, spec required message matching /%s/",
						branch, got.Exception, got.Message, wantPattern),
				}}
			}
		}
		return nil
	}

	// Successful return — accumulate constraints from each declared spec.
	var reasons []Reason

	if hasRaise && !hasRet && !hasPlan && !hasPlanAny && !hasGt && !hasLt && !hasColl {
		// Branch was supposed to raise but returned, and no other constraints
		// to layer onto the success path.
		return []Reason{{
			Code: "unexpected_return",
			Message: fmt.Sprintf("%s returned %s value, spec required a raised exception",
				branch, got.TypeName),
		}}
	}

	if hasRet {
		reasons = append(reasons, matchReturnSpec(branch, got, retSpec)...)
	}
	if hasPlan || hasPlanAny {
		reasons = append(reasons, matchPlanNodeContains(branch, got, planSpec, hasPlan, planAnySpec, hasPlanAny)...)
	}
	reasons = append(reasons, matchPlanNodeTiming(branch, got, gtSpec, hasGt, ltSpec, hasLt)...)
	reasons = append(reasons, matchCollectionProperty(branch, got, collSpec, hasColl)...)

	return reasons
}

// matchReturnSpec compares ExecResult against a {type, value_equals} spec.
// The schema's open-ended `failed_branch_must_return: {}` allows other
// shapes; v0.1 supports {type, value_equals} and rejects others with a
// distinct reason so submitters know to wait for the next slice.
func matchReturnSpec(branch string, got runner.ExecResult, spec any) []Reason {
	sm, ok := spec.(map[string]any)
	if !ok {
		return []Reason{{
			Code: "unsupported_return_spec",
			Message: fmt.Sprintf("%s_branch_must_return: only {type, value_equals} shape is supported in v0.1, got %T",
				branch, spec),
		}}
	}

	var out []Reason

	if specType, ok := sm["type"].(string); ok && specType != got.TypeName {
		out = append(out, Reason{
			Code: "wrong_return_type",
			Message: fmt.Sprintf("%s returned type %s, spec required %s",
				branch, got.TypeName, specType),
		})
	}

	if val, hasVal := sm["value_equals"]; hasVal {
		if !got.Serializable {
			out = append(out, Reason{
				Code: "non_serializable_return",
				Message: fmt.Sprintf("%s returned non-serializable value (repr=%s); cannot compare to value_equals",
					branch, got.Repr),
			})
			return out
		}
		wantBytes, err := json.Marshal(val)
		if err != nil {
			out = append(out, Reason{
				Code:    "malformed_value_equals",
				Message: fmt.Sprintf("%s_branch_must_return.value_equals: %v", branch, err),
			})
			return out
		}
		gotCanonical, err := canonicalizeJSON(got.JSONValue)
		if err != nil {
			out = append(out, Reason{
				Code:    "branch_runner_error",
				Message: fmt.Sprintf("%s: canonicalize return value: %v", branch, err),
			})
			return out
		}
		if !bytes.Equal(wantBytes, gotCanonical) {
			out = append(out, Reason{
				Code: "wrong_return_value",
				Message: fmt.Sprintf("%s returned %s, spec required %s",
					branch, string(gotCanonical), string(wantBytes)),
			})
		}
	}

	if want, ok := lengthFromSpec(sm["length"]); ok {
		if got.Length == nil {
			out = append(out, Reason{
				Code: "non_sized_return",
				Message: fmt.Sprintf("%s returned %s value (no len()); cannot compare to length=%d",
					branch, got.TypeName, want),
			})
		} else if *got.Length != want {
			out = append(out, Reason{
				Code: "wrong_return_length",
				Message: fmt.Sprintf("%s returned length=%d, spec required length=%d",
					branch, *got.Length, want),
			})
		}
	}

	if elemType, ok := sm["contains_exception_type"].(string); ok {
		if len(got.ElementTypes) == 0 {
			out = append(out, Reason{
				Code: "non_iterable_return",
				Message: fmt.Sprintf("%s returned %s value (no element types captured); cannot check contains_exception_type=%s",
					branch, got.TypeName, elemType),
			})
		} else {
			found := false
			for _, t := range got.ElementTypes {
				if t == elemType {
					found = true
					break
				}
			}
			if !found {
				out = append(out, Reason{
					Code: "missing_exception_element",
					Message: fmt.Sprintf("%s returned elements %v, spec required at least one of type %s",
						branch, got.ElementTypes, elemType),
				})
			}
		}
	}

	return out
}

// matchPlanNodeContains evaluates the *_must_contain_plan_node and
// *_must_contain_plan_node_any differential keys. Both run as substring
// containment against got.Repr — for SubprocessDriver branches that's the
// raw stdout (e.g. psql EXPLAIN output), for PythonDriver it's repr($RESULT).
//
// Designed for canonical postgres seeds whose action runs `EXPLAIN (FORMAT…)`
// and asserts that the captured plan tree mentions specific node types
// (Seq Scan, Bitmap Index Scan, …); the substring shape sidesteps brittle
// byte-equality on plan output that varies by server version + statistics.
//
// hasKey / hasAnyKey are passed explicitly so the helper distinguishes
// "key absent" (no constraint) from "key present with nil value" (malformed).
func matchPlanNodeContains(branch string, got runner.ExecResult, scalarSpec any, hasScalar bool, anySpec any, hasAny bool) []Reason {
	var out []Reason

	if hasScalar {
		needle, ok := scalarSpec.(string)
		if !ok {
			out = append(out, Reason{
				Code:    "malformed_plan_node_spec",
				Message: fmt.Sprintf("%s_branch_must_contain_plan_node: expected string, got %T", branch, scalarSpec),
			})
		} else if needle == "" {
			out = append(out, Reason{
				Code:    "malformed_plan_node_spec",
				Message: fmt.Sprintf("%s_branch_must_contain_plan_node: empty string is not a valid substring", branch),
			})
		} else if !strings.Contains(got.Repr, needle) {
			out = append(out, Reason{
				Code: "differential_plan_node_mismatch",
				Message: fmt.Sprintf("%s output did not contain %q (got: %s)",
					branch, needle, truncateForReason(got.Repr)),
			})
		}
	}

	if hasAny {
		list, ok := anySpec.([]any)
		if !ok {
			out = append(out, Reason{
				Code:    "malformed_plan_node_spec",
				Message: fmt.Sprintf("%s_branch_must_contain_plan_node_any: expected list of strings, got %T", branch, anySpec),
			})
			return out
		}
		if len(list) == 0 {
			out = append(out, Reason{
				Code:    "malformed_plan_node_spec",
				Message: fmt.Sprintf("%s_branch_must_contain_plan_node_any: empty list is not a valid any-of constraint", branch),
			})
			return out
		}
		needles := make([]string, 0, len(list))
		for i, item := range list {
			s, sOK := item.(string)
			if !sOK {
				out = append(out, Reason{
					Code:    "malformed_plan_node_spec",
					Message: fmt.Sprintf("%s_branch_must_contain_plan_node_any[%d]: expected string, got %T", branch, i, item),
				})
				return out
			}
			if s == "" {
				out = append(out, Reason{
					Code:    "malformed_plan_node_spec",
					Message: fmt.Sprintf("%s_branch_must_contain_plan_node_any[%d]: empty string is not a valid substring", branch, i),
				})
				return out
			}
			needles = append(needles, s)
		}
		matched := false
		for _, n := range needles {
			if strings.Contains(got.Repr, n) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, Reason{
				Code: "differential_plan_node_mismatch",
				Message: fmt.Sprintf("%s output did not contain any of %v (got: %s)",
					branch, needles, truncateForReason(got.Repr)),
			})
		}
	}

	return out
}

// matchCollectionProperty evaluates the *_collection_has_duplicates
// differential keys. The captured stdout (got.Repr) is parsed as a
// newline-separated collection — each line is trimmed of whitespace, empty
// lines are dropped, and duplicates are detected by comparing list length
// to set size. The spec value (bool) declares which state the branch must
// be in.
//
// Designed for canonical seeds whose action prints a list of items and the
// claim is "this approach yields duplicates" vs "the working approach
// doesn't" (e.g. redis-scan-cursor-... where the failed approach can return
// the same key twice under concurrent mutation, the working approach
// dedups).
//
// hasKey is passed explicitly so the helper distinguishes "key absent"
// (no constraint) from "key present with nil value" (malformed).
func matchCollectionProperty(branch string, got runner.ExecResult, spec any, hasKey bool) []Reason {
	if !hasKey {
		return nil
	}
	want, ok := spec.(bool)
	if !ok {
		return []Reason{{
			Code:    "malformed_collection_property_spec",
			Message: fmt.Sprintf("%s_branch_collection_has_duplicates: expected bool, got %T", branch, spec),
		}}
	}

	items := parseCollectionLines(got.Repr)
	seen := make(map[string]struct{}, len(items))
	duplicates := false
	for _, item := range items {
		if _, ok := seen[item]; ok {
			duplicates = true
			break
		}
		seen[item] = struct{}{}
	}

	if duplicates == want {
		return nil
	}
	return []Reason{{
		Code: "differential_collection_property_mismatch",
		Message: fmt.Sprintf(
			"%s collection has_duplicates=%t (got %d lines), spec required has_duplicates=%t (output: %s)",
			branch, duplicates, len(items), want, truncateForReason(got.Repr),
		),
	}}
}

// parseCollectionLines splits captured stdout into a normalized line list:
// split on '\n', trim each line of whitespace, drop empty-after-trim lines.
// Used by matchCollectionProperty as the canonical "stdout as a collection"
// reading. Sufficient for redis-cli, ls, and any line-oriented tool whose
// output is one element per line.
func parseCollectionLines(repr string) []string {
	if repr == "" {
		return nil
	}
	raw := strings.Split(repr, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// matchPlanNodeTiming evaluates the *_branch_planning_time_seconds_gt and
// *_branch_planning_time_seconds_lt differential keys. Both keys are
// independent and may both apply to the same branch (additive — e.g. "gt 1
// AND lt 10" expresses a range constraint).
//
// Source: parses got.Repr as `EXPLAIN (FORMAT JSON)` output (top-level array
// → first object → "Planning Time" field, milliseconds), divides by 1000.0
// for seconds. Parse is lazy — only runs when at least one timing key is
// present. Lives entirely inside the helper so the runner stays driver-
// agnostic, mirroring matchPlanNodeContains' design.
//
// Reasons:
//   - differential_planning_time_mismatch — value is well-formed but fails
//     the gt/lt comparison.
//   - malformed_planning_time_spec — bad spec value (non-number, negative),
//     or got.Repr can't be parsed as EXPLAIN (FORMAT JSON), or the parsed
//     output doesn't carry a numeric "Planning Time" field.
func matchPlanNodeTiming(branch string, got runner.ExecResult, gtSpec any, hasGt bool, ltSpec any, hasLt bool) []Reason {
	if !hasGt && !hasLt {
		return nil
	}
	var out []Reason

	// Validate spec shapes before parsing got.Repr — surfaces malformed
	// specs even on no-output branches.
	var gtThreshold, ltThreshold float64
	if hasGt {
		v, ok := planningTimeSpecAsFloat(gtSpec)
		if !ok {
			out = append(out, Reason{
				Code:    "malformed_planning_time_spec",
				Message: fmt.Sprintf("%s_branch_planning_time_seconds_gt: expected number, got %T", branch, gtSpec),
			})
			hasGt = false
		} else if v < 0 {
			out = append(out, Reason{
				Code:    "malformed_planning_time_spec",
				Message: fmt.Sprintf("%s_branch_planning_time_seconds_gt: negative threshold %v is not valid", branch, v),
			})
			hasGt = false
		} else {
			gtThreshold = v
		}
	}
	if hasLt {
		v, ok := planningTimeSpecAsFloat(ltSpec)
		if !ok {
			out = append(out, Reason{
				Code:    "malformed_planning_time_spec",
				Message: fmt.Sprintf("%s_branch_planning_time_seconds_lt: expected number, got %T", branch, ltSpec),
			})
			hasLt = false
		} else if v < 0 {
			out = append(out, Reason{
				Code:    "malformed_planning_time_spec",
				Message: fmt.Sprintf("%s_branch_planning_time_seconds_lt: negative threshold %v is not valid", branch, v),
			})
			hasLt = false
		} else {
			ltThreshold = v
		}
	}
	if !hasGt && !hasLt {
		return out
	}

	return append(out, planningTimeGateReasons(
		branch, got, gtThreshold, hasGt, ltThreshold, hasLt,
		"malformed_planning_time_spec", "differential_planning_time_mismatch")...)
}

// planningTimeGateReasons is the shared parse-and-compare tail behind both
// the differential (matchPlanNodeTiming) and action-level
// (matchActionPlanNodeTiming) planning-time gates. Both forms parse
// got.Repr as EXPLAIN (FORMAT JSON) and emit the identical
// "not > / not < threshold" comparison; pre-R4 the parse-error block and
// the two comparison blocks were duplicated verbatim across the two
// functions, differing only in the Reason.Code strings. Callers retain
// their own threshold-resolution prologue (the differential form coerces
// from `any` specs, the action form reads typed float fields) and pass the
// already-resolved thresholds + active flags here.
//
// parseErrCode is the Reason.Code emitted when got.Repr is not parseable
// EXPLAIN JSON; mismatchCode is emitted when a well-formed planning time
// fails the gt/lt comparison. The gt/lt active flags must already account
// for negative-threshold filtering — this helper only parses and compares.
func planningTimeGateReasons(
	branch string, got runner.ExecResult,
	gtThreshold float64, gtActive bool, ltThreshold float64, ltActive bool,
	parseErrCode, mismatchCode string,
) []Reason {
	seconds, err := parsePlanningTimeSeconds(got.Repr)
	if err != nil {
		return []Reason{{
			Code:    parseErrCode,
			Message: fmt.Sprintf("%s output is not a parseable EXPLAIN (FORMAT JSON) document with a numeric \"Planning Time\" field: %v", branch, err),
		}}
	}

	var out []Reason
	if gtActive && !(seconds > gtThreshold) {
		out = append(out, Reason{
			Code:    mismatchCode,
			Message: fmt.Sprintf("%s planning time %.3fs is not > %.3fs", branch, seconds, gtThreshold),
		})
	}
	if ltActive && !(seconds < ltThreshold) {
		out = append(out, Reason{
			Code:    mismatchCode,
			Message: fmt.Sprintf("%s planning time %.3fs is not < %.3fs", branch, seconds, ltThreshold),
		})
	}
	return out
}

// planningTimeSpecAsFloat coerces a YAML-decoded numeric value to float64.
// Matches lengthFromSpec's defensive int/int64/float64 acceptance — yaml.v3
// decodes integer literals as int but JSON-decoded specs come through as
// float64, so accept both. Rejects non-numeric types.
func planningTimeSpecAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// parsePlanningTimeSeconds extracts the "Planning Time" field (milliseconds)
// from an `EXPLAIN (FORMAT JSON)` document and returns it in seconds. The
// document shape is `[{"Plan": ..., "Planning Time": <ms>, ...}]` — a
// single-element top-level array containing one object. Returns an error
// when repr is not valid JSON, not the expected array-of-object shape, or
// when "Planning Time" is missing or non-numeric.
func parsePlanningTimeSeconds(repr string) (float64, error) {
	if repr == "" {
		return 0, errors.New("output is empty")
	}
	var top []map[string]any
	if err := json.Unmarshal([]byte(repr), &top); err != nil {
		return 0, fmt.Errorf("not valid JSON: %w", err)
	}
	if len(top) == 0 {
		return 0, errors.New("top-level array is empty")
	}
	raw, ok := top[0]["Planning Time"]
	if !ok {
		return 0, errors.New("\"Planning Time\" field is absent")
	}
	ms, ok := raw.(float64)
	if !ok {
		return 0, fmt.Errorf("\"Planning Time\" is %T, expected number", raw)
	}
	return ms / 1000.0, nil
}

// matchActionPlanNodeTiming evaluates the per-branch
// assertion.planning_time_seconds_gt and assertion.planning_time_seconds_lt
// fields against the captured ExecResult. The action-level analogue of
// matchPlanNodeTiming — F79 added the differential (cross-branch) form;
// F80 adds the action-level (per-branch) form so the canonical
// postgres-update-returning-... seed's failed_approach.assertion and
// working_approach.assertion timing thresholds actually fire.
//
// Returns nil when:
//   - both timing fields are zero (no constraint declared — 0 is the
//     unset sentinel, mirroring TimeoutSeconds);
//   - a.Type != "plan_node" (defensive — only plan_node assertions can
//     carry timing thresholds; for other types, the fields are inert).
//
// Source: parses got.Repr as `EXPLAIN (FORMAT JSON)` output via the
// F79-shipped parsePlanningTimeSeconds helper. malformed JSON or absent
// "Planning Time" field surface as malformed_action_planning_time_spec
// (distinct reason code from the differential's malformed_planning_time_spec
// so callers can discriminate which gate fired).
//
// Reasons:
//   - action_planning_time_mismatch — well-formed but fails gt/lt.
//   - malformed_action_planning_time_spec — negative threshold, or
//     got.Repr not parseable as EXPLAIN (FORMAT JSON), or "Planning Time"
//     absent / non-numeric.
func matchActionPlanNodeTiming(branch string, a Assertion, got runner.ExecResult) []Reason {
	if a.Type != "plan_node" {
		return nil
	}
	if a.PlanningTimeSecondsGt == 0 && a.PlanningTimeSecondsLt == 0 {
		return nil
	}
	var out []Reason

	if a.PlanningTimeSecondsGt < 0 {
		out = append(out, Reason{
			Code:    "malformed_action_planning_time_spec",
			Message: fmt.Sprintf("%s assertion.planning_time_seconds_gt: negative threshold %v is not valid", branch, a.PlanningTimeSecondsGt),
		})
	}
	if a.PlanningTimeSecondsLt < 0 {
		out = append(out, Reason{
			Code:    "malformed_action_planning_time_spec",
			Message: fmt.Sprintf("%s assertion.planning_time_seconds_lt: negative threshold %v is not valid", branch, a.PlanningTimeSecondsLt),
		})
	}
	// Re-derive after the negative-check filter — only well-formed
	// thresholds drive the comparison.
	gtActive := a.PlanningTimeSecondsGt > 0
	ltActive := a.PlanningTimeSecondsLt > 0
	if !gtActive && !ltActive {
		return out
	}

	return append(out, planningTimeGateReasons(
		branch, got, a.PlanningTimeSecondsGt, gtActive, a.PlanningTimeSecondsLt, ltActive,
		"malformed_action_planning_time_spec", "action_planning_time_mismatch")...)
}

// matchActionOutputPattern evaluates the per-branch
// assertion.pattern_absent and assertion.pattern_present fields against the
// captured ExecResult's stdout (got.Repr). The action-level analogue of
// matchPlanNodeContains — F86 adds typed regex assertions on captured output
// so seeds like docker-buildkit-copy-link-cache-... can claim
// "the failed approach's stdout MUST NOT match /CACHED \[.*COPY/" while the
// working approach asserts the same pattern MUST be present.
//
// Returns nil when:
//   - a.Type != "output_match" (defensive — only output_match assertions
//     carry pattern fields; for other types, the fields are inert);
//   - both pattern fields are empty strings (no constraint declared — empty
//     string is the unset sentinel, mirroring the F80 timing-fields zero
//     convention).
//
// Source: matches against got.Repr — for SubprocessDriver branches that's
// the raw stdout (e.g. `docker build` output), for PythonDriver it's
// repr($RESULT). Patterns are unanchored (regexp.MatchString), matching the
// "found anywhere in the output" reading documented in seed examples.
//
// Both pattern fields gate independently — an assertion may declare ABSENT
// AND PRESENT patterns and both will fire. A malformed pattern in one field
// does not suppress evaluation of the other (each compile failure surfaces
// its own malformed reason).
//
// Reasons:
//   - pattern_unexpectedly_present — pattern_absent's regex matched got.Repr.
//   - pattern_unexpectedly_absent — pattern_present's regex did not match.
//   - pattern_compile_failed — one of the pattern fields is not a valid
//     Go regexp; the message names which field (pattern_absent or
//     pattern_present) and includes the regexp library's compile error.
func matchActionOutputPattern(branch string, a Assertion, got runner.ExecResult) []Reason {
	if a.Type != "output_match" {
		return nil
	}
	if a.PatternAbsent == "" && a.PatternPresent == "" {
		return nil
	}
	var out []Reason

	if a.PatternAbsent != "" {
		re, err := regexp.Compile(a.PatternAbsent)
		if err != nil {
			out = append(out, Reason{
				Code: "pattern_compile_failed",
				Message: fmt.Sprintf("%s assertion.pattern_absent %q failed to compile: %v",
					branch, a.PatternAbsent, err),
			})
		} else if re.MatchString(got.Repr) {
			out = append(out, Reason{
				Code: "pattern_unexpectedly_present",
				Message: fmt.Sprintf("%s output matched pattern_absent /%s/ but spec required absence (got: %s)",
					branch, a.PatternAbsent, truncateForReason(got.Repr)),
			})
		}
	}

	if a.PatternPresent != "" {
		re, err := regexp.Compile(a.PatternPresent)
		if err != nil {
			out = append(out, Reason{
				Code: "pattern_compile_failed",
				Message: fmt.Sprintf("%s assertion.pattern_present %q failed to compile: %v",
					branch, a.PatternPresent, err),
			})
		} else if !re.MatchString(got.Repr) {
			out = append(out, Reason{
				Code: "pattern_unexpectedly_absent",
				Message: fmt.Sprintf("%s output did not match pattern_present /%s/ but spec required presence (got: %s)",
					branch, a.PatternPresent, truncateForReason(got.Repr)),
			})
		}
	}

	return out
}

// truncateForReason caps long output strings so Reason.Message stays readable
// in CLI/JSON dumps. EXPLAIN output runs to several KB; the first ~200 chars
// is enough to diagnose most mismatches without flooding the report.
func truncateForReason(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// lengthFromSpec extracts an integer length from a YAML-decoded value. yaml.v3
// decodes plain integers as int (Go), but as the exact YAML int kind. Accept
// both int and float64 (defensive — JSON-decoded values come through as float64).
func lengthFromSpec(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// Reject non-integer floats.
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// canonicalizeJSON re-marshals raw through Go's encoder so that comparisons
// don't fail on whitespace differences between Python's json.dumps and
// Go's encoding/json (e.g., Python emits "[1, 2]", Go emits "[1,2]").
func canonicalizeJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// exceptionFromSpec extracts the expected exception class names and optional
// message-pattern regex from a *_must_raise spec. Accepts:
//   - nil → no constraints
//   - bare string → single class name, no message pattern
//   - {type|class|exception: <name>, message_pattern: <regex>} → single class + pattern
//   - {exception_any: [<name>, ...], message_pattern: <regex>} → any-of + pattern
//
// Returns (nil, "", nil) when the spec is nil. Returns (typeNames, pattern, nil)
// otherwise. Empty typeNames means "any exception class is acceptable" — the
// pattern alone constrains.
func exceptionFromSpec(spec any) (typeNames []string, messagePattern string, err error) {
	switch v := spec.(type) {
	case nil:
		return nil, "", nil
	case string:
		return []string{v}, "", nil
	case map[string]any:
		// exception_any takes precedence over single-name keys.
		if anyVal, ok := v["exception_any"]; ok {
			list, listOK := anyVal.([]any)
			if !listOK {
				return nil, "", fmt.Errorf("exception_any must be a list of class names")
			}
			for _, item := range list {
				s, sOK := item.(string)
				if !sOK {
					return nil, "", fmt.Errorf("exception_any entry %v is not a string", item)
				}
				typeNames = append(typeNames, s)
			}
		} else {
			// Single-name keys, in priority order: exception, type, class.
			for _, key := range []string{"exception", "type", "class"} {
				if s, ok := v[key].(string); ok {
					typeNames = []string{s}
					break
				}
			}
		}
		if mp, ok := v["message_pattern"].(string); ok {
			messagePattern = mp
		}
		if len(typeNames) == 0 && messagePattern == "" {
			return nil, "", errors.New("raise spec object missing exception/type/class/exception_any field")
		}
		return typeNames, messagePattern, nil
	default:
		return nil, "", fmt.Errorf("raise spec has unsupported shape %T", spec)
	}
}

// formatExpectedExceptions renders one or more expected exception class names
// for the Reason message — single name as `Foo`, multiple as `any of [A, B]`.
func formatExpectedExceptions(types []string) string {
	if len(types) == 1 {
		return types[0]
	}
	return fmt.Sprintf("any of %v", types)
}
