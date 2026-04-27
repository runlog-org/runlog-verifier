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

func TestRunIntegrationReexecuteUnsupported(t *testing.T) {
	yaml := strings.Replace(
		integrationGreenYAML,
		"mode: replay",
		"mode: reexecute",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "cassette_mode_not_yet_implemented") {
		t.Fatalf("expected cassette_mode_not_yet_implemented, got %v", res.Reasons)
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
