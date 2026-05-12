// Package verify share-state safety predicate.
//
// IsShareStateSafe is the F91 static-analysis complement to F87's explicit
// cassette.runtime.share_state_across_mutations opt-in flag. The verifier
// auto-enables share-state for docker reexecute cassettes whose structure
// proves safe; the predicate is what "structurally safe" means.

package verify

import (
	"strings"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
)

// IsShareStateSafe reports whether a reexecute-mode cassette can safely
// share runtime state across mutation re-runs without leaking state
// from one mutation into the next.
//
// The v0.1 rule is conservative:
//
//   - If cassette.setup_script is empty, sharing is trivially safe (no
//     setup state to leak).
//   - Otherwise, sharing is safe only when EVERY mutation is a
//     source-mutation on action steps: Strategy != "mutate_fixture"
//     (which could touch setup-consumed inputs via input-rebind or
//     fixture-directory mutation) AND Target does not contain ".setup."
//     (per B20's setup-scope widening, sourceRemoveStrategy can route
//     mutations at setup steps directly).
//
// Authors who set the explicit flag always win at the runReexecute
// call site; this predicate only fires when the field is absent and
// the caller is choosing whether to auto-promote.
//
// Tool scope. Auto-promotion stays docker-only by design — F93 widened
// the explicit-opt-in gate to permit postgres/redis but did NOT extend
// auto-promotion. The predicate analyzes mutation-strategy and target
// shape; it can't statically reason about database row-state leakage
// from setup_script side effects, so postgres/redis seeds must opt in
// explicitly via share_state_across_mutations: true and shoulder the
// row-leak responsibility (TRUNCATE / FLUSHDB in setup_script).
//
// F89's future fixture-mutation strategies may need to extend the
// unsafe-marker set here; the predicate currently keys on the
// "mutate_fixture" strategy name.
func IsShareStateSafe(cas *cassette.Cassette, mutations []Mutation) bool {
	if cas == nil {
		return false
	}
	if len(cas.SetupScript) == 0 {
		return true
	}
	for _, m := range mutations {
		if m.Strategy == "mutate_fixture" {
			return false
		}
		if strings.Contains(m.Target, ".setup.") {
			return false
		}
	}
	return true
}
