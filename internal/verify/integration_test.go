package verify

import (
	"os"
	"strings"
	"testing"
)

// integrationGreenYAML is a minimal integration-tier entry that runs a single
// HTTP request through the cassette stub. Both branches issue one POST to
// /v1/echo and receive a canned response with a status code that distinguishes
// failure from success. Tests below substitute the cassette response status
// (and other fields) to exercise the integration runner's branches.
const integrationGreenYAML = `
unit_id: integration-runner-greenpath
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: posts once and returns the status code as $RESULT.status — failure path
  setup: []
  action:
    - type: code
      lang: python
      body: |
        import urllib.request, urllib.error
        try:
            with urllib.request.urlopen(f"{$ENDPOINT}/v1/echo", data=b"{}") as resp:
                $RESULT = {"status": resp.status}
        except urllib.error.HTTPError as e:
            $RESULT = {"status": e.code}
  assertion: { type: returns, expect: fail }
working_approach:
  description: posts once and returns the status code as $RESULT.status — success path
  setup: []
  action:
    - type: code
      lang: python
      body: |
        import urllib.request, urllib.error
        try:
            with urllib.request.urlopen(f"{$ENDPOINT}/v1/echo", data=b"{}") as resp:
                $RESULT = {"status": resp.status}
        except urllib.error.HTTPError as e:
            $RESULT = {"status": e.code}
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: http_client
  cassette:
    mode: replay
    artifact: green.cassette.yaml
    steps:
      echo-fail:
        request: "POST /v1/echo\n\n"
        response: "503 Service Unavailable\n\n"
      echo-ok:
        request: "POST /v1/echo\n\n"
        response: "200 OK\n\n"
    captures: [test capture]
    strips: [test strip]
    replay_targets: [test target]
  differential:
    failed_approach_replay_sequence: [echo-fail]
    working_approach_replay_sequence: [echo-ok]
    failed_branch_must_return: { type: dict, value_equals: { status: 503 } }
    working_branch_must_return: { type: dict, value_equals: { status: 200 } }
  mutations:
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 99
      branch: failed_approach
      expected_result: unchanged
    - strategy: swap_function_call
      target: working_approach.action
      token: urlopen
      new_value: not_a_real_function
      branch: working_approach
      expected_result: fail
  timeout_seconds: 10
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`

func TestRunIntegrationVerified(t *testing.T) {
	skipIfNoPython3(t)
	res, err := Run([]byte(integrationGreenYAML))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "integration" {
		t.Fatalf("tier=%q, want integration", res.Tier)
	}
}

func TestRunIntegrationCassettePOC(t *testing.T) {
	skipIfNoPython3(t)
	data, err := os.ReadFile("testdata/integration-shape-poc.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	res, err := Run(data)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "integration" {
		t.Fatalf("tier=%q, want integration", res.Tier)
	}
	if res.UnitID != "integration-shape-poc-http-retry-on-502" {
		t.Fatalf("unit_id=%q", res.UnitID)
	}
}

func TestRunIntegrationUnmatchedRequest(t *testing.T) {
	skipIfNoPython3(t)
	// Action posts to /v1/echo but cassette declares /v1/different. The stub
	// responds 599; the verifier surfaces cassette_unmatched_request.
	yaml := strings.Replace(
		integrationGreenYAML,
		`request: "POST /v1/echo\n\n"
        response: "503 Service Unavailable\n\n"`,
		`request: "POST /v1/different\n\n"
        response: "503 Service Unavailable\n\n"`,
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "cassette_unmatched_request") {
		t.Fatalf("expected cassette_unmatched_request, got %v", res.Reasons)
	}
}

func TestRunIntegrationCassetteUnused(t *testing.T) {
	skipIfNoPython3(t)
	// Add an unused cassette step (echo-ghost) that no replay_sequence
	// references. The verifier rejects with cassette_unused.
	yaml := strings.Replace(
		integrationGreenYAML,
		`      echo-ok:
        request: "POST /v1/echo\n\n"
        response: "200 OK\n\n"`,
		`      echo-ok:
        request: "POST /v1/echo\n\n"
        response: "200 OK\n\n"
      echo-ghost:
        request: "GET /never\n\n"
        response: "200 OK\n\n"`,
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "cassette_unused") {
		t.Fatalf("expected cassette_unused, got %v", res.Reasons)
	}
}

func TestRunIntegrationUnknownStepName(t *testing.T) {
	// A replay_sequence references a step name that isn't in cassette.steps.
	// Surfaces cassette_step_unknown without needing python3 — the check
	// runs before any subprocess.
	yaml := strings.Replace(
		integrationGreenYAML,
		"failed_approach_replay_sequence: [echo-fail]",
		"failed_approach_replay_sequence: [echo-typo]",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "cassette_step_unknown") {
		t.Fatalf("expected cassette_step_unknown, got %v", res.Reasons)
	}
}

