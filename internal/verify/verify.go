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
		// F36: universal shape runs SHAPE-FIRST — before runUnit's
		// isolation/driver determination — so an under-shaped entry is
		// rejected even when this build can't execute its isolation. An
		// entry that can never be accepted on shape grounds must not get
		// the "come back later" tier_unsupported signal, and this mirrors
		// the server, which global-enforces submission rules §1-3 at
		// runlog_submit regardless of executability.
		if rs := checkUniversalShape(&e); len(rs) > 0 {
			return rejectedReasons(res, rs), nil
		}
		return runUnit(&e), nil

	case "integration":
		// Shape-first (see the unit arm). checkUniversalShape here also
		// gates the reexecute path: runReexecute is dispatched from inside
		// runIntegration, so this pre-flight covers replay-integration and
		// reexecute alike.
		if rs := checkUniversalShape(&e); len(rs) > 0 {
			return rejectedReasons(res, rs), nil
		}
		return runIntegration(&e), nil

	default:
		// Unknown tier — accepted as well-formed YAML but not executable.
		return tierUnsupported(res, "tier_not_yet_implemented", fmt.Sprintf(
			"verification tier %q is not implemented in this verifier build",
			tier,
		)), nil
	}
}

// checkUniversalShape runs the falsifiability / branch-shape checks that
// apply to *every* tier, not just assertion_only.
//
// Before F36 these five checks ran only inside runAssertionOnly, so a
// unit / integration / reexecute entry with tautological branches or zero
// falsifiability mutations verified green at the verifier — a submitter
// who opted into a richer tier paradoxically got fewer shape checks.
// End-to-end this was not a hole (the server global-enforces submission
// rules §1-3 at runlog_submit, see server/.../sanitize/submit_rules.py),
// but it meant the local verifier and the server disagreed: a bundle the
// verifier signed green could still be rejected at submit. Run() now
// applies this as a SHAPE-FIRST pre-flight for unit and integration
// (integration covers reexecute, dispatched from inside runIntegration),
// giving the submitter the failure locally instead of after a signed
// round-trip.
//
// This is the single source of truth for "what is universal" so the
// assertion_only path and the richer tiers cannot drift apart again —
// duplicate definitions are exactly what caused the original gap.
//
// The schema labels submission rules §1-3 "global" (no tier qualifier;
// §8 is the only tier-scoped rule), so all five belong here.
// checkPrimitivesRegistered flags only *unregistered* primitives, so the
// empty primitives_required common on unit / integration produces no
// reason; the non-empty requirement is assertion_only-specific and stays
// in checkAssertionOnlyShape.
//
// Enforcement is unconditional: no grandfather list. An existing entry
// that was green only because its tier skipped these checks is now
// rejected on re-verify — the intended correction, and consistent with
// what runlog_submit already does.
func checkUniversalShape(e *Entry) []Reason {
	var reasons []Reason
	reasons = append(reasons, checkBranchesPresent(e)...)
	reasons = append(reasons, checkBranchesDiscriminating(e)...)
	reasons = append(reasons, checkMutationStructure(e)...)
	reasons = append(reasons, checkMutationDiscriminating(e)...)
	reasons = append(reasons, checkPrimitivesRegistered(e)...)
	return reasons
}

// runAssertionOnly runs all assertion_only checks and returns the Result.
// Mirrors the runUnit pattern for symmetry: one call site in Run, all check
// logic contained here. The universal shape checks come from the shared
// checkUniversalShape; checkAssertionOnlyShape adds the tier-specific
// constraints (non-empty primitives_required, no cassette, timeout range).
func runAssertionOnly(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "assertion_only"}

	reasons := checkUniversalShape(e)
	reasons = append(reasons, checkAssertionOnlyShape(e)...)

	if len(reasons) == 0 {
		res.Status = "verified"
		return res
	}
	return rejectedReasons(res, reasons)
}
