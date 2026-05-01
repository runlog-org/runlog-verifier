// Package verify implements declarative verification of runlog entries.
//
// For the v0.1 Phase 2 starter, only the `assertion_only` tier is supported.
// Entries of type `unit` and `integration` are rejected with a
// not-yet-supported reason — those tiers require subprocess execution and
// cassette replay respectively, which land in follow-up commits.
package verify

// Entry is the partial decoding of a runlog YAML used by the verifier.
// It captures only the fields the checks need; the surrounding YAML may
// contain other top-level keys we ignore.
type Entry struct {
	UnitID          string         `yaml:"unit_id"`
	Domain          []string       `yaml:"domain"`
	FailedApproach  Branch         `yaml:"failed_approach"`
	WorkingApproach Branch         `yaml:"working_approach"`
	Verification    Verification   `yaml:"verification"`
	Literals        map[string]any `yaml:"literals"`
}

// Branch is one of failed_approach / working_approach. The verifier reads
// only the assertion block plus the loose action/setup payloads (kept as
// any so the schema-side variability does not propagate into Go types).
type Branch struct {
	Description string    `yaml:"description"`
	Setup       any       `yaml:"setup"`
	Action      any       `yaml:"action"`
	Assertion   Assertion `yaml:"assertion"`
}

// Assertion is the per-branch claim. The verifier reads enough of it to
// compare the two branches' assertions against each other and detect the
// tautological-test case.
type Assertion struct {
	Type      string `yaml:"type"`
	Expect    string `yaml:"expect"`
	Path      string `yaml:"path"`
	Value     any    `yaml:"value"`
	Exception string `yaml:"exception"`
}

// Verification mirrors the schema's verification block.
type Verification struct {
	Type                string         `yaml:"type"`
	Isolation           string         `yaml:"isolation"`
	PrimitivesRequired  []string       `yaml:"primitives_required"`
	Differential        map[string]any `yaml:"differential"`
	Mutations           []Mutation     `yaml:"mutations"`
	TimeoutSeconds      float64        `yaml:"timeout_seconds"`
	Cassette            map[string]any `yaml:"cassette"`
}

// Mutation mirrors the per-mutation entry. Both expected_result and
// expected_branch_outcome are read as raw maps so the type-discriminated
// schema rule can be evaluated without forcing a single shape.
type Mutation struct {
	Strategy               string            `yaml:"strategy"`
	Target                 string            `yaml:"target"`
	NewValue               any               `yaml:"new_value"`
	Token                  string            `yaml:"token"`
	Field                  string            `yaml:"action,omitempty"`
	Branch                 string            `yaml:"branch"`
	ExpectedResult         string            `yaml:"expected_result"`
	ExpectedBranchOutcome  map[string]string `yaml:"expected_branch_outcome"`
}