func TestRunIntegrationMissingReplaySequence(t *testing.T) {
	// Strip both replay_sequence keys → missing_replay_sequence. Runs without
	// python3 (the check is pre-runner).
	yaml := strings.Replace(
		integrationGreenYAML,
		"failed_approach_replay_sequence: [echo-fail]\n    working_approach_replay_sequence: [echo-ok]\n    ",
		"",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "missing_replay_sequence") {
		t.Fatalf("expected missing_replay_sequence, got %v", res.Reasons)
	}
}

func TestRunIntegrationCassetteMalformed(t *testing.T) {
	// Replace the request line with an empty string → cassette_malformed.
	// Runs without python3 (the check is pre-runner).
	yaml := strings.Replace(
		integrationGreenYAML,
		`request: "POST /v1/echo\n\n"
        response: "503 Service Unavailable\n\n"`,
		`request: ""
        response: "503\n\n"`,
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "cassette_malformed") {
		t.Fatalf("expected cassette_malformed, got %v", res.Reasons)
	}
}

func TestRunIntegrationReexecuteIsolationMismatch(t *testing.T) {
	// integrationGreenYAML uses isolation: http_client; flipping the cassette
	// mode to reexecute now routes through runReexecute, which only
	// dispatches under {subprocess, database}. The entry is well-formed but
	// its isolation isn't a reexecute-mode isolation — surface as
	// isolation_not_yet_implemented (the precise diagnostic), not the old
	// cassette_mode_not_yet_implemented blanket.
	//
	// A runtime block is added because the schema's allOf gate (and our CLI
	// fallback in runReexecute) requires it for mode=reexecute.
	yaml := strings.Replace(
		integrationGreenYAML,
		"mode: replay",
		"mode: reexecute\n    runtime:\n      tool: shell",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "isolation_not_yet_implemented") {
		t.Fatalf("expected isolation_not_yet_implemented, got %v", res.Reasons)
	}
}

func TestRunIntegrationSequenceUnderrun(t *testing.T) {
	skipIfNoPython3(t)
	// working_approach_replay_sequence declares two steps but the action
	// only fires one request → cassette_sequence_underrun.
	yaml := strings.Replace(
		integrationGreenYAML,
		"working_approach_replay_sequence: [echo-ok]",
		"working_approach_replay_sequence: [echo-ok, echo-fail]",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "cassette_sequence_underrun") {
		t.Fatalf("expected cassette_sequence_underrun, got %v", res.Reasons)
	}
}

// ─── F22: cassette-response mutation strategy ────────────────────────────────

// cassetteResponseBaseYAML is the integration-tier scaffold the cassette-
// response mutation tests interpolate over. Both branches issue one GET to
// /v1/widget. The action returns {status, body, retry_after} so all three
// perturbation surfaces (status, body, header.<NAME>) are observable. The
// MUTATIONS slot is replaced per test.
const cassetteResponseBaseYAML = `
unit_id: integration-cassette-response-mutation
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: GETs /v1/widget once and returns status, body, retry_after — failure path
  setup: []
  action:
    - type: code
      lang: python
      body: |
        import urllib.request, urllib.error
        try:
            with urllib.request.urlopen(f"{$ENDPOINT}/v1/widget") as resp:
                $RESULT = {"status": resp.status, "body": resp.read().decode(), "retry_after": resp.headers.get("Retry-After", "")}
        except urllib.error.HTTPError as e:
            $RESULT = {"status": e.code, "body": e.read().decode() if hasattr(e, "read") else "", "retry_after": e.headers.get("Retry-After", "") if e.headers else ""}
  assertion: { type: returns, expect: fail }
working_approach:
  description: GETs /v1/widget once and returns status, body, retry_after — success path
  setup: []
  action:
    - type: code
      lang: python
      body: |
        import urllib.request, urllib.error
        try:
            with urllib.request.urlopen(f"{$ENDPOINT}/v1/widget") as resp:
                $RESULT = {"status": resp.status, "body": resp.read().decode(), "retry_after": resp.headers.get("Retry-After", "")}
        except urllib.error.HTTPError as e:
            $RESULT = {"status": e.code, "body": e.read().decode() if hasattr(e, "read") else "", "retry_after": e.headers.get("Retry-After", "") if e.headers else ""}
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: http_client
  cassette:
    mode: replay
    artifact: cassette-response-mutation.cassette.yaml
    steps:
      widget-fail:
        request: "GET /v1/widget\n\n"
        response: "503 Service Unavailable\nRetry-After: 1\n\nupstream down"
      widget-ok:
        request: "GET /v1/widget\n\n"
        response: "200 OK\nRetry-After: 0\n\nok"
    captures: [test capture]
    strips: [test strip]
    replay_targets: [test target]
  differential:
    failed_approach_replay_sequence: [widget-fail]
    working_approach_replay_sequence: [widget-ok]
    failed_branch_must_return: { type: dict, value_equals: { status: 503, body: "upstream down", retry_after: "1" } }
    working_branch_must_return: { type: dict, value_equals: { status: 200, body: "ok", retry_after: "0" } }
  mutations: __MUTATIONS__
  timeout_seconds: 10
`

func buildCassetteResponseYAML(mutations string) string {
	return strings.Replace(cassetteResponseBaseYAML, "__MUTATIONS__", mutations, 1)
}

