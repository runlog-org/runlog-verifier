package verify

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Result is the outcome of running the verifier against an entry.
type Result struct {
	UnitID  string   `json:"unit_id"`
	Tier    string   `json:"tier"`    // verification.type from the entry
	Status  string   `json:"status"`  // "verified" | "rejected" | "tier_unsupported"
	Reasons []Reason `json:"reasons"` // empty when Status == "verified"
}

// MaxEntryBytes caps the size of an entry YAML file the verifier will
// decode. The schema does not bound entry size directly; without a cap,
// a hostile or malformed entry could exhaust memory during yaml.Unmarshal.
// 1 MiB is well above any realistic hand-authored entry.
const MaxEntryBytes = 1 << 20

// ErrEntryEmpty is returned when the YAML decodes but yields a zero entry.
var ErrEntryEmpty = errors.New("entry is empty after YAML decode")

// ErrEntryTooLarge is returned when the entry payload exceeds MaxEntryBytes.
var ErrEntryTooLarge = fmt.Errorf("entry exceeds %d bytes", MaxEntryBytes)

// Run decodes data as a runlog entry and runs the v0.1 Phase 2 checks.
//
// Returns a Result describing which tier the entry declares and whether
// it was accepted. The error return is reserved for *programming* failures
// (YAML parse errors, unrecoverable I/O); rejected entries return a Result
// with Status == "rejected" and a populated Reasons slice — they are not
// errors.
func Run(data []byte) (Result, error) {
	if len(data) > MaxEntryBytes {
		return Result{}, ErrEntryTooLarge
	}
	var e Entry
	if err := yaml.Unmarshal(data, &e); err != nil {
		return Result{}, fmt.Errorf("yaml unmarshal: %w", err)
	}
	if e.UnitID == "" {
		return Result{}, ErrEntryEmpty
	}

	tier := e.Verification.Type
	if tier == "" {
		return Result{
			UnitID: e.UnitID,
			Status: "rejected",
			Reasons: []Reason{{
				Code:    "missing_verification_type",
				Message: "verification.type is required",
			}},
		}, nil
	}

	res := Result{UnitID: e.UnitID, Tier: tier}

	switch tier {
	case "assertion_only":
		return runAssertionOnly(&e), nil

	case "unit":
		return runUnit(&e), nil

	case "integration":
		return runIntegration(&e), nil

	default:
		// Unknown tier — accepted as well-formed YAML but not executable.
		return tierUnsupported(res, "tier_not_yet_implemented", fmt.Sprintf(
			"verification tier %q is not implemented in this verifier build",
			tier,
		)), nil
	}
}

// runAssertionOnly runs all assertion_only checks and returns the Result.
// Mirrors the runUnit pattern for symmetry: one call site in Run, all check
// logic contained here.
func runAssertionOnly(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "assertion_only"}

	var reasons []Reason
	reasons = append(reasons, checkBranchesPresent(e)...)
	reasons = append(reasons, checkBranchesDiscriminating(e)...)
	reasons = append(reasons, checkAssertionOnlyShape(e)...)
	reasons = append(reasons, checkMutationStructure(e)...)
	reasons = append(reasons, checkMutationDiscriminating(e)...)
	reasons = append(reasons, checkPrimitivesRegistered(e)...)

	if len(reasons) == 0 {
		res.Status = "verified"
		return res
	}
	return rejectedReasons(res, reasons)
}