func TestMutationCassetteResponseBodyExpectedFail(t *testing.T) {
	skipIfNoPython3(t)
	// Perturbing the working-branch body from "ok" to "TAMPERED" must break
	// the value_equals body assertion → outcomeFail. Plus a sentinel
	// mutate_fixture (unchanged) to satisfy the schema's two-mutation min.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      action: body
      new_value: "TAMPERED"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationCassetteResponseStatusExpectedFail(t *testing.T) {
	skipIfNoPython3(t)
	// Perturbing the working-branch status from 200 to 500 must break the
	// status: 200 assertion → outcomeFail.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      action: status
      new_value: 500
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationCassetteResponseHeaderExpectedFail(t *testing.T) {
	skipIfNoPython3(t)
	// Perturbing the Retry-After header from "0" to "999" must break the
	// retry_after: "0" assertion → outcomeFail.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      action: header.Retry-After
      new_value: "999"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationCassetteResponseUnknownStepID(t *testing.T) {
	skipIfNoPython3(t)
	// target references a step name that doesn't exist → mutation_step_id_unknown.
	mutations := `
    - strategy: mutate_cassette_response
      target: nonexistent-step
      action: body
      new_value: "TAMPERED"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_step_id_unknown") {
		t.Fatalf("expected mutation_step_id_unknown, got %v", res.Reasons)
	}
}

func TestMutationCassetteResponseStepNotInBranchSequence(t *testing.T) {
	skipIfNoPython3(t)
	// target names a real step but the targeted branch's sequence doesn't
	// consume it → still mutation_step_id_unknown (unobservable perturbation).
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-fail
      action: body
      new_value: "TAMPERED"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_step_id_unknown") {
		t.Fatalf("expected mutation_step_id_unknown, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_step_id_unknown" {
			msg = r.Message
			break
		}
	}
	if !strings.Contains(msg, "unobservable") {
		t.Errorf("message %q missing 'unobservable' hint", msg)
	}
}

func TestMutationCassetteResponseInvalidField(t *testing.T) {
	skipIfNoPython3(t)
	// action: nonsense → mutation_field_invalid.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      action: nonsense
      new_value: "x"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_field_invalid") {
		t.Fatalf("expected mutation_field_invalid, got %v", res.Reasons)
	}
}

func TestMutationCassetteResponseMissingField(t *testing.T) {
	skipIfNoPython3(t)
	// no action: at all → mutation_field_invalid.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      new_value: "x"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_field_invalid") {
		t.Fatalf("expected mutation_field_invalid, got %v", res.Reasons)
	}
}

func TestMutationCassetteResponseNonDiscriminatingHint(t *testing.T) {
	skipIfNoPython3(t)
	// Working spec accepts any dict (no value_equals on body / status / header).
	// Perturbing body from "ok" to "ok-but-different" still satisfies the spec
	// AND the action's $RESULT now differs from baseline → outcomePass, not
	// outcomeUnchanged. To force outcomeUnchanged we need to perturb in a way
	// the baseline already absorbs. Easiest: re-set the same value as before.
	// new_value: "ok" matches the existing body → unchanged → expected fail
	// → triggers mutation_did_not_discriminate.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      action: body
      new_value: "ok"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
`
	yml := buildCassetteResponseYAML(mutations) + `
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel literal not referenced by either branch
    category: public_constant
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_did_not_discriminate") {
		t.Fatalf("expected mutation_did_not_discriminate, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_did_not_discriminate" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{"perturbed cassette response", "widget-ok", "body"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestMutationCassetteResponseUnsupportedAtUnitTier(t *testing.T) {
	skipIfNoPython3(t)
	// mutate_cassette_response only makes sense at integration tier. A unit-
	// tier entry that declares this strategy must surface tier_unsupported
	// with mutation_strategy_unsupported (the strategies registry rejects it).
	yaml := `
unit_id: unit-mutation-cassette-response-rejected
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns 1
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 1 }
  mutations:
    - strategy: mutate_cassette_response
      target: irrelevant-step
      action: body
      new_value: "x"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_NOOP
      new_value: 1
      branch: working_approach
      expected_result: unchanged
  timeout_seconds: 5
literals:
  $LITERAL_NOOP:
    value: 1
    reason: sentinel
    category: public_constant
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_strategy_unsupported") {
		t.Fatalf("expected mutation_strategy_unsupported at unit tier, got %v", res.Reasons)
	}
}

func TestMutationCassetteResponseClonePreservesBaseline(t *testing.T) {
	skipIfNoPython3(t)
	// Two cassette-response mutations on the same step with different
	// perturbations must both fire correctly — proves cassette cloning
	// isolates each mutation from the others. If the clone leaked, the
	// second mutation would observe the first's perturbed state.
	mutations := `
    - strategy: mutate_cassette_response
      target: widget-ok
      action: body
      new_value: "first-tampered"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_cassette_response
      target: widget-ok
      action: status
      new_value: 418
      branch: working_approach
      expected_result: fail
`
	yml := buildCassetteResponseYAML(mutations)
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}
